// Command stormops 是 StormOps 平台的服务端。
//
// 单进程负责：指标接收与告警判定、CMDB、发布编排、网关配置生成、WAF 判定与审计。
// 没有拆成微服务，因为目标场景是"一个人维护"，
// 拆开之后光是服务间鉴权、链路追踪、部署顺序就够喝一壶，收益为零。
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Ricevire/stormops/internal/config"
	"github.com/Ricevire/stormops/internal/deploy"
	"github.com/Ricevire/stormops/internal/httpapi"
	"github.com/Ricevire/stormops/internal/model"
	"github.com/Ricevire/stormops/internal/monitor"
	"github.com/Ricevire/stormops/internal/seed"
	"github.com/Ricevire/stormops/internal/store"
)

var version = "dev"

func main() {
	var (
		cfgPath   = flag.String("config", "/etc/stormops/config.json", "配置文件路径")
		listen    = flag.String("listen", "", "覆盖配置文件里的监听地址")
		showVer   = flag.Bool("version", false, "打印版本后退出")
		forceSeed = flag.Bool("seed", false, "忽略已有状态，强制写入演示数据")
		exportDir = flag.String("export-csv", "", "把当前状态导出为 CSV 到指定目录后退出")
	)
	flag.Parse()

	if *showVer {
		fmt.Printf("stormops %s\n", version)
		return
	}

	log.SetFlags(log.LstdFlags)
	log.Printf("stormops %s 启动中，配置：%s", version, *cfgPath)

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Fatalf("加载配置失败: %v", err)
	}
	if *listen != "" {
		cfg.Listen = *listen
	}

	st, err := store.Open(cfg.DataFile)
	if err != nil {
		log.Fatalf("打开状态文件失败: %v", err)
	}
	defer func() { _ = st.Close() }()

	if *exportDir != "" {
		if err := exportState(st, *exportDir); err != nil {
			log.Fatalf("导出失败: %v", err)
		}
		return
	}

	if *forceSeed {
		st.Update(func(s *model.State) { delete(s.Meta, "seeded") })
	}
	if cfg.Demo {
		seed.Apply(st, cfg)
	} else if len(cfg.Assets) > 0 {
		// 非演示模式下仍然要把配置里的台账灌进去（配置即真相源）
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
		})
	}

	dep := deploy.New(cfg.Deploy)
	api := httpapi.New(cfg, st, dep)
	httpapi.Version = version

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           api.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       90 * time.Second,
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go evalLoop(ctx, st, cfg)
	go offlineWatch(ctx, st)
	if cfg.Demo {
		go demoTick(ctx, st, cfg)
	}

	go func() {
		log.Printf("HTTP 监听 %s", cfg.Listen)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("HTTP 服务异常退出: %v", err)
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
	log.Println("收到退出信号，开始优雅关闭…")

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer shutdownCancel()
	_ = srv.Shutdown(shutdownCtx)
	cancel()
	time.Sleep(200 * time.Millisecond)
	log.Println("已退出")
}

// evalLoop 每 15 秒跑一次告警判定。
//
// 之所以不在 ingest 时直接判定：agent 上报频率可能随部署调整，
// 把"判定节奏"和"上报节奏"解耦，规则里的 for 语义才稳定。
func evalLoop(ctx context.Context, st *store.Store, cfg *config.Config) {
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			now := time.Now().Unix()
			var res monitor.EvalResult
			st.Update(func(s *model.State) {
				res = monitor.EvalAll(s, now)
			})
			if res.Created > 0 || res.Resolved > 0 {
				log.Printf("告警判定：新增 %d，恢复 %d，静默 %d", res.Created, res.Resolved, res.Suppressed)
			}
		case <-ctx.Done():
			return
		}
	}
}

// demoTick 每分钟为演示环境补一条采样，保证界面长期保持"活"的状态。
func demoTick(ctx context.Context, st *store.Store, cfg *config.Config) {
	t := time.NewTicker(60 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			seed.DemoTick(st, cfg)
		case <-ctx.Done():
			return
		}
	}
}

// offlineWatch 每 30 秒检查一次资产心跳。
func offlineWatch(ctx context.Context, st *store.Store) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			now := time.Now().Unix()
			st.Update(func(s *model.State) {
				for i := range s.Assets {
					a := &s.Assets[i]
					if a.LastSeen == 0 {
						continue
					}
					if now-a.LastSeen > 120 && a.Status != "offline" {
						log.Printf("资产心跳超时：%s（最后心跳 %ds 前）", a.Hostname, now-a.LastSeen)
						a.Status = "offline"
					}
				}
			})
		case <-ctx.Done():
			return
		}
	}
}
