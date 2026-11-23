// Package config 负责加载 StormOps 的运行配置。
package config

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/Ricevire/stormops/internal/model"
)

// Config 是进程级配置，来源是 /etc/stormops/config.json。
type Config struct {
	Listen     string          `json:"listen"`
	DataFile   string          `json:"dataFile"`
	PublicBase string          `json:"publicBase"`
	Org        string          `json:"org"`
	Demo       bool            `json:"demo"`
	Agents     []AgentConfig   `json:"agents"`
	Services   []model.Service `json:"services"`
	Assets     []model.Asset   `json:"assets"`
	Rules      []model.AlertRule `json:"rules"`
	WAFRules   []model.WAFRule `json:"wafRules"`
	Capacity   CapacityConfig  `json:"capacity"`
	Deploy     DeployConfig    `json:"deploy"`
	Gateway    GatewayConfig   `json:"gateway"`
	Notify     NotifyConfig    `json:"notify"`
}

// AgentConfig 描述一个 stormagent 的接入点。
type AgentConfig struct {
	Name  string `json:"name"`
	Addr  string `json:"addr"`
	Token string `json:"token"`
}

// CapacityConfig 描述容量模型的输入。
type CapacityConfig struct {
	Volumes []VolumeConfig `json:"volumes"`
}

// VolumeConfig 是一个存储卷的容量上限与预警线。
type VolumeConfig struct {
	Name     string  `json:"name"`
	Mount    string  `json:"mount"`
	Capacity float64 `json:"capacity"`
	WarnPct  float64 `json:"warnPct"`
	CritPct  float64 `json:"critPct"`
}

// DeployConfig 描述发布流程的执行细节。
//
// Hooks 里每个键对应一个步骤名，值是 shell 命令。
// 留空则视为 dry-run：步骤会被记录但不真正执行，健康检查仍然是真的。
type DeployConfig struct {
	Enabled     bool              `json:"enabled"`
	DryRun      bool              `json:"dryRun"`
	ReleasesDir string            `json:"releasesDir"`
	TargetDir   string            `json:"targetDir"`
	HealthURL   string            `json:"healthUrl"`
	ServiceTag  string            `json:"serviceTag"`
	Hooks       map[string]string `json:"hooks"`
}

// GatewayConfig 描述 Nginx 配置生成。
type GatewayConfig struct {
	ConfPath  string `json:"confPath"`
	ServerName string `json:"serverName"`
	ListenPort int    `json:"listenPort"`
	UpstreamPort int  `json:"upstreamPort"`
	WAFCheckURL string `json:"wafCheckUrl"`
	ValidateCmd string `json:"validateCmd"`
	ReloadCmd   string `json:"reloadCmd"`
}

// NotifyConfig 描述告警外发。
type NotifyConfig struct {
	FeishuWebhook string `json:"feishuWebhook"`
	MinSeverity   string `json:"minSeverity"`
}

// Load 读取并校验配置文件。
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取配置失败: %w", err)
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("解析配置失败: %w", err)
	}
	if c.Listen == "" {
		c.Listen = "127.0.0.1:8080"
	}
	if c.DataFile == "" {
		c.DataFile = "/var/lib/stormops/state.json"
	}
	return &c, nil
}
