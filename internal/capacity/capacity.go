// Package capacity 负责容量规划。
//
// 用的是最小二乘线性回归，而不是"拿最近 7 天平均增长外推"。
// 原因：素材库的增长有明显的项目周期性（大项目集中导入后进入剪辑期），
// 7 天均值会被最近一次导入带偏；回归至少能给出 R²，
// 让运维判断"这条预测到底可不可信"—— 这正是本模块对外暴露 R² 字段的意义。
package capacity

import (
	"math"
	"sort"
	"time"

	"github.com/Ricevire/stormops/internal/model"
)

const maxDaysLeft = 3650

// Forecast 基于历史点预测某个存储卷的耗尽时间。
func Forecast(volume string, pts []model.CapPoint, capacity, warnPct, critPct float64) model.CapacityForecast {
	fc := model.CapacityForecast{Volume: volume, Capacity: capacity}
	if capacity > 0 {
		fc.UsedPct = 0
	}
	if len(pts) == 0 {
		fc.Status = "ok"
		return fc
	}

	sorted := make([]model.CapPoint, len(pts))
	copy(sorted, pts)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Date < sorted[j].Date })

	base, err := time.Parse("2006-01-02", sorted[0].Date)
	if err != nil {
		base = time.Now()
	}

	n := float64(len(sorted))
	var sx, sy, sxy, sxx float64
	xs := make([]float64, len(sorted))
	ys := make([]float64, len(sorted))
	for i, p := range sorted {
		t, err := time.Parse("2006-01-02", p.Date)
		if err != nil {
			t = base
		}
		x := t.Sub(base).Hours() / 24
		xs[i] = x
		ys[i] = p.Bytes
		sx += x
		sy += p.Bytes
		sxy += x * p.Bytes
		sxx += x * x
	}
	denom := n*sxx - sx*sx
	var slope, intercept float64
	if math.Abs(denom) < 1e-9 {
		slope = 0
		intercept = sy / n
	} else {
		slope = (n*sxy - sx*sy) / denom
		intercept = (sy - slope*sx) / n
	}

	// R²：残差平方和 / 总平方和
	meanY := sy / n
	var ssRes, ssTot float64
	for i := range xs {
		pred := intercept + slope*xs[i]
		ssRes += (ys[i] - pred) * (ys[i] - pred)
		ssTot += (ys[i] - meanY) * (ys[i] - meanY)
	}
	if ssTot > 1e-9 {
		fc.R2 = 1 - ssRes/ssTot
	}

	current := sorted[len(sorted)-1].Bytes
	fc.Current = current
	fc.GrowthPerDay = slope
	fc.History = sorted

	if capacity > 0 {
		fc.UsedPct = current / capacity * 100
	}

	switch {
	case slope <= 0:
		fc.DaysLeft = maxDaysLeft // 不增长甚至下降，不构成容量风险
	case capacity <= 0:
		fc.DaysLeft = -1
	default:
		d := (capacity - current) / slope
		if d < 0 {
			d = 0
		}
		if d > maxDaysLeft {
			d = maxDaysLeft
		}
		fc.DaysLeft = int(d)
	}

	lastDate, err := time.Parse("2006-01-02", sorted[len(sorted)-1].Date)
	if err != nil {
		lastDate = time.Now()
	}
	if fc.DaysLeft >= 0 {
		fc.RunwayDate = lastDate.AddDate(0, 0, fc.DaysLeft).Format("2006-01-02")
	} else {
		fc.RunwayDate = "未设上限"
	}

	// 未来 30 天预测折线
	if slope > 0 {
		for i := 1; i <= 30; i++ {
			fc.Trend = append(fc.Trend, model.CapPoint{
				Date:  lastDate.AddDate(0, 0, i).Format("2006-01-02"),
				Bytes: current + slope*float64(i),
			})
		}
		fc.Projected30 = current + slope*30
	} else {
		fc.Projected30 = current
	}

	fc.Status = classify(fc, warnPct, critPct)
	return fc
}

func classify(fc model.CapacityForecast, warnPct, critPct float64) string {
	if warnPct <= 0 {
		warnPct = 75
	}
	if critPct <= 0 {
		critPct = 90
	}
	// 双重判据：既看水位，也看剩余天数。
	// 只看到期天数会漏掉"水位已经 95% 但刚好不增长了"的卷；
	// 只看水位会漏掉"才 60% 但每天涨 2%"的卷。
	if fc.UsedPct >= critPct || (fc.DaysLeft >= 0 && fc.DaysLeft < 14) {
		return "critical"
	}
	if fc.UsedPct >= warnPct || (fc.DaysLeft >= 0 && fc.DaysLeft < 45) {
		return "warn"
	}
	return "ok"
}

// Bandwidth 汇总带宽指标，返回 P95 / 峰值 / 均值（Mbps）。
func Bandwidth(seq []model.Sample) (p95, peak, avg float64, samples int) {
	st := model.ComputeStats(seq)
	return st.P95, st.Max, st.Avg, st.Count
}
