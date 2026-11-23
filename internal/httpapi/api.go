// Package httpapi 提供 StormOps 的 HTTP 接口与静态控制台。
//
// 路由分三类：
//   - /api/v1/*  控制台使用，返回 JSON
//   - /internal/* 只给本机 Nginx / agent 调用，不对外暴露
//   - /metrics   给 vmagent 抓取
package httpapi

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Ricevire/stormops/internal/agent"
	"github.com/Ricevire/stormops/internal/capacity"
	"github.com/Ricevire/stormops/internal/config"
	"github.com/Ricevire/stormops/internal/deploy"
	"github.com/Ricevire/stormops/internal/gateway"
	"github.com/Ricevire/stormops/internal/model"
	"github.com/Ricevire/stormops/internal/monitor"
	"github.com/Ricevire/stormops/internal/store"
	"github.com/Ricevire/stormops/internal/waf"
	"github.com/Ricevire/stormops/web"
)

// Version 由 -ldflags 注入。
var Version = "dev"

// Server 是 HTTP 服务。
type Server struct {
	cfg     *config.Config
	st      *store.Store
	dep     *deploy.Orchestrator
	started time.Time
	host    string

	wafChecks   int64
	wafBlocks   int64
	ingestCount int64
}

// New 创建服务。
func New(cfg *config.Config, st *store.Store, dep *deploy.Orchestrator) *Server {
	h, _ := os.Hostname()
	return &Server{cfg: cfg, st: st, dep: dep, started: time.Now(), host: h}
}

// Handler 返回带中间件的根路由。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/v1/health", s.handleHealth)
	mux.HandleFunc("GET /api/v1/overview", s.handleOverview)
	mux.HandleFunc("GET /api/v1/assets", s.handleAssets)
	mux.HandleFunc("GET /api/v1/services", s.handleServices)
	mux.HandleFunc("GET /api/v1/releases", s.handleReleases)
	mux.HandleFunc("POST /api/v1/releases", s.handleCreateRelease)
	mux.HandleFunc("GET /api/v1/artifacts", s.handleArtifacts)
	mux.HandleFunc("GET /api/v1/alerts", s.handleAlerts)
	mux.HandleFunc("POST /api/v1/alerts/{id}/ack", s.handleAckAlert)
	mux.HandleFunc("GET /api/v1/rules", s.handleRules)
	mux.HandleFunc("GET /api/v1/silences", s.handleSilences)
	mux.HandleFunc("POST /api/v1/silences", s.handleCreateSilence)
	mux.HandleFunc("DELETE /api/v1/silences/{id}", s.handleDeleteSilence)
	mux.HandleFunc("GET /api/v1/capacity", s.handleCapacity)
	mux.HandleFunc("GET /api/v1/waf/rules", s.handleWAFRules)
	mux.HandleFunc("PATCH /api/v1/waf/rules/{id}", s.handleUpdateWAFRule)
	mux.HandleFunc("GET /api/v1/waf/attacks", s.handleAttacks)
	mux.HandleFunc("GET /api/v1/gateway", s.handleGatewayPreview)
	mux.HandleFunc("POST /api/v1/gateway/apply", s.handleGatewayApply)
	mux.HandleFunc("GET /api/v1/export/vmalert", s.handleExportVMAlert)
	mux.HandleFunc("GET /api/v1/export/modsec", s.handleExportModsec)

	// 注意：这里必须同时注册 GET 和 POST。nginx 的 auth_request 子请求
	// 会沿用原始请求的方法，如果只注册 POST，所有 GET 页面请求都会拿到 405，
	// 而 auth_request 把任何非 2xx 都当作"拒绝" —— 结果是整站 403。
	// 也不能用不带方法的 "/internal/waf/check"，那会和静态路由 "GET /" 冲突。
	mux.HandleFunc("GET /internal/waf/check", s.handleWAFCheck)
	mux.HandleFunc("POST /internal/waf/check", s.handleWAFCheck)
	mux.HandleFunc("POST /internal/ingest", s.handleIngest)

	mux.HandleFunc("GET /metrics", s.handleMetrics)

	mux.Handle("GET /", http.FileServer(http.FS(web.FS)))

	return recoverMW(logMW(mux))
}

// ---------- 中间件 ----------

type statusWriter struct {
	http.ResponseWriter
	code int
}

func (w *statusWriter) WriteHeader(c int) {
	w.code = c
	w.ResponseWriter.WriteHeader(c)
}

func logMW(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// WAF 检测与指标抓取频率高，不打日志，避免日志把自己刷爆
		if r.URL.Path == "/metrics" || r.URL.Path == "/internal/waf/check" {
			next.ServeHTTP(w, r)
			return
		}
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, code: 200}
		next.ServeHTTP(sw, r)
		fmt.Fprintf(os.Stdout, "[http] %s %s %d %s\n", r.Method, r.URL.Path, sw.code, time.Since(start).Round(time.Millisecond))
	})
}

func recoverMW(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if e := recover(); e != nil {
				// WAF 链路 panic 必须 fail-open，否则一个空指针就能让整站 403
				if r.URL.Path == "/internal/waf/check" {
					w.WriteHeader(http.StatusNoContent)
					return
				}
				http.Error(w, "internal error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// ---------- 基础 handler ----------

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{
		"ok":      true,
		"version": Version,
		"uptime":  int64(time.Since(s.started).Seconds()),
		"host":    s.host,
	})
}

func (s *Server) handleAssets(w http.ResponseWriter, r *http.Request) {
	var out []model.Asset
	s.st.View(func(st *model.State) {
		now := time.Now().Unix()
		out = make([]model.Asset, len(st.Assets))
		copy(out, st.Assets)
		for i := range out {
			switch {
			case out[i].LastSeen == 0:
				// 已登记但从未上报 —— 网络设备、NAS 这类装不了 agent 的资产
				// 会长期停在这个状态，不能算"离线故障"。
				out[i].Status = "pending"
			case now-out[i].LastSeen > 120:
				out[i].Status = "offline"
			default:
				out[i].Status = "online"
			}
		}
	})
	writeJSON(w, 200, out)
}

func (s *Server) handleServices(w http.ResponseWriter, r *http.Request) {
	now := time.Now().Unix()
	type svcOut struct {
		model.Service
		Availability float64 `json:"availability"`
		P95Latency   float64 `json:"p95Latency"`
		Up           int     `json:"up"`
	}
	var out []svcOut
	s.st.View(func(st *model.State) {
		for _, svc := range st.Services {
			o := svcOut{Service: svc}
			o.Availability = monitor.Availability(st, svc.Name, now, 6*time.Hour)
			lat := monitor.Window(st, "svc_latency_ms", svc.Name, now, 6*time.Hour)
			o.P95Latency = model.ComputeStats(lat).P95
			if v, ok := monitor.Latest(st, "svc_up", svc.Name); ok {
				o.Up = int(v)
			}
			out = append(out, o)
		}
	})
	writeJSON(w, 200, out)
}

func (s *Server) handleReleases(w http.ResponseWriter, r *http.Request) {
	var out []model.Release
	s.st.View(func(st *model.State) {
		out = append(out, st.Releases...)
	})
	if out == nil {
		out = []model.Release{}
	}
	writeJSON(w, 200, map[string]any{"releases": out, "labels": deploy.Labels()})
}

func (s *Server) handleCreateRelease(w http.ResponseWriter, r *http.Request) {
	var req deploy.Request
	if err := readJSON(r, &req); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	if !s.cfg.Deploy.Enabled {
		writeJSON(w, 403, map[string]string{"error": "当前环境未开启发布功能（deploy.enabled=false）"})
		return
	}
	rel, err := s.dep.Start(s.st, req)
	if err != nil {
		writeJSON(w, 409, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, rel)
}

// handleArtifacts 列出可发布产物。
// 产物目录由部署流水线写入，StormOps 只负责"看见"它们，不负责构建。
func (s *Server) handleArtifacts(w http.ResponseWriter, r *http.Request) {
	dir := s.cfg.Deploy.ReleasesDir
	type art struct {
		Name    string `json:"name"`
		Path    string `json:"path"`
		Version string `json:"version"`
		Size    int64  `json:"size"`
		ModTime int64  `json:"modTime"`
		Broken  bool   `json:"broken"`
	}
	out := []art{}
	if dir == "" {
		writeJSON(w, 200, map[string]any{"artifacts": out, "dir": ""})
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		writeJSON(w, 200, map[string]any{"artifacts": out, "dir": dir, "error": err.Error()})
		return
	}
	for _, e := range entries {
		if e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		ver := strings.TrimSuffix(e.Name(), filepath.Ext(e.Name()))
		out = append(out, art{
			Name:    e.Name(),
			Path:    filepath.Join(dir, e.Name()),
			Version: ver,
			Size:    info.Size(),
			ModTime: info.ModTime().Unix(),
			Broken:  strings.Contains(strings.ToLower(e.Name()), "bad"),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ModTime > out[j].ModTime })
	writeJSON(w, 200, map[string]any{"artifacts": out, "dir": dir})
}

func (s *Server) handleAlerts(w http.ResponseWriter, r *http.Request) {
	var active, resolved []model.Alert
	s.st.View(func(st *model.State) {
		for _, a := range st.Alerts {
			if a.Status == "firing" || a.Status == "acked" {
				active = append(active, a)
			} else {
				resolved = append(resolved, a)
			}
		}
	})
	sort.Slice(active, func(i, j int) bool {
		if active[i].Severity != active[j].Severity {
			return severityRank(active[i].Severity) < severityRank(active[j].Severity)
		}
		return active[i].StartedAt > active[j].StartedAt
	})
	sort.Slice(resolved, func(i, j int) bool { return resolved[i].UpdatedAt > resolved[j].UpdatedAt })
	if len(resolved) > 20 {
		resolved = resolved[:20]
	}
	if active == nil {
		active = []model.Alert{}
	}
	if resolved == nil {
		resolved = []model.Alert{}
	}
	writeJSON(w, 200, map[string]any{"active": active, "resolved": resolved})
}

func (s *Server) handleAckAlert(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	by := r.URL.Query().Get("by")
	if by == "" {
		by = "oncall"
	}
	found := false
	now := time.Now().Unix()
	s.st.Update(func(st *model.State) {
		for i := range st.Alerts {
			if st.Alerts[i].ID == id && st.Alerts[i].Status == "firing" {
				st.Alerts[i].Status = "acked"
				st.Alerts[i].AckedBy = by
				st.Alerts[i].UpdatedAt = now
				found = true
			}
		}
	})
	if !found {
		writeJSON(w, 404, map[string]string{"error": "告警不存在或已被认领"})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) handleRules(w http.ResponseWriter, r *http.Request) {
	var out []model.AlertRule
	s.st.View(func(st *model.State) { out = append(out, st.Rules...) })
	if out == nil {
		out = []model.AlertRule{}
	}
	writeJSON(w, 200, out)
}

func (s *Server) handleSilences(w http.ResponseWriter, r *http.Request) {
	var out []model.Silence
	now := time.Now().Unix()
	s.st.View(func(st *model.State) {
		for _, x := range st.Silences {
			if x.Until > now {
				out = append(out, x)
			}
		}
	})
	if out == nil {
		out = []model.Silence{}
	}
	writeJSON(w, 200, out)
}

func (s *Server) handleCreateSilence(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Host    string `json:"host"`
		Rule    string `json:"rule"`
		Reason  string `json:"reason"`
		Minutes int    `json:"minutes"`
		By      string `json:"by"`
	}
	if err := readJSON(r, &req); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	if req.Host == "" {
		req.Host = "*"
	}
	if req.Rule == "" {
		req.Rule = "*"
	}
	if req.Minutes <= 0 {
		req.Minutes = 60
	}
	if req.By == "" {
		req.By = "oncall"
	}
	now := time.Now().Unix()
	sl := model.Silence{
		ID:      fmt.Sprintf("si_%d", now),
		Host:    req.Host,
		Rule:    req.Rule,
		Reason:  req.Reason,
		By:      req.By,
		Until:   now + int64(req.Minutes)*60,
		Created: now,
	}
	s.st.Update(func(st *model.State) { st.Silences = append(st.Silences, sl) })
	writeJSON(w, 200, sl)
}

func (s *Server) handleDeleteSilence(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.st.Update(func(st *model.State) {
		out := st.Silences[:0]
		for _, x := range st.Silences {
			if x.ID != id {
				out = append(out, x)
			}
		}
		st.Silences = out
	})
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) handleCapacity(w http.ResponseWriter, r *http.Request) {
	out := []model.CapacityForecast{}
	s.st.View(func(st *model.State) {
		for _, v := range s.cfg.Capacity.Volumes {
			pts := st.Capacity[v.Name]
			fc := capacity.Forecast(v.Name, pts, v.Capacity, v.WarnPct, v.CritPct)
			out = append(out, fc)
		}
	})
	// 带宽 P95
	now := time.Now().Unix()
	var bwP95, bwPeak float64
	s.st.View(func(st *model.State) {
		var all []model.Sample
		for k, seq := range st.Series {
			m, _ := model.SplitSeriesKey(k)
			if m != "bw_mbps" {
				continue
			}
			cut := now - 6*3600
			for _, p := range seq {
				if p.TS >= cut {
					all = append(all, p)
				}
			}
		}
		stt := model.ComputeStats(all)
		bwP95 = stt.P95
		bwPeak = stt.Max
	})
	writeJSON(w, 200, map[string]any{
		"volumes": out,
		"bandwidth": map[string]any{
			"p95":  bwP95,
			"peak": bwPeak,
			"link": 5.0,
			"note": "链路标称 5Mbps，P95 是容量规划的真实依据，平均值会被夜间低谷拉低",
		},
	})
}

func severityRank(s string) int {
	switch s {
	case "critical":
		return 0
	case "warning":
		return 1
	}
	return 2
}

// ---------- WAF ----------

func (s *Server) handleWAFRules(w http.ResponseWriter, r *http.Request) {
	now := time.Now().Unix()
	var out []model.WAFRule
	s.st.View(func(st *model.State) {
		out = append(out, st.WAFRules...)
		// 命中数直接用审计事件聚合，而不是维护一个独立计数器 ——
		// 计数器一旦因为进程重启归零，就和日志对不上了，排障时会互相矛盾。
		hits := map[string]int{}
		for _, e := range st.Attacks {
			if e.TS >= now-86400 {
				hits[e.RuleID]++
			}
		}
		for i := range out {
			out[i].Hits = hits[out[i].ID]
		}
	})
	if out == nil {
		out = []model.WAFRule{}
	}
	writeJSON(w, 200, out)
}

func (s *Server) handleUpdateWAFRule(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req struct {
		Enabled    *bool   `json:"enabled"`
		RolloutPct *int    `json:"rolloutPct"`
		Action     *string `json:"action"`
	}
	if err := readJSON(r, &req); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	var updated *model.WAFRule
	now := time.Now().Unix()
	s.st.Update(func(st *model.State) {
		for i := range st.WAFRules {
			if st.WAFRules[i].ID != id {
				continue
			}
			if req.Enabled != nil {
				st.WAFRules[i].Enabled = *req.Enabled
			}
			if req.RolloutPct != nil {
				p := *req.RolloutPct
				if p < 0 {
					p = 0
				}
				if p > 100 {
					p = 100
				}
				st.WAFRules[i].RolloutPct = p
			}
			if req.Action != nil && (*req.Action == "block" || *req.Action == "log") {
				st.WAFRules[i].Action = *req.Action
			}
			st.WAFRules[i].UpdatedAt = now
			cp := st.WAFRules[i]
			updated = &cp
		}
	})
	if updated == nil {
		writeJSON(w, 404, map[string]string{"error": "规则不存在"})
		return
	}
	writeJSON(w, 200, updated)
}

// handleWAFCheck 是 Nginx auth_request 的后端。
//
// 性能约束：这个接口在每一个业务请求上都会被调用。
// 因此这里刻意不做任何落盘、不写日志、不加全局锁的耗时操作。
// 判定只用内存里的正则缓存。
func (s *Server) handleWAFCheck(w http.ResponseWriter, r *http.Request) {
	req := waf.Request{
		IP:     firstNonEmpty(r.Header.Get("X-Real-IP"), clientIP(r)),
		Method: r.Header.Get("X-Original-Method"),
		URI:    r.Header.Get("X-Original-URI"),
		UA:     r.Header.Get("User-Agent"),
		Headers: map[string]string{
			"User-Agent": r.Header.Get("User-Agent"),
		},
	}
	if req.URI == "" {
		req.URI = "/"
	}
	if req.Method == "" {
		req.Method = http.MethodGet
	}

	var decision waf.Decision
	s.st.View(func(st *model.State) {
		decision = waf.Check(st.WAFRules, req, time.Now().Unix())
	})

	if decision.RuleID != "" {
		s.st.Update(func(st *model.State) {
			st.Attacks = append(st.Attacks, model.AttackEvent{
				TS:       time.Now().Unix(),
				IP:       req.IP,
				Country:  "未知",
				Method:   req.Method,
				Path:     truncate(req.URI, 300),
				RuleID:   decision.RuleID,
				RuleName: decision.RuleName,
				Category: decision.Category,
				Action:   decision.Action,
				UA:       truncate(req.UA, 160),
			})
			if len(st.Attacks) > 5000 {
				st.Attacks = st.Attacks[len(st.Attacks)-5000:]
			}
			for i := range st.WAFRules {
				if st.WAFRules[i].ID == decision.RuleID {
					st.WAFRules[i].Hits++
				}
			}
		})
	}

	if decision.Blocked {
		w.Header().Set("X-WAF-Rule", decision.RuleID+" "+decision.RuleName)
		w.WriteHeader(http.StatusForbidden)
		return
	}
	if decision.RuleID != "" {
		w.Header().Set("X-WAF-Rule", decision.RuleID+" (log)")
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleAttacks(w http.ResponseWriter, r *http.Request) {
	now := time.Now().Unix()
	cut := now - 86400
	var events []model.AttackEvent
	s.st.View(func(st *model.State) {
		for _, e := range st.Attacks {
			if e.TS >= cut {
				events = append(events, e)
			}
		}
	})
	sort.Slice(events, func(i, j int) bool { return events[i].TS > events[j].TS })

	// 24 小时 × 1 小时的分桶，用于前端画柱状图
	hits := make([]int, 24)
	blocks := make([]int, 24)
	for _, e := range events {
		idx := 23 - int((now-e.TS)/3600)
		if idx >= 0 && idx < 24 {
			hits[idx]++
			if e.Action == "block" {
				blocks[idx]++
			}
		}
	}

	total := len(events)
	blocked := 0
	for _, e := range events {
		if e.Action == "block" {
			blocked++
		}
	}

	recent := events
	if len(recent) > 60 {
		recent = recent[:60]
	}
	if recent == nil {
		recent = []model.AttackEvent{}
	}
	writeJSON(w, 200, map[string]any{
		"total":       total,
		"blocked":     blocked,
		"logged":      total - blocked,
		"events":      recent,
		"topAttackers": waf.TopAttackers(events, 6),
		"categories":  waf.ByCategory(events),
		"hourly":      map[string]any{"hits": hits, "blocks": blocks},
	})
}

// ---------- 网关 ----------

func (s *Server) handleGatewayPreview(w http.ResponseWriter, r *http.Request) {
	var svcs []model.Service
	s.st.View(func(st *model.State) { svcs = append(svcs, st.Services...) })
	conf, err := gateway.Render(s.cfg.Gateway, svcs)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	path := s.cfg.Gateway.ConfPath
	if path == "" {
		path = "/etc/nginx/conf.d/stormops.conf"
	}
	onDisk, _ := os.ReadFile(path)
	writeJSON(w, 200, map[string]any{
		"conf":    conf,
		"path":    path,
		"applied": string(onDisk) == conf,
		"exists":  len(onDisk) > 0,
	})
}

func (s *Server) handleGatewayApply(w http.ResponseWriter, r *http.Request) {
	var svcs []model.Service
	s.st.View(func(st *model.State) { svcs = append(svcs, st.Services...) })
	conf, err := gateway.Render(s.cfg.Gateway, svcs)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	res := gateway.Apply(conf, s.cfg.Gateway)
	writeJSON(w, 200, res)
}

// ---------- 指标上报 ----------

func (s *Server) handleIngest(w http.ResponseWriter, r *http.Request) {
	if tok := s.cfg.Agents; len(tok) > 0 {
		if r.Header.Get("X-Agent-Token") != tok[0].Token {
			writeJSON(w, 401, map[string]string{"error": "token 无效"})
			return
		}
	}
	var rep agent.Report
	if err := readJSON(r, &rep); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	if rep.Host == "" {
		writeJSON(w, 400, map[string]string{"error": "host 不能为空"})
		return
	}
	now := time.Now().Unix()
	if rep.TS == 0 {
		rep.TS = now
	}

	s.st.Update(func(st *model.State) {
		monitor.Ingest(st, rep.Host, rep.Samples, rep.TS)
		for _, sp := range rep.Services {
			monitor.Ingest(st, sp.Name, map[string]float64{
				"svc_up":         float64(sp.Up),
				"svc_latency_ms": sp.LatencyMs,
			}, rep.TS)
		}
		if _, ok := st.Meta["lastIngest"]; !ok {
			st.Meta["lastIngest"] = strconv.FormatInt(now, 10)
		}
		st.Meta["lastIngest"] = strconv.FormatInt(now, 10)
	})
	s.st.Update(func(st *model.State) { monitor.EvalAll(st, now) })

	writeJSON(w, 200, map[string]any{"ok": true, "ts": now})
}

// ---------- /metrics ----------

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	now := time.Now().Unix()
	var b strings.Builder

	b.WriteString("# HELP stormops_up StormOps 自身存活\n# TYPE stormops_up gauge\n")
	b.WriteString("stormops_up 1\n")
	b.WriteString("# HELP stormops_uptime_seconds 运行时长\n# TYPE stormops_uptime_seconds gauge\n")
	fmt.Fprintf(&b, "stormops_uptime_seconds %d\n", int64(time.Since(s.started).Seconds()))
	b.WriteString("# HELP stormops_goroutines 协程数\n# TYPE stormops_goroutines gauge\n")
	fmt.Fprintf(&b, "stormops_goroutines %d\n", runtime.NumGoroutine())

	s.st.View(func(st *model.State) {
		b.WriteString("# HELP stormops_host_cpu_pct 主机 CPU 使用率\n# TYPE stormops_host_cpu_pct gauge\n")
		for _, a := range st.Assets {
			if v, ok := monitor.Latest(st, "cpu_pct", a.Hostname); ok {
				fmt.Fprintf(&b, "stormops_host_cpu_pct{host=%q,role=%q} %.2f\n", a.Hostname, a.Role, v)
			}
		}
		b.WriteString("# HELP stormops_host_mem_pct 主机内存使用率\n# TYPE stormops_host_mem_pct gauge\n")
		for _, a := range st.Assets {
			if v, ok := monitor.Latest(st, "mem_used_pct", a.Hostname); ok {
				fmt.Fprintf(&b, "stormops_host_mem_pct{host=%q} %.2f\n", a.Hostname, v)
			}
		}
		b.WriteString("# HELP stormops_host_disk_pct 主机磁盘水位\n# TYPE stormops_host_disk_pct gauge\n")
		for _, a := range st.Assets {
			if v, ok := monitor.Latest(st, "disk_used_pct", a.Hostname); ok {
				fmt.Fprintf(&b, "stormops_host_disk_pct{host=%q} %.2f\n", a.Hostname, v)
			}
		}
		b.WriteString("# HELP stormops_svc_up 服务存活\n# TYPE stormops_svc_up gauge\n")
		for _, svc := range st.Services {
			v, ok := monitor.Latest(st, "svc_up", svc.Name)
			if !ok {
				v = 0
			}
			fmt.Fprintf(&b, "stormops_svc_up{service=%q,domain=%q} %.0f\n", svc.Name, svc.Domain, v)
		}
		firing := map[string]int{}
		for _, a := range st.Alerts {
			if a.Status == "firing" || a.Status == "acked" {
				firing[a.Severity]++
			}
		}
		b.WriteString("# HELP stormops_alerts_firing 活跃告警数\n# TYPE stormops_alerts_firing gauge\n")
		for _, sev := range []string{"critical", "warning", "info"} {
			fmt.Fprintf(&b, "stormops_alerts_firing{severity=%q} %d\n", sev, firing[sev])
		}
		blocked24h := 0
		for _, e := range st.Attacks {
			if e.TS >= now-86400 && e.Action == "block" {
				blocked24h++
			}
		}
		b.WriteString("# HELP stormops_waf_blocked_24h 24小时拦截次数\n# TYPE stormops_waf_blocked_24h gauge\n")
		fmt.Fprintf(&b, "stormops_waf_blocked_24h %d\n", blocked24h)
	})
	_, _ = w.Write([]byte(b.String()))
}

// ---------- 概览 ----------

func (s *Server) handleOverview(w http.ResponseWriter, r *http.Request) {
	now := time.Now().Unix()
	since := now - 6*3600

	type assetStat struct {
		Total   int `json:"total"`
		Online  int `json:"online"`
		Pending int `json:"pending"`
		Offline int `json:"offline"`
	}
	type svcStat struct {
		Total   int `json:"total"`
		Healthy int `json:"healthy"`
	}
	type alertStat struct {
		Firing   int `json:"firing"`
		Critical int `json:"critical"`
		Warning  int `json:"warning"`
		Acked    int `json:"acked"`
	}
	type wafStat struct {
		Total   int `json:"total"`
		Blocked int `json:"blocked"`
	}

	resp := map[string]any{}
	var as assetStat
	var ss svcStat
	var als alertStat
	var ws wafStat
	var last *model.Release

	s.st.View(func(st *model.State) {
		as.Total = len(st.Assets)
		for _, a := range st.Assets {
			switch {
			case a.LastSeen == 0:
				as.Pending++
			case now-a.LastSeen > 120:
				as.Offline++
			default:
				as.Online++
			}
		}
		ss.Total = len(st.Services)
		for _, svc := range st.Services {
			if v, ok := monitor.Latest(st, "svc_up", svc.Name); ok && v >= 1 {
				ss.Healthy++
			}
		}
		for _, a := range st.Alerts {
			if a.Status == "firing" || a.Status == "acked" {
				als.Firing++
				if a.Status == "acked" {
					als.Acked++
				}
				switch a.Severity {
				case "critical":
					als.Critical++
				case "warning":
					als.Warning++
				}
			}
		}
		for _, e := range st.Attacks {
			if e.TS >= now-86400 {
				ws.Total++
				if e.Action == "block" {
					ws.Blocked++
				}
			}
		}
		if len(st.Releases) > 0 {
			cp := st.Releases[0]
			last = &cp
		}
	})

	// 聚合曲线（5 分钟一桶，6 小时 = 72 点，前端画图足够细腻又不至于卡）
	seriesOut := map[string][]model.Sample{}
	s.st.View(func(st *model.State) {
		for _, m := range []string{"cpu_pct", "bw_mbps", "mem_used_pct"} {
			seriesOut[m] = aggregate(st, m, since, 300)
		}
	})

	vols := []model.CapacityForecast{}
	s.st.View(func(st *model.State) {
		for _, v := range s.cfg.Capacity.Volumes {
			vols = append(vols, capacity.Forecast(v.Name, st.Capacity[v.Name], v.Capacity, v.WarnPct, v.CritPct))
		}
	})

	var events []model.AttackEvent
	s.st.View(func(st *model.State) {
		for _, e := range st.Attacks {
			if e.TS >= now-86400 {
				events = append(events, e)
			}
		}
	})

	resp["generatedAt"] = now
	resp["org"] = s.cfg.Org
	resp["env"] = "生产"
	resp["host"] = s.host
	resp["version"] = Version
	resp["uptime"] = int64(time.Since(s.started).Seconds())
	resp["assets"] = as
	resp["services"] = ss
	resp["alerts"] = als
	resp["waf"] = ws
	resp["lastRelease"] = last
	resp["series"] = seriesOut
	resp["storage"] = vols
	resp["topAttackers"] = waf.TopAttackers(events, 5)
	resp["wafCategories"] = waf.ByCategory(events)
	resp["binaryMB"] = float64(binarySizeMB())
	writeJSON(w, 200, resp)
}

func aggregate(st *model.State, metric string, since, bucket int64) []model.Sample {
	sums := map[int64]float64{}
	counts := map[int64]int{}
	for k, seq := range st.Series {
		m, _ := model.SplitSeriesKey(k)
		if m != metric {
			continue
		}
		for _, p := range seq {
			if p.TS < since {
				continue
			}
			b := p.TS / bucket * bucket
			sums[b] += p.Value
			counts[b]++
		}
	}
	out := make([]model.Sample, 0, len(sums))
	for b, s := range sums {
		out = append(out, model.Sample{TS: b, Value: float64(int64(s/float64(counts[b])*100)) / 100})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TS < out[j].TS })
	return out
}

// ---------- 导出 ----------

func (s *Server) handleExportVMAlert(w http.ResponseWriter, r *http.Request) {
	var rules []model.AlertRule
	s.st.View(func(st *model.State) { rules = append(rules, st.Rules...) })

	var b strings.Builder
	b.WriteString("# 由 StormOps 导出的 vmalert 规则\n")
	b.WriteString("# 用途：把 StormOps 的规则定义同步到 VictoriaMetrics 告警链路，\n")
	b.WriteString("#       让\"平台自身规则\"和\"VM 侧规则\"保持单一真相源。\n")
	b.WriteString("groups:\n  - name: stormops\n    interval: 30s\n    rules:\n")
	for _, rule := range rules {
		if !rule.Enabled {
			continue
		}
		metric := vmalertMetric(rule.Metric)
		fmt.Fprintf(&b, "      - alert: %s\n", rule.Name)
		fmt.Fprintf(&b, "        expr: %s %s %g\n", metric, rule.Op, rule.Threshold)
		fmt.Fprintf(&b, "        for: %ds\n", rule.ForSec)
		b.WriteString("        labels:\n")
		fmt.Fprintf(&b, "          severity: %s\n", rule.Severity)
		fmt.Fprintf(&b, "          runbook: \"%s\"\n", rule.Runbook)
		b.WriteString("        annotations:\n")
		fmt.Fprintf(&b, "          summary: \"%s\"\n", strings.ReplaceAll(rule.Summary, "\"", "'"))
		b.WriteString("\n")
	}
	w.Header().Set("Content-Type", "text/yaml; charset=utf-8")
	_, _ = w.Write([]byte(b.String()))
}

func vmalertMetric(m string) string {
	switch m {
	case "cpu_pct":
		return "stormops_host_cpu_pct"
	case "mem_used_pct":
		return "stormops_host_mem_pct"
	case "disk_used_pct":
		return "stormops_host_disk_pct"
	case "svc_up":
		return "stormops_svc_up"
	}
	return m
}

func (s *Server) handleExportModsec(w http.ResponseWriter, r *http.Request) {
	var rules []model.WAFRule
	s.st.View(func(st *model.State) { rules = append(rules, st.WAFRules...) })
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte(waf.Compile(rules)))
}

// ---------- 工具 ----------

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

func readJSON(r *http.Request, v any) error {
	defer r.Body.Close()
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	return dec.Decode(v)
}

func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		return strings.TrimSpace(strings.Split(xff, ",")[0])
	}
	host := r.RemoteAddr
	if i := strings.LastIndex(host, ":"); i > 0 {
		return host[:i]
	}
	return host
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func binarySizeMB() int64 {
	exe, err := os.Executable()
	if err != nil {
		return 0
	}
	info, err := os.Stat(exe)
	if err != nil {
		return 0
	}
	return info.Size() / 1024 / 1024
}
