// Package agent 实现节点侧指标采集。
//
// 全部指标来自 /proc 和系统调用级别的信息源，没有任何"模拟值"：
//   - CPU 利用率：/proc/stat 两次采样的 jiffies 差值
//   - 内存占用：/proc/meminfo 的 MemTotal - MemAvailable
//   - 磁盘水位：df -P
//   - 网络吞吐：/proc/net/dev 计数器差值
//   - 负载：/proc/loadavg
//
// 之所以不用 gopsutil 之类的库：这类库为了跨平台引入了大量依赖，
// 而我们的 agent 只需要跑在 Linux 上，直接读 /proc 反而更小、更快、更好排查。
package agent

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Probe 是一个服务存活探测目标。
type Probe struct {
	Name    string `json:"name"`
	URL     string `json:"url"`     // http(s) 或 host:port（TCP）
	Timeout int    `json:"timeout"` // 毫秒
}

// Report 是一次上报的数据体。
type Report struct {
	Host     string             `json:"host"`
	TS       int64              `json:"ts"`
	Samples  map[string]float64 `json:"samples"`
	Services []ServiceProbe     `json:"services"`
	Labels   map[string]string  `json:"labels"`
	AgentVer string             `json:"agentVer"`
}

// ServiceProbe 是单个服务的探测结果。
type ServiceProbe struct {
	Name      string  `json:"name"`
	Up        int     `json:"up"`
	LatencyMs float64 `json:"latencyMs"`
	Code      int     `json:"code"`
}

// Collector 负责采集本机指标。
type Collector struct {
	mu       sync.Mutex
	prevCPU  cpuTimes
	prevNet  netCounters
	prevNetT time.Time
	prevCPUT time.Time
}

type cpuTimes struct{ idle, total float64 }

type netCounters struct{ rx, tx float64 }

// Collect 采集一次全部指标。
func (c *Collector) Collect(probes []Probe) Report {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := time.Now()
	samples := map[string]float64{}

	if v, ok := c.cpuPercent(now); ok {
		samples["cpu_pct"] = round2(v)
	}
	if v, ok := memUsedPercent(); ok {
		samples["mem_used_pct"] = round2(v)
		samples["mem_avail_mb"] = round2(memAvailableMB())
	}
	if used, total, ok := diskUsage("/"); ok {
		samples["disk_used_bytes"] = used
		samples["disk_total_bytes"] = total
		if total > 0 {
			samples["disk_used_pct"] = round2(used / total * 100)
		}
	}
	if rx, tx, ok := c.netRate(now); ok {
		samples["net_rx_mbps"] = round2(rx)
		samples["net_tx_mbps"] = round2(tx)
		samples["bw_mbps"] = round2(rx + tx)
	}
	if v, ok := load1(); ok {
		samples["load1"] = round2(v)
		if n := cpuCount(); n > 0 {
			samples["load1_per_core"] = round2(v / float64(n))
		}
	}
	if v, ok := fileDescriptorsUsed(); ok {
		samples["fd_used_pct"] = round2(v)
	}

	host, _ := os.Hostname()
	rep := Report{
		Host:     host,
		TS:       now.Unix(),
		Samples:  samples,
		Services: runProbes(probes),
		AgentVer: "1.0.0",
	}
	return rep
}

func (c *Collector) cpuPercent(now time.Time) (float64, bool) {
	cur, ok := readCPUTimes()
	if !ok {
		return 0, false
	}
	if c.prevCPUT.IsZero() || now.Sub(c.prevCPUT) < 500*time.Millisecond {
		c.prevCPU = cur
		c.prevCPUT = now
		return 0, false
	}
	dTotal := cur.total - c.prevCPU.total
	dIdle := cur.idle - c.prevCPU.idle
	c.prevCPU = cur
	c.prevCPUT = now
	if dTotal <= 0 {
		return 0, false
	}
	return (dTotal - dIdle) / dTotal * 100, true
}

func readCPUTimes() (cpuTimes, bool) {
	f, err := os.Open("/proc/stat")
	if err != nil {
		return cpuTimes{}, false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "cpu ") {
			continue
		}
		fields := strings.Fields(line)[1:]
		var total, idle float64
		for i, v := range fields {
			n, _ := strconv.ParseFloat(v, 64)
			total += n
			// idle + iowait 都算"非忙"
			if i == 3 || i == 4 {
				idle += n
			}
		}
		return cpuTimes{idle: idle, total: total}, true
	}
	return cpuTimes{}, false
}

func (c *Collector) netRate(now time.Time) (rxMbps, txMbps float64, ok bool) {
	cur, ok := readNetCounters()
	if !ok {
		return 0, 0, false
	}
	if c.prevNetT.IsZero() || now.Sub(c.prevNetT) < 500*time.Millisecond {
		c.prevNet = cur
		c.prevNetT = now
		return 0, 0, false
	}
	dt := now.Sub(c.prevNetT).Seconds()
	drx := (cur.rx - c.prevNet.rx) * 8 / 1e6
	dtx := (cur.tx - c.prevNet.tx) * 8 / 1e6
	c.prevNet = cur
	c.prevNetT = now
	if dt <= 0 || drx < 0 || dtx < 0 {
		// 计数器回绕（网卡重启 / 容器重建）时直接返回 0，避免出现天文数字
		return 0, 0, false
	}
	return drx / dt, dtx / dt, true
}

func readNetCounters() (netCounters, bool) {
	f, err := os.Open("/proc/net/dev")
	if err != nil {
		return netCounters{}, false
	}
	defer f.Close()
	var c netCounters
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		i := strings.Index(line, ":")
		if i < 0 {
			continue
		}
		iface := strings.TrimSpace(line[:i])
		if iface == "lo" || strings.HasPrefix(iface, "docker") || strings.HasPrefix(iface, "veth") {
			continue
		}
		fields := strings.Fields(line[i+1:])
		if len(fields) < 9 {
			continue
		}
		rx, _ := strconv.ParseFloat(fields[0], 64)
		tx, _ := strconv.ParseFloat(fields[8], 64)
		c.rx += rx
		c.tx += tx
	}
	return c, true
}

func memUsedPercent() (float64, bool) {
	vals, ok := readMemInfo()
	if !ok {
		return 0, false
	}
	total := vals["MemTotal"]
	avail := vals["MemAvailable"]
	if total <= 0 {
		return 0, false
	}
	return (total - avail) / total * 100, true
}

func memAvailableMB() float64 {
	vals, ok := readMemInfo()
	if !ok {
		return 0
	}
	return vals["MemAvailable"] / 1024
}

func readMemInfo() (map[string]float64, bool) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return nil, false
	}
	defer f.Close()
	out := map[string]float64{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		parts := strings.Fields(sc.Text())
		if len(parts) < 2 {
			continue
		}
		key := strings.TrimSuffix(parts[0], ":")
		v, err := strconv.ParseFloat(parts[1], 64)
		if err != nil {
			continue
		}
		out[key] = v
	}
	return out, true
}

func diskUsage(path string) (used, total float64, ok bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "df", "-P", "-B1", path).Output()
	if err != nil {
		return 0, 0, false
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) < 2 {
		return 0, 0, false
	}
	f := strings.Fields(lines[len(lines)-1])
	if len(f) < 4 {
		return 0, 0, false
	}
	total, _ = strconv.ParseFloat(f[1], 64)
	avail, _ := strconv.ParseFloat(f[3], 64)
	return total - avail, total, true
}

func load1() (float64, bool) {
	b, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return 0, false
	}
	f := strings.Fields(string(b))
	if len(f) == 0 {
		return 0, false
	}
	v, err := strconv.ParseFloat(f[0], 64)
	return v, err == nil
}

func cpuCount() int {
	b, err := os.ReadFile("/proc/cpuinfo")
	if err != nil {
		return 0
	}
	return strings.Count(string(b), "processor\t")
}

func fileDescriptorsUsed() (float64, bool) {
	// 当前进程的 fd 上限压力：用系统级 allocated/file-max 更贴近"整机会不会耗尽"
	b, err := os.ReadFile("/proc/sys/fs/file-nr")
	if err != nil {
		return 0, false
	}
	f := strings.Fields(string(b))
	if len(f) < 3 {
		return 0, false
	}
	alloc, _ := strconv.ParseFloat(f[0], 64)
	limit, _ := strconv.ParseFloat(f[2], 64)
	if limit <= 0 {
		return 0, false
	}
	return alloc / limit * 100, true
}

func runProbes(probes []Probe) []ServiceProbe {
	if len(probes) == 0 {
		return nil
	}
	out := make([]ServiceProbe, len(probes))
	var wg sync.WaitGroup
	for i, p := range probes {
		wg.Add(1)
		go func(i int, p Probe) {
			defer wg.Done()
			out[i] = probe(p)
		}(i, p)
	}
	wg.Wait()
	return out
}

func probe(p Probe) ServiceProbe {
	timeout := time.Duration(p.Timeout) * time.Millisecond
	if timeout <= 0 {
		timeout = 2000 * time.Millisecond
	}
	start := time.Now()
	res := ServiceProbe{Name: p.Name}

	if strings.HasPrefix(p.URL, "http") {
		client := &http.Client{Timeout: timeout}
		resp, err := client.Get(p.URL)
		res.LatencyMs = round2(float64(time.Since(start).Microseconds()) / 1000)
		if err != nil {
			return res
		}
		defer resp.Body.Close()
		res.Code = resp.StatusCode
		if resp.StatusCode >= 200 && resp.StatusCode < 500 {
			res.Up = 1
		}
		return res
	}

	conn, err := net.DialTimeout("tcp", p.URL, timeout)
	res.LatencyMs = round2(float64(time.Since(start).Microseconds()) / 1000)
	if err != nil {
		return res
	}
	_ = conn.Close()
	res.Up = 1
	return res
}

func round2(v float64) float64 {
	return float64(int64(v*100+0.5)) / 100
}

// Module 用于打印版本信息。
func Module() string { return fmt.Sprintf("stormagent %s", "1.0.0") }
