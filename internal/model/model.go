// Package model 定义 StormOps 的领域对象。
//
// 所有时间戳统一使用 Unix 秒（UTC），前端负责按浏览器时区渲染。
// 这样做的原因：跨时区排障时，"服务器 UTC 时间" 和 "日志时间" 必须能直接对齐，
// 一旦在存储层混入本地时区，跨时区事故复盘几乎必然出现时间线错乱。
package model

import (
	"fmt"
	"math"
	"sort"
	"strings"
)

// Asset 是 CMDB 中纳管的一台机器。
type Asset struct {
	ID       string   `json:"id"`
	Hostname string   `json:"hostname"`
	IP       string   `json:"ip"`
	Role     string   `json:"role"` // edge / web / db / storage / worker
	Zone     string   `json:"zone"` // prod / dmz / office
	OS       string   `json:"os"`
	CPUCores int      `json:"cpuCores"`
	MemGB    int      `json:"memGb"`
	DiskGB   int      `json:"diskGb"`
	Tags     []string `json:"tags"`
	Status   string   `json:"status"` // online / degraded / offline
	LastSeen int64    `json:"lastSeen"`
}

// Service 是网关后面的一个业务服务。
type Service struct {
	Name      string   `json:"name"`
	Domain    string   `json:"domain"`
	Upstreams []string `json:"upstreams"`
	// Prefix 是这个服务在网关上的挂载路径（如 "/" 或 "/ops/"）。
	// 必须显式声明：早期版本靠"把所有 upstream 合并成一个池子"来省事，
	// 结果 Nginx 轮询到了错误的应用程序上，见 PM-004。
	Prefix string `json:"prefix,omitempty"`
	// SkipWAF 让这条路由绕过 auth_request。控制台必须绕过：
	// WAF 规则写错时，运维得还有一条远程自救的路。
	SkipWAF    bool   `json:"skipWaf,omitempty"`
	HealthPath string `json:"healthPath"`
	Owner      string `json:"owner"`
	Env        string `json:"env"`
	Version    string `json:"version"`
	UpdatedAt  int64  `json:"updatedAt"`
}

// Sample 是指标的一个采样点。
type Sample struct {
	TS    int64   `json:"ts"`
	Value float64 `json:"value"`
}

// AlertRule 是告警规则。expr 被拆成 metric / op / threshold 三段，
// 而不是让用户写 PromQL —— 原因见 docs/decisions/ADR-002。
type AlertRule struct {
	ID        string  `json:"id"`
	Name      string  `json:"name"`
	Metric    string  `json:"metric"`
	Op        string  `json:"op"`
	Threshold float64 `json:"threshold"`
	ForSec    int     `json:"forSec"`
	Severity  string  `json:"severity"` // critical / warning / info
	Summary   string  `json:"summary"`
	Runbook   string  `json:"runbook"`
	Enabled   bool    `json:"enabled"`
}

// Alert 是一条实际的告警实例。
type Alert struct {
	ID        string  `json:"id"`
	RuleID    string  `json:"ruleId"`
	RuleName  string  `json:"ruleName"`
	Host      string  `json:"host"`
	Metric    string  `json:"metric"`
	Severity  string  `json:"severity"`
	Value     float64 `json:"value"`
	Threshold float64 `json:"threshold"`
	Summary   string  `json:"summary"`
	Runbook   string  `json:"runbook"`
	Status    string  `json:"status"` // firing / acked / resolved
	StartedAt int64   `json:"startedAt"`
	UpdatedAt int64   `json:"updatedAt"`
	AckedBy   string  `json:"ackedBy"`
	Count     int     `json:"count"`
}

// Silence 是一条告警静默。Host / Rule 支持 "*" 通配。
type Silence struct {
	ID      string `json:"id"`
	Host    string `json:"host"`
	Rule    string `json:"rule"`
	Reason  string `json:"reason"`
	By      string `json:"by"`
	Until   int64  `json:"until"`
	Created int64  `json:"created"`
}

// Matches 判断静默是否覆盖某条告警。
func (s Silence) Matches(host, ruleName string, now int64) bool {
	if s.Until <= now {
		return false
	}
	if s.Host != "*" && s.Host != host {
		return false
	}
	if s.Rule != "*" && s.Rule != ruleName {
		return false
	}
	return true
}

// ReleaseStep 是发布流程中的一个步骤。
type ReleaseStep struct {
	Name      string `json:"name"`
	Status    string `json:"status"` // pending / running / success / failed / skipped
	Detail    string `json:"detail"`
	StartedAt int64  `json:"startedAt"`
	EndedAt   int64  `json:"endedAt"`
}

// Release 是一次发布记录。
type Release struct {
	ID          string        `json:"id"`
	Service     string        `json:"service"`
	Version     string        `json:"version"`
	PrevVersion string        `json:"prevVersion"`
	Artifact    string        `json:"artifact"`
	Strategy    string        `json:"strategy"` // rolling / canary
	Status      string        `json:"status"`   // running / success / failed / rolled_back
	Steps       []ReleaseStep `json:"steps"`
	Operator    string        `json:"operator"`
	StartedAt   int64         `json:"startedAt"`
	EndedAt     int64         `json:"endedAt"`
	Note        string        `json:"note"`
}

// WAFRule 是一条 WAF 规则。
//
// RolloutPct 实现灰度：只有 hash(ip+ruleID)%100 < RolloutPct 的流量才会被这条规则判定，
// 这是把"规则上线"从一次全量赌博变成可回退的分批动作的关键。
type WAFRule struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Category   string `json:"category"` // sqli / xss / rce / scanner / ratelimit / custom
	Target     string `json:"target"`   // uri / args / headers / body
	Pattern    string `json:"pattern"`
	Action     string `json:"action"` // block / log
	Severity   string `json:"severity"`
	Enabled    bool   `json:"enabled"`
	RolloutPct int    `json:"rolloutPct"`
	Hits       int    `json:"hits"`
	UpdatedAt  int64  `json:"updatedAt"`
}

// AttackEvent 是一次 WAF 命中事件。
type AttackEvent struct {
	TS       int64  `json:"ts"`
	IP       string `json:"ip"`
	Country  string `json:"country"`
	Method   string `json:"method"`
	Path     string `json:"path"`
	RuleID   string `json:"ruleId"`
	RuleName string `json:"ruleName"`
	Category string `json:"category"`
	Action   string `json:"action"`
	UA       string `json:"ua"`
}

// CapPoint 是容量历史中的一个点。
type CapPoint struct {
	Date  string  `json:"date"` // 2006-01-02
	Bytes float64 `json:"bytes"`
}

// CapacityForecast 是容量预测结果。
type CapacityForecast struct {
	Volume       string     `json:"volume"`
	Current      float64    `json:"current"`
	Capacity     float64    `json:"capacity"`
	GrowthPerDay float64    `json:"growthPerDay"`
	DaysLeft     int        `json:"daysLeft"`
	RunwayDate   string     `json:"runwayDate"`
	Projected30  float64    `json:"projected30"`
	History      []CapPoint `json:"history"`
	Trend        []CapPoint `json:"trend"`
	R2           float64    `json:"r2"`
	Status       string     `json:"status"` // ok / warn / critical
	UsedPct      float64    `json:"usedPct"`
}

// State 是 StormOps 的全量内存状态。
//
// 单进程 + 一把读写锁 + 定期落盘。这个选择在 150 人规模的公司里足够，
// 因为写入路径只有 agent 上报（每 15s 一次）和人工操作（每天个位数）。
type State struct {
	Assets    []Asset               `json:"assets"`
	Services  []Service             `json:"services"`
	Series    map[string][]Sample   `json:"series"`
	Rules     []AlertRule           `json:"rules"`
	Alerts    []Alert               `json:"alerts"`
	Silences  []Silence             `json:"silences"`
	Releases  []Release             `json:"releases"`
	WAFRules  []WAFRule             `json:"wafRules"`
	Attacks   []AttackEvent         `json:"attacks"`
	Capacity  map[string][]CapPoint `json:"capacity"`
	Meta      map[string]string     `json:"meta"`
	UpdatedAt int64                 `json:"updatedAt"`
}

// SeriesKey 生成指标序列的键。
func SeriesKey(metric, host string) string {
	return metric + "|" + host
}

// SplitSeriesKey 拆分指标序列键。
func SplitSeriesKey(k string) (metric, host string) {
	i := strings.Index(k, "|")
	if i < 0 {
		return k, ""
	}
	return k[:i], k[i+1:]
}

// Stats 是序列的统计摘要。
type Stats struct {
	Last  float64 `json:"last"`
	Min   float64 `json:"min"`
	Max   float64 `json:"max"`
	Avg   float64 `json:"avg"`
	P95   float64 `json:"p95"`
	Count int     `json:"count"`
}

// ComputeStats 计算序列统计。P95 用最近秩法（nearest-rank），
// 样本量小的时候比分位数插值更不容易产生"看似精确其实无意义"的数字。
func ComputeStats(points []Sample) Stats {
	if len(points) == 0 {
		return Stats{}
	}
	vals := make([]float64, 0, len(points))
	sum := 0.0
	mn, mx := math.Inf(1), math.Inf(-1)
	for _, p := range points {
		vals = append(vals, p.Value)
		sum += p.Value
		if p.Value < mn {
			mn = p.Value
		}
		if p.Value > mx {
			mx = p.Value
		}
	}
	sort.Float64s(vals)
	idx := int(math.Ceil(0.95*float64(len(vals)))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(vals) {
		idx = len(vals) - 1
	}
	return Stats{
		Last:  points[len(points)-1].Value,
		Min:   mn,
		Max:   mx,
		Avg:   sum / float64(len(vals)),
		P95:   vals[idx],
		Count: len(vals),
	}
}

// Compare 按 op 比较当前值与阈值。
func Compare(v float64, op string, threshold float64) bool {
	switch op {
	case ">":
		return v > threshold
	case ">=":
		return v >= threshold
	case "<":
		return v < threshold
	case "<=":
		return v <= threshold
	}
	return false
}

// HumanBytes 把字节数格式化成人类可读形式。
func HumanBytes(b float64) string {
	const unit = 1024.0
	units := []string{"B", "KB", "MB", "GB", "TB", "PB"}
	v := b
	i := 0
	for v >= unit && i < len(units)-1 {
		v /= unit
		i++
	}
	if i == 0 {
		return fmt.Sprintf("%.0f %s", v, units[i])
	}
	return fmt.Sprintf("%.1f %s", v, units[i])
}
