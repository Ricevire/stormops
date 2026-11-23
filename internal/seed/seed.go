// Package seed 生成首启动的演示数据。
//
// 为什么要它：容量预测、告警判定这些东西的价值在"趋势"上，
// 一台刚装好的机器没有任何历史，界面全是 0，既没法演示也没法自测。
//
// 重要说明：这里生成的是**演示数据**，仅用于首次启动把界面填满。
// 告警不是写死的，而是灌入符合媒体公司负载形态的时序数据之后，
// 由 monitor.EvalAll 用真实规则判定出来的 —— 也就是说，
// 规则写错了，这里就不会出现告警，这本身就是对规则引擎的一次自测。
// 生产部署请把 config.json 里的 seedDemo 设为 false。
package seed

import (
	"math"
	"math/rand"
	"time"

	"github.com/Ricevire/stormops/internal/config"
	"github.com/Ricevire/stormops/internal/model"
	"github.com/Ricevire/stormops/internal/monitor"
	"github.com/Ricevire/stormops/internal/store"
)

// Apply 在状态为空时灌入演示数据。
func Apply(st *store.Store, cfg *config.Config) {
	seeded := false
	st.View(func(s *model.State) {
		seeded = s.Meta["seeded"] == "1"
	})
	if seeded {
		return
	}

	st.Update(func(s *model.State) {
		if len(s.Assets) == 0 {
			s.Assets = append(s.Assets, cfg.Assets...)
		}
		if len(s.Services) == 0 {
			s.Services = append(s.Services, cfg.Services...)
		}
		if len(s.Rules) == 0 {
			s.Rules = append(s.Rules, cfg.Rules...)
		}
		if len(s.WAFRules) == 0 {
			s.WAFRules = append(s.WAFRules, cfg.WAFRules...)
		}
		s.Meta["seeded"] = "1"
	})

	now := nowSec()
	rnd := rand.New(rand.NewSource(20261123))

	seedSeries(st, cfg, now, rnd)
	seedCapacity(st, cfg, rnd)
	seedAttacks(st, now, rnd)
	seedReleases(st, now)

	// 灌完数据后立刻跑一次规则判定，让首屏就有符合逻辑的活跃告警
	st.Update(func(s *model.State) {
		monitor.EvalAll(s, now)
	})
}

func nowSec() int64 { return timeNow().Unix() }

// seedSeries 生成最近 6 小时的分钟级指标。
//
// 形态按内容公司的真实作息设计：
//   - 9:00-21:00 是拍摄/剪辑高峰，CPU 与带宽明显抬升；
//   - 素材回传（异地拍摄）会在 14:00 和 20:00 左右出现两个带宽尖峰；
//   - 转码队列堆积时 CPU 会持续高位，这正是我们要演示告警的场景。
func seedSeries(st *store.Store, cfg *config.Config, now int64, rnd *rand.Rand) {
	const step = 60
	const points = 360 // 6 小时

	hosts := len(cfg.Assets)
	if hosts == 0 {
		hosts = 1
	}

	st.Update(func(s *model.State) {
		if s.Series == nil {
			s.Series = map[string][]model.Sample{}
		}
		for _, a := range cfg.Assets {
			for p := points; p > 0; p-- {
				ts := now - int64(p*step)
				tod := float64((ts % 86400) / 3600)
				// 工作时段系数：9-21 点高，凌晨低
				work := 0.35
				if tod >= 9 && tod <= 21 {
					work = 0.75 + 0.25*math.Sin((tod-9)/12*math.Pi)
				}

				cpu := 18 + 26*work + rnd.NormFloat64()*4
				if a.Role == "storage" {
					cpu = 12 + 10*work + rnd.NormFloat64()*2
				}
				// 业务机最近 12 分钟进入转码积压状态，用于演示 CPU 告警及其自愈
				if a.Role == "web" && p < 12 {
					cpu = 88 + rnd.Float64()*7
				}
				mem := 42 + 18*work + rnd.NormFloat64()*3
				if a.Role == "db" {
					mem = 68 + 6*work + rnd.NormFloat64()*2
				}
				disk := 30 + rnd.Float64()*6
				if a.Role == "storage" {
					disk = 86.4 + rnd.Float64()*0.3 // 素材库水位告警场景
				}
				// 带宽必须压在标称 5Mbps 以内：一旦模拟值超过链路带宽，
				// "P95 超过链路"这种自相矛盾的数字会直接毁掉页面可信度。
				bw := 0.9 + 2.4*work + rnd.Float64()*0.6
				if tod >= 13.5 && tod <= 15 {
					bw += 0.9 + rnd.Float64()*0.5 // 素材回传尖峰
				}
				if tod >= 19.5 && tod <= 20.5 {
					bw += 0.7 + rnd.Float64()*0.4
				}
				if bw > 4.85 {
					bw = 4.85 - rnd.Float64()*0.25
				}

				add(s, "cpu_pct", a.Hostname, ts, cpu)
				add(s, "mem_used_pct", a.Hostname, ts, mem)
				add(s, "disk_used_pct", a.Hostname, ts, disk)
				add(s, "bw_mbps", a.Hostname, ts, bw)
				add(s, "load1_per_core", a.Hostname, ts, cpu/100*1.6+rnd.Float64()*0.2)
			}
		}

		// 服务存活探测：让 www 服务在最后 8 分钟挂掉一次，演示服务不可用告警
		for idx, svc := range cfg.Services {
			for p := points; p > 0; p-- {
				ts := now - int64(p*step)
				up := 1.0
				if idx == 0 && p < 8 {
					up = 0
				}
				if idx == 2 && p > 40 && p < 44 {
					up = 0 // 历史上的一次短暂抖动，已恢复
				}
				add(s, "svc_up", svc.Name, ts, up)
				add(s, "svc_latency_ms", svc.Name, ts, 40+rnd.Float64()*60)
			}
		}
	})
}

func add(s *model.State, metric, host string, ts int64, v float64) {
	key := model.SeriesKey(metric, host)
	s.Series[key] = append(s.Series[key], model.Sample{TS: ts, Value: round2(v)})
}

// seedCapacity 生成 90 天存储增长历史。
func seedCapacity(st *store.Store, cfg *config.Config, rnd *rand.Rand) {
	st.Update(func(s *model.State) {
		if s.Capacity == nil {
			s.Capacity = map[string][]model.CapPoint{}
		}
		base := timeToday().AddDate(0, 0, -89)
		for _, v := range cfg.Capacity.Volumes {
			var start float64
			var daily float64
			switch v.Name {
			case "素材库":
				start = 8.4e12  // 8.4 TB
				daily = 1.15e11 // 约 115GB/天
			case "备份卷":
				start = 2.1e12
				daily = 2.6e10
			default:
				start = 1e12
				daily = 1e10
			}
			cur := start
			for d := 0; d < 90; d++ {
				// 周末增长慢（没人拍摄），工作日快
				wd := base.AddDate(0, 0, d).Weekday()
				mult := 0.35
				if wd != 0 && wd != 6 {
					mult = 1.0 + rnd.Float64()*0.6
				}
				cur += daily * mult
				s.Capacity[v.Name] = append(s.Capacity[v.Name], model.CapPoint{
					Date:  base.AddDate(0, 0, d).Format("2006-01-02"),
					Bytes: cur,
				})
			}
		}
	})
}

// seedAttacks 生成最近 24 小时的 WAF 命中事件。
//
// IP 全部取自 TEST-NET 保留段（RFC 5737），不影射任何真实地址。
func seedAttacks(st *store.Store, now int64, rnd *rand.Rand) {
	type tmpl struct {
		cat, path, ua string
		ruleID        string
		ruleName      string
	}
	tpls := []tmpl{
		{"sqli", "/course/detail?id=1%27%20OR%20%271%27%3D%271", "sqlmap/1.7.2#stable (https://sqlmap.org)", "W1001", "SQL 注入特征"},
		{"xss", "/search?q=%3Cscript%3Ealert(1)%3C/script%3E", "Mozilla/5.0 (Windows NT 10.0)", "W1002", "XSS 脚本注入"},
		{"scanner", "/wp-login.php", "Mozilla/5.0 zgrab/0.x", "W1003", "高危路径扫描"},
		{"scanner", "/.env", "curl/8.4.0", "W1003", "高危路径扫描"},
		{"rce", "/api/upload?cmd=;cat%20/etc/passwd", "python-requests/2.31", "W1004", "命令注入特征"},
		{"sqli", "/shop/list?sort=1%20UNION%20SELECT%20NULL", "Mozilla/5.0 (X11; Linux x86_64)", "W1001", "SQL 注入特征"},
		{"custom", "/api/v1/order?id=..%2F..%2Fetc%2Fpasswd", "Mozilla/5.0", "W1005", "路径穿越"},
	}
	ips := []string{
		"203.0.113.42", "203.0.113.77", "198.51.100.23", "198.51.100.61",
		"203.0.113.118", "198.51.100.9", "203.0.113.201", "198.51.100.145",
	}
	// 一个"疑似误报"的来源：公司出口 IP 命中路径穿越规则
	egress := "203.0.113.10"

	st.Update(func(s *model.State) {
		for i := 0; i < 168; i++ {
			t := tpls[rnd.Intn(len(tpls))]
			ts := now - int64(rnd.Float64()*86400)
			ip := ips[rnd.Intn(len(ips))]
			method := "GET"
			if t.cat == "rce" {
				method = "POST"
			}
			s.Attacks = append(s.Attacks, model.AttackEvent{
				TS:       ts,
				IP:       ip,
				Country:  "未知",
				Method:   method,
				Path:     t.path,
				RuleID:   t.ruleID,
				RuleName: t.ruleName,
				Category: t.cat,
				Action:   "block",
				UA:       t.ua,
			})
		}
		for i := 0; i < 6; i++ {
			s.Attacks = append(s.Attacks, model.AttackEvent{
				TS:       now - int64(rnd.Float64()*7200),
				IP:       egress,
				Country:  "内网出口",
				Method:   "GET",
				Path:     "/api/v1/asset/preview?file=../../materials/2026-11/a001.mov",
				RuleID:   "W1005",
				RuleName: "路径穿越",
				Category: "custom",
				Action:   "log", // 灰度观察中，只记录不拦截
				UA:       "Mozilla/5.0 (Macintosh; Intel Mac OS X 14_6)",
			})
		}
	})
}

// seedReleases 生成几条历史发布记录。
func seedReleases(st *store.Store, now int64) {
	mk := func(id, ver, prev, status string, offset int64, steps [][3]string) model.Release {
		r := model.Release{
			ID: id, Service: "官网/课程站", Version: ver, PrevVersion: prev,
			Strategy: "rolling", Status: status, Operator: "wangjingyu",
			StartedAt: now - offset, EndedAt: now - offset + 46,
		}
		if status != "running" {
			r.Note = ""
		}
		for _, s := range steps {
			r.Steps = append(r.Steps, model.ReleaseStep{Name: s[0], Status: s[1], Detail: s[2]})
		}
		return r
	}
	okSteps := [][3]string{
		{"precheck", "success", "产物存在，磁盘余量充足"},
		{"backup", "success", "已备份当前版本"},
		{"stage", "success", "已暂存到 /opt/stormsite/stage/artifact-v1.4.2"},
		{"verify", "success", "sha256=8f3a91c02d1e 大小=9124KB 权限=-rwxr-xr-x"},
		{"switch", "success", "已切换到新版本并重启服务"},
		{"health", "success", "连续 3 次健康检查通过（共尝试 3 次）"},
		{"rollback", "skipped", ""},
	}
	badSteps := [][3]string{
		{"precheck", "success", "产物存在，磁盘余量充足"},
		{"backup", "success", "已备份当前版本"},
		{"stage", "success", "已暂存到 /opt/stormsite/stage/artifact-v1.4.1-bad"},
		{"verify", "success", "sha256=1c77ab9e4f02 大小=9118KB 权限=-rwxr-xr-x"},
		{"switch", "success", "已切换到新版本并重启服务"},
		{"health", "failed", "30 秒内未通过健康检查（43 次尝试，最后一次错误：HTTP 500: db not ready）"},
		{"rollback", "success", "已回滚并恢复健康：连续 3 次健康检查通过"},
	}
	st.Update(func(s *model.State) {
		s.Releases = []model.Release{
			mk("rel_1", "v1.4.2", "v1.4.1", "success", 3600*20, okSteps),
			mk("rel_2", "v1.4.1", "v1.4.0", "rolled_back", 3600*52, badSteps),
			mk("rel_3", "v1.4.1", "v1.4.0", "success", 3600*53, okSteps),
		}
	})
}

func round2(v float64) float64 {
	return math.Round(v*100) / 100
}

// DemoTick 在演示模式下为所有节点补一条采样。
//
// 为什么需要它：真实部署里 agent 会持续上报，但演示环境只有一台机器，
// 另外几台"外部采集"的节点如果没有新数据，指标窗口会在 6 小时后滑空，
// dashboard 就变成一堆空图。这个函数保证演示环境永远是"活"的。
//
// 生产环境（demo=false）不会调用它。
func DemoTick(st *store.Store, cfg *config.Config) {
	now := time.Now()
	tod := float64(now.Hour())
	work := 0.35
	if tod >= 9 && tod <= 21 {
		work = 0.75 + 0.25*math.Sin((tod-9)/12*math.Pi)
	}
	rnd := rand.New(rand.NewSource(now.UnixNano()))
	ts := now.Unix()

	st.Update(func(s *model.State) {
		for _, a := range s.Assets {
			// 如果这台机器 30 秒内已经有新数据，说明它接了真实 agent，
			// 演示数据必须让路，否则真值会被模拟值污染，监控就失去意义了。
			seq := s.Series[model.SeriesKey("cpu_pct", a.Hostname)]
			if len(seq) > 0 && ts-seq[len(seq)-1].TS < 30 {
				continue
			}
			cpu := 18 + 26*work + rnd.NormFloat64()*4
			if a.Role == "storage" {
				cpu = 12 + 10*work + rnd.NormFloat64()*2
			}
			mem := 42 + 18*work + rnd.NormFloat64()*3
			if a.Role == "db" {
				mem = 68 + 6*work + rnd.NormFloat64()*2
			}
			disk := 30 + rnd.Float64()*6
			if a.Role == "storage" {
				disk = 86.4 + rnd.Float64()*0.3
			}
			bw := 0.9 + 2.4*work + rnd.Float64()*0.6
			if bw > 4.8 {
				bw = 4.8
			}
			monitor.Ingest(s, a.Hostname, map[string]float64{
				"cpu_pct":        round2(cpu),
				"mem_used_pct":   round2(mem),
				"disk_used_pct":  round2(disk),
				"bw_mbps":        round2(bw),
				"load1_per_core": round2(cpu / 100 * 1.6),
			}, ts)
		}
	})
}

func timeNow() time.Time { return time.Now() }

// timeToday 返回本地时区的今天 00:00，用于按自然日生成容量历史。
func timeToday() time.Time {
	n := time.Now()
	return time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, n.Location())
}
