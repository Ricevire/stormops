// Package monitor 实现指标存储、统计与告警判定。
//
// 设计取舍：指标不走数据库，走内存 map + 定长环形裁剪。
// 因为 stormagent 每 15 秒上报一次、单机指标数在 20 个以内，
// 一天也就 10 万级样本，内存占用不到 20MB，查询是 O(n) 但 n 很小。
// 真正的长周期指标交给 VictoriaMetrics —— 本模块只负责"最近 24 小时 + 告警判定"，
// 全量历史通过 /metrics 暴露给 vmagent 抓取，两条链路分工明确（见 ADR-003）。
package monitor

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Ricevire/stormops/internal/model"
)

// MaxSamplesPerSeries 是单序列保留的样本上限（24h，按 1 分钟一个点估算）。
const MaxSamplesPerSeries = 1440

// Ingest 把一次上报写入状态。samples 的 key 是指标名。
func Ingest(st *model.State, host string, samples map[string]float64, now int64) {
	if st.Series == nil {
		st.Series = map[string][]model.Sample{}
	}
	for name, v := range samples {
		key := model.SeriesKey(name, host)
		seq := st.Series[key]
		seq = append(seq, model.Sample{TS: now, Value: v})
		if len(seq) > MaxSamplesPerSeries {
			seq = seq[len(seq)-MaxSamplesPerSeries:]
		}
		st.Series[key] = seq
	}
	// 顺手维护资产心跳，避免 agent 活着但 CMDB 显示 offline
	for i := range st.Assets {
		if st.Assets[i].Hostname == host {
			st.Assets[i].LastSeen = now
			if st.Assets[i].Status == "offline" {
				st.Assets[i].Status = "online"
			}
		}
	}
}

// Latest 取某指标在某主机上的最新值。
func Latest(st *model.State, metric, host string) (float64, bool) {
	seq := st.Series[model.SeriesKey(metric, host)]
	if len(seq) == 0 {
		return 0, false
	}
	return seq[len(seq)-1].Value, true
}

// Window 取最近 dur 时间内的样本。
func Window(st *model.State, metric, host string, now int64, dur time.Duration) []model.Sample {
	seq := st.Series[model.SeriesKey(metric, host)]
	if len(seq) == 0 {
		return nil
	}
	cut := now - int64(dur.Seconds())
	i := sort.Search(len(seq), func(i int) bool { return seq[i].TS >= cut })
	return seq[i:]
}

// Hosts 返回所有上报过该指标的主机名。
func Hosts(st *model.State, metric string) []string {
	prefix := metric + "|"
	out := make([]string, 0, 8)
	for k := range st.Series {
		if strings.HasPrefix(k, prefix) {
			out = append(out, k[len(prefix):])
		}
	}
	sort.Strings(out)
	return out
}

func breachSustained(seq []model.Sample, op string, th float64, forSec int, now int64) (bool, float64) {
	if len(seq) == 0 {
		return false, 0
	}
	last := seq[len(seq)-1].Value
	if !model.Compare(last, op, th) {
		return false, last
	}
	if forSec <= 0 {
		return true, last
	}
	// 用 0.8 倍容差判定"持续"：上报间隔抖动不该让告警永远不触发，
	// 但也不能拿两个相邻的瞬时点就宣布故障。
	span := now - seq[0].TS
	if span < int64(float64(forSec)*0.8) {
		return false, last
	}
	for _, s := range seq {
		if !model.Compare(s.Value, op, th) {
			return false, last
		}
	}
	return true, last
}

// EvalResult 是一次告警判定的结果摘要。
type EvalResult struct {
	Created   int `json:"created"`
	Resolved  int `json:"resolved"`
	Suppressed int `json:"suppressed"`
}

// EvalAll 遍历所有启用规则，更新告警状态。
//
// 幂等：重复调用不会产生重复告警。同一个 (rule, host) 只会有一条活跃告警。
func EvalAll(st *model.State, now int64) EvalResult {
	var res EvalResult
	active := map[string]*model.Alert{}
	for i := range st.Alerts {
		a := &st.Alerts[i]
		if a.Status == "firing" || a.Status == "acked" {
			active[a.RuleID+"|"+a.Host] = a
		}
	}

	for _, r := range st.Rules {
		if !r.Enabled {
			continue
		}
		forSec := r.ForSec
		dur := time.Duration(forSec+120) * time.Second
		for _, host := range Hosts(st, r.Metric) {
			key := r.ID + "|" + host
			seq := Window(st, r.Metric, host, now, dur)
			hit, val := breachSustained(seq, r.Op, r.Threshold, forSec, now)

			if hit {
				if a, ok := active[key]; ok {
					a.Value = val
					a.UpdatedAt = now
					a.Count++
					continue
				}
				if silenced(st, host, r.Name, now) {
					res.Suppressed++
					continue
				}
				st.Alerts = append(st.Alerts, model.Alert{
					ID:        fmt.Sprintf("al_%d_%s", now, shortHash(key)),
					RuleID:    r.ID,
					RuleName:  r.Name,
					Host:      host,
					Metric:    r.Metric,
					Severity:  r.Severity,
					Value:     val,
					Threshold: r.Threshold,
					Summary:   renderSummary(r.Summary, host, val, r.Threshold),
					Runbook:   r.Runbook,
					Status:    "firing",
					StartedAt: now,
					UpdatedAt: now,
					Count:     1,
				})
				res.Created++
				continue
			}

			if a, ok := active[key]; ok {
				a.Status = "resolved"
				a.UpdatedAt = now
				a.Value = val
				res.Resolved++
			}
		}
	}
	return res
}

func silenced(st *model.State, host, rule string, now int64) bool {
	for _, s := range st.Silences {
		if s.Matches(host, rule, now) {
			return true
		}
	}
	return false
}

func renderSummary(tpl, host string, val, th float64) string {
	if tpl == "" {
		return host + " 指标越线"
	}
	r := strings.NewReplacer(
		"{{host}}", host,
		"{{value}}", trimFloat(val),
		"{{threshold}}", trimFloat(th),
	)
	return r.Replace(tpl)
}

func trimFloat(v float64) string {
	s := fmt.Sprintf("%.2f", v)
	s = strings.TrimRight(s, "0")
	return strings.TrimRight(s, ".")
}

func shortHash(s string) string {
	// FNV-1a，够用且不引入依赖
	var h uint32 = 2166136261
	for i := 0; i < len(s); i++ {
		h ^= uint32(s[i])
		h *= 16777619
	}
	return fmt.Sprintf("%06x", h&0xffffff)
}

// Availability 计算某服务最近 dur 的可用率，基于 agent 上报的 svc_up 序列。
func Availability(st *model.State, host string, now int64, dur time.Duration) float64 {
	seq := Window(st, "svc_up", host, now, dur)
	if len(seq) == 0 {
		return 0
	}
	up := 0.0
	for _, s := range seq {
		if s.Value >= 1 {
			up++
		}
	}
	return up / float64(len(seq)) * 100
}
