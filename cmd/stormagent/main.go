// Command stormagent 是节点侧采集代理。
//
// 设计成"只推不拉"：agent 主动向 StormOps 上报，StormOps 不主动连 agent。
// 原因：生产网的入站策略通常比出站严格得多，主动拉取需要逐台开放端口，
// 而主动推送只需要 agent 能出网到一台固定的控制面地址。
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/Ricevire/stormops/internal/agent"
)

var version = "dev"

type config struct {
	ServerURL string            `json:"serverUrl"`
	Token     string            `json:"token"`
	Interval  int               `json:"interval"`
	Probes    []agent.Probe     `json:"probes"`
	Labels    map[string]string `json:"labels"`
}

func main() {
	cfgPath := flag.String("config", "/etc/stormops/agent.json", "配置文件路径")
	once := flag.Bool("once", false, "只采集并上报一次，用于排障")
	showVer := flag.Bool("version", false, "打印版本后退出")
	flag.Parse()

	if *showVer {
		fmt.Printf("stormagent %s\n", version)
		return
	}

	b, err := os.ReadFile(*cfgPath)
	if err != nil {
		log.Fatalf("读取配置失败: %v", err)
	}
	var cfg config
	if err := json.Unmarshal(b, &cfg); err != nil {
		log.Fatalf("解析配置失败: %v", err)
	}
	if cfg.ServerURL == "" {
		log.Fatal("serverUrl 不能为空")
	}
	if cfg.Interval <= 0 {
		cfg.Interval = 15
	}

	col := &agent.Collector{}
	client := &http.Client{Timeout: 10 * time.Second}
	url := cfg.ServerURL + "/internal/ingest"

	push := func() {
		rep := col.Collect(cfg.Probes)
		rep.Labels = cfg.Labels
		rep.AgentVer = version
		body, _ := json.Marshal(rep)
		req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			log.Printf("构造请求失败: %v", err)
			return
		}
		req.Header.Set("Content-Type", "application/json")
		if cfg.Token != "" {
			req.Header.Set("X-Agent-Token", cfg.Token)
		}
		resp, err := client.Do(req)
		if err != nil {
			log.Printf("上报失败: %v", err)
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode >= 300 {
			log.Printf("上报被拒绝: HTTP %d", resp.StatusCode)
			return
		}
		log.Printf("上报成功 host=%s 指标=%d 服务=%d", rep.Host, len(rep.Samples), len(rep.Services))
	}

	// 先采一次建立 CPU / 网卡基线（第一次差值没有意义，会返回空）
	_ = col.Collect(cfg.Probes)
	if *once {
		time.Sleep(600 * time.Millisecond)
		push()
		return
	}

	log.Printf("stormagent %s 启动，上报到 %s，间隔 %ds", version, url, cfg.Interval)
	push()
	t := time.NewTicker(time.Duration(cfg.Interval) * time.Second)
	defer t.Stop()
	for range t.C {
		push()
	}
}
