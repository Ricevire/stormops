// Package waf 实现 StormOps 的 Web 应用防火墙判定与审计日志解析。
//
// 为什么不用 ModSecurity：
//  1. Nginx 上编译 ModSecurity 需要单独编译连接器，升级 Nginx 就得重编，
//     对一台 2C2G 的边缘机来说维护成本过高；
//  2. 我们的场景（官网 + 课程站 + 电商落地页）流量不高，但规则需要频繁调整，
//     自研检测服务能让"改规则"变成一次 API 调用，而不是改文件 + reload。
//
// 代价：没有 ModSecurity 的 OWASP CRS 全套规则和成熟的 anomaly scoring。
// 因此本模块同时提供 Compile()，把规则导出成标准 SecRule 语法，
// 需要时可以无缝迁回 ModSecurity（见 docs/decisions/ADR-004）。
package waf

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Ricevire/stormops/internal/model"
)

// Request 是待判定的请求元信息。注意不含请求体 —— 见 gateway 包里的说明。
type Request struct {
	IP      string            `json:"ip"`
	Method  string            `json:"method"`
	URI     string            `json:"uri"`
	UA      string            `json:"ua"`
	Body    string            `json:"body"`
	Headers map[string]string `json:"headers"`
}

// Decision 是判定结果。
type Decision struct {
	Blocked  bool   `json:"blocked"`
	RuleID   string `json:"ruleId"`
	RuleName string `json:"ruleName"`
	Category string `json:"category"`
	Action   string `json:"action"`
	Reason   string `json:"reason"`
}

var reCache sync.Map

func getRe(r model.WAFRule) (*regexp.Regexp, error) {
	if v, ok := reCache.Load(r.ID + "|" + r.Pattern); ok {
		return v.(*regexp.Regexp), nil
	}
	// 加 (?i) 让规则不区分大小写：攻击者很容易靠大小写绕过，
	// 而我们的规则库没有理由要求运维每条都记得写 (?i)。
	re, err := regexp.Compile("(?i)" + r.Pattern)
	if err != nil {
		return nil, err
	}
	reCache.Store(r.ID+"|"+r.Pattern, re)
	return re, nil
}

func subjectFor(target string, req Request) string {
	switch target {
	case "uri":
		return req.Method + " " + req.URI
	case "args":
		if i := strings.Index(req.URI, "?"); i >= 0 {
			return req.URI[i+1:]
		}
		return ""
	case "headers":
		var b strings.Builder
		for k, v := range req.Headers {
			b.WriteString(k)
			b.WriteString(": ")
			b.WriteString(v)
			b.WriteString("\n")
		}
		b.WriteString("User-Agent: ")
		b.WriteString(req.UA)
		return b.String()
	case "body":
		return req.Body
	}
	return req.URI
}

// normalize 做匹配前的归一化：百分号解码 + 加号转空格。
//
// 这一步是 WAF 能不能拦住真实攻击的分水岭。
// 如果直接拿 nginx 传来的 $request_uri 去匹配，看到的是
// "/?id=1%27%20OR%20%271%27%3D%271"，而规则里写的是 "or 1=1"，
// 所有针对参数的规则都会失效 —— 但攻击者发送的实际语义确实是注入。
//
// 解两层是因为双重编码（%2527 -> %27 -> '）是最常见的绕过手法之一。
func normalize(s string) string {
	for i := 0; i < 2; i++ {
		d, err := url.QueryUnescape(s)
		if err != nil || d == s {
			break
		}
		s = d
	}
	return s
}

// inRollout 判断某 IP 是否落在规则的灰度范围内。
//
// 用 hash(ip|ruleID) 而不是随机数，保证同一个客户端在规则灰度期间判定稳定 ——
// 否则攻击者只要重试几次就能穿过 30% 灰度的规则，灰度就失去意义了。
func inRollout(r model.WAFRule, ip string) bool {
	if r.RolloutPct >= 100 {
		return true
	}
	if r.RolloutPct <= 0 {
		return false
	}
	var h uint32 = 2166136261
	for _, c := range []byte(ip + "|" + r.ID) {
		h ^= uint32(c)
		h *= 16777619
	}
	return int(h%100) < r.RolloutPct
}

// Check 对单个请求做规则判定。
//
// 判定顺序：block 类规则优先，同类按规则 ID 排序保证结果可复现。
// 命中 log 类规则时仍然放行，但会记录事件（用于规则上线前的观察期）。
func Check(rules []model.WAFRule, req Request, now int64) Decision {
	ordered := make([]model.WAFRule, 0, len(rules))
	for _, r := range rules {
		if r.Enabled {
			ordered = append(ordered, r)
		}
	}
	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].Action != ordered[j].Action {
			return ordered[i].Action == "block"
		}
		return ordered[i].ID < ordered[j].ID
	})

	var logHit *model.WAFRule
	for i := range ordered {
		r := ordered[i]
		if !inRollout(r, req.IP) {
			continue
		}
		re, err := getRe(r)
		if err != nil {
			continue
		}
		if re.MatchString(normalize(subjectFor(r.Target, req))) {
			if r.Action == "block" {
				return Decision{
					Blocked:  true,
					RuleID:   r.ID,
					RuleName: r.Name,
					Category: r.Category,
					Action:   r.Action,
					Reason:   "命中规则 " + r.Name + "（" + r.Category + "）",
				}
			}
			if logHit == nil {
				cp := r
				logHit = &cp
			}
		}
	}
	if logHit != nil {
		return Decision{RuleID: logHit.ID, RuleName: logHit.Name, Category: logHit.Category, Action: "log", Reason: "观察模式命中"}
	}
	return Decision{}
}

// Compile 把规则导出为 ModSecurity SecRule 语法，用于迁移或人工 review。
func Compile(rules []model.WAFRule) string {
	var b strings.Builder
	b.WriteString("# 由 StormOps 生成的标准 ModSecurity 规则（可直接放入 modsecurity.d/*.conf）\n")
	b.WriteString("# 生成时间: " + time.Now().Format(time.RFC3339) + "\n\n")
	for _, r := range rules {
		target := "REQUEST_URI"
		switch r.Target {
		case "args":
			target = "ARGS"
		case "headers":
			target = "REQUEST_HEADERS"
		case "body":
			target = "REQUEST_BODY"
		}
		action := "deny,status:403"
		if r.Action != "block" {
			action = "pass"
		}
		if !r.Enabled {
			action = "pass"
		}
		sev := strings.ToUpper(r.Severity)
		switch sev {
		case "CRITICAL", "WARNING", "INFO", "ERROR", "NOTICE":
		default:
			sev = "WARNING"
		}
		fmt.Fprintf(&b,
			"SecRule %s \"@rx %s\" \"id:%s,phase:2,%s,log,msg:'%s',severity:'%s',tag:'stormops/%s'\"\n",
			target, escapeRule(r.Pattern), numericID(r.ID), action, r.Name, sev, r.Category)
	}
	return b.String()
}

func escapeRule(p string) string {
	// SecRule 的参数里双引号和反斜杠必须转义
	p = strings.ReplaceAll(p, `\`, `\\`)
	return strings.ReplaceAll(p, `"`, `\"`)
}

func numericID(id string) string {
	n := 0
	for _, c := range id {
		if c >= '0' && c <= '9' {
			n = n*10 + int(c-'0')
		}
	}
	if n == 0 {
		n = 900001
	}
	return fmt.Sprintf("9%05d", n%100000)
}

// ParseAuditLog 解析攻击日志。
//
// 支持两种格式：
//  1. ModSecurity 原生审计日志（---xxx---A-- / ---xxx---B-- 分段）；
//  2. StormOps 自己输出的 JSONL（每行一条 AttackEvent）。
//
// 之所以要兼容 ModSecurity 格式，是为了让"以后换回 ModSecurity"
// 或者"临时用 modsec 审计日志做对照"这两件事不需要重写解析器。
func ParseAuditLog(r io.Reader) ([]model.AttackEvent, error) {
	var events []model.AttackEvent
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var cur model.AttackEvent
	inA, inB := false, false
	rules := map[string]string{}

	for sc.Scan() {
		line := sc.Text()
		trimmed := strings.TrimSpace(line)

		if strings.HasPrefix(trimmed, "---") && strings.HasSuffix(trimmed, "--") {
			marker := strings.Trim(trimmed, "-")
			switch marker {
			case "A":
				inA, inB = true, false
			case "B":
				inA, inB = false, true
			default:
				inA, inB = false, false
				if cur.IP != "" {
					events = append(events, cur)
					cur = model.AttackEvent{}
				}
			}
			continue
		}

		if strings.HasPrefix(trimmed, "{") {
			var ev model.AttackEvent
			if err := json.Unmarshal([]byte(trimmed), &ev); err == nil {
				events = append(events, ev)
			}
			continue
		}

		if inA {
			// [23/Nov/2026:13:45:01 +0800] 1.2.3.4 12345 10.0.0.1 80
			if t, err := time.Parse("[02/Jan/2006:15:04:05 -0700]", firstField(trimmed)); err == nil {
				cur.TS = t.Unix()
			}
			f := strings.Fields(trimmed)
			if len(f) >= 2 {
				cur.IP = f[1]
			}
			continue
		}

		if inB {
			if strings.HasPrefix(trimmed, "User-Agent:") {
				cur.UA = strings.TrimSpace(strings.TrimPrefix(trimmed, "User-Agent:"))
				continue
			}
			if cur.Method == "" {
				f := strings.Fields(trimmed)
				if len(f) >= 2 {
					cur.Method = f[0]
					cur.Path = f[1]
				}
			}
			continue
		}

		if strings.Contains(trimmed, "[id \"") {
			// [id "942100"] [msg "SQL Injection Attack Detected"]
			rules[trimmed] = ""
		}
	}
	if err := sc.Err(); err != nil {
		return events, err
	}
	if cur.IP != "" {
		events = append(events, cur)
	}
	return events, nil
}

func firstField(s string) string {
	if i := strings.Index(s, "]"); i > 0 {
		return s[:i+1]
	}
	return s
}

// TopAttackers 统计攻击次数最多的 IP。
type Attacker struct {
	IP    string `json:"ip"`
	Count int    `json:"count"`
	Last  int64  `json:"last"`
}

// TopAttackers 返回攻击来源 TopN。
func TopAttackers(events []model.AttackEvent, n int) []Attacker {
	m := map[string]*Attacker{}
	for _, e := range events {
		a, ok := m[e.IP]
		if !ok {
			a = &Attacker{IP: e.IP}
			m[e.IP] = a
		}
		a.Count++
		if e.TS > a.Last {
			a.Last = e.TS
		}
	}
	out := make([]Attacker, 0, len(m))
	for _, a := range m {
		out = append(out, *a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Count > out[j].Count })
	if len(out) > n {
		out = out[:n]
	}
	return out
}

// CategoryStat 是按攻击类型聚合的统计。
type CategoryStat struct {
	Category string `json:"category"`
	Count    int    `json:"count"`
	Blocked  int    `json:"blocked"`
}

// ByCategory 按攻击类型聚合。
func ByCategory(events []model.AttackEvent) []CategoryStat {
	m := map[string]*CategoryStat{}
	for _, e := range events {
		c, ok := m[e.Category]
		if !ok {
			c = &CategoryStat{Category: e.Category}
			m[e.Category] = c
		}
		c.Count++
		if e.Action == "block" {
			c.Blocked++
		}
	}
	out := make([]CategoryStat, 0, len(m))
	for _, c := range m {
		out = append(out, *c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Count > out[j].Count })
	return out
}
