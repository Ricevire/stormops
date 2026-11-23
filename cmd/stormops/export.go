package main

import (
	"encoding/csv"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/Ricevire/stormops/internal/model"
	"github.com/Ricevire/stormops/internal/store"
)

// exportState 把内存状态导出成 CSV，供 PostgreSQL 分析库加载。
//
// 为什么不直接让 stormops 连 PG：
//  1. 纯标准库无法在不引入第三方 driver 的前提下连 PG，
//     而这个项目刻意保持零依赖（见 README 的取舍说明）；
//  2. 更重要的是架构上不该让在线路径依赖分析库 ——
//     分析库做一次大查询把 PG 拖慢，不该连带控制台一起卡住。
//
// 用 CSV 做交接面还有个好处：出问题时可以直接用 Excel 打开对账，
// 不需要先装一个客户端。
func exportState(st *store.Store, dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}

	tables := map[string]struct {
		header []string
		rows   func(*model.State) [][]string
	}{
		"assets": {
			header: []string{"id", "hostname", "ip", "role", "zone", "os", "cpu_cores", "mem_gb", "disk_gb", "status", "last_seen"},
			rows: func(s *model.State) [][]string {
				out := make([][]string, 0, len(s.Assets))
				for _, a := range s.Assets {
					out = append(out, []string{
						a.ID, a.Hostname, a.IP, a.Role, a.Zone, a.OS,
						strconv.Itoa(a.CPUCores), strconv.Itoa(a.MemGB), strconv.Itoa(a.DiskGB),
						a.Status, iso(a.LastSeen),
					})
				}
				return out
			},
		},
		"services": {
			header: []string{"name", "domain", "env", "version", "owner", "health_path", "updated_at"},
			rows: func(s *model.State) [][]string {
				out := make([][]string, 0, len(s.Services))
				for _, v := range s.Services {
					out = append(out, []string{v.Name, v.Domain, v.Env, v.Version, v.Owner, v.HealthPath, iso(v.UpdatedAt)})
				}
				return out
			},
		},
		"releases": {
			header: []string{"id", "service", "version", "prev_version", "strategy", "status", "operator", "started_at", "ended_at", "duration_sec"},
			rows: func(s *model.State) [][]string {
				out := make([][]string, 0, len(s.Releases))
				for _, r := range s.Releases {
					dur := int64(0)
					if r.EndedAt > 0 {
						dur = r.EndedAt - r.StartedAt
					}
					out = append(out, []string{
						r.ID, r.Service, r.Version, r.PrevVersion, r.Strategy, r.Status,
						r.Operator, iso(r.StartedAt), iso(r.EndedAt),
						strconv.FormatInt(dur, 10),
					})
				}
				return out
			},
		},
		"waf_audit": {
			header: []string{"ts", "ip", "country", "method", "path", "rule_id", "rule_name", "category", "action", "ua"},
			rows: func(s *model.State) [][]string {
				out := make([][]string, 0, len(s.Attacks))
				for _, e := range s.Attacks {
					out = append(out, []string{
						iso(e.TS), e.IP, e.Country, e.Method, e.Path,
						e.RuleID, e.RuleName, e.Category, e.Action, e.UA,
					})
				}
				return out
			},
		},
		"capacity": {
			header: []string{"volume", "day", "bytes"},
			rows: func(s *model.State) [][]string {
				var out [][]string
				for vol, pts := range s.Capacity {
					for _, p := range pts {
						out = append(out, []string{vol, p.Date, strconv.FormatFloat(p.Bytes, 'f', 0, 64)})
					}
				}
				return out
			},
		},
	}

	for name, t := range tables {
		var rows [][]string
		st.View(func(s *model.State) { rows = t.rows(s) })

		f, err := os.Create(filepath.Join(dir, name+".csv"))
		if err != nil {
			return fmt.Errorf("创建 %s.csv 失败: %w", name, err)
		}
		w := csv.NewWriter(f)
		if err := w.Write(t.header); err != nil {
			f.Close()
			return err
		}
		for _, r := range rows {
			if err := w.Write(r); err != nil {
				f.Close()
				return err
			}
		}
		w.Flush()
		if err := w.Error(); err != nil {
			f.Close()
			return err
		}
		f.Close()
		fmt.Printf("导出 %s.csv：%d 行\n", name, len(rows))
	}
	return nil
}

// iso 把 Unix 秒转成 RFC3339。空值输出空串，
// 让 PostgreSQL 的 \copy 把空字段当成 NULL，而不是 1970-01-01。
func iso(ts int64) string {
	if ts <= 0 {
		return ""
	}
	return time.Unix(ts, 0).UTC().Format(time.RFC3339)
}
