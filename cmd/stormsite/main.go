// Command stormsite 是被 StormOps 发布、被 Nginx 反代、被 WAF 保护的演示业务站。
//
// 它存在的意义是让"发布"这件事可验证：页面右上角显示的版本号来自二进制内嵌变量，
// 发布 v1.1.0 之后刷新页面，版本号必须变。健康检查失败时会自动回滚，
// 版本号也会退回去 —— 整个过程不需要任何人工解释。
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
)

var version = "v1.0.0"

const page = `<!doctype html>
<html lang="zh-CN"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>STORMCREW · 影像与周边</title>
<style>
*{box-sizing:border-box}body{margin:0;font-family:-apple-system,"PingFang SC","Microsoft YaHei",sans-serif;
background:#0b0f14;color:#e8edf3;-webkit-font-smoothing:antialiased}
header{display:flex;align-items:center;gap:14px;padding:22px 44px;border-bottom:1px solid #1b2431}
.logo{width:30px;height:30px;border-radius:8px;background:linear-gradient(140deg,#14b8a6,#0f766e);
display:grid;place-items:center;font-weight:700;font-size:15px;color:#fff}
.brand{font-weight:600;letter-spacing:.5px}
nav{margin-left:auto;display:flex;gap:22px;font-size:13.5px;color:#8b9bb0}
.tag{font-family:ui-monospace,Menlo,monospace;font-size:11.5px;color:#5eead4;background:#0f2b28;
padding:3px 9px;border-radius:20px;border:1px solid #164e48}
main{padding:60px 44px 40px;max-width:1080px}
h1{font-size:44px;line-height:1.15;letter-spacing:-1.5px;margin:0 0 14px;font-weight:600}
h1 em{font-style:normal;color:#5eead4}
p.lead{color:#94a3b8;font-size:15px;line-height:1.8;max-width:620px;margin:0 0 34px}
.grid{display:grid;grid-template-columns:repeat(3,1fr);gap:16px;margin-top:8px}
.card{background:#111823;border:1px solid #1b2431;border-radius:12px;padding:20px}
.card h3{margin:0 0 6px;font-size:14px;font-weight:500}
.card p{margin:0;color:#7d8b9e;font-size:12.5px;line-height:1.7}
.k{font-family:ui-monospace,Menlo,monospace;font-size:11px;color:#5eead4;letter-spacing:.5px}
footer{padding:26px 44px 50px;color:#4b5768;font-size:12px}
</style></head><body>
<header><div class="logo">S</div><div class="brand">STORMCREW</div>
<span class="tag">%s</span>
<nav><span>影像</span><span>课程</span><span>周边</span><span>关于</span></nav></header>
<main>
<h1>把影像拍到<em>极限</em>，<br>也把系统做到极限。</h1>
<p class="lead">这是一个用于验证发布链路的演示站点。它由 StormOps 编排发布、由 Nginx 反向代理、
由 StormOps WAF 做旁路检测。右上角的版本号来自二进制内嵌变量 —— 发布成功就变，回滚就退回去。</p>
<div class="grid">
<div class="card"><div class="k">BUILD</div><h3>版本 %s</h3><p>由 ldflags 注入，不读配置文件。避免"配置写错导致版本显示不对"。</p></div>
<div class="card"><div class="k">HEALTH</div><h3>/healthz</h3><p>发布编排在这里轮询，连续 3 次 200 才判定新版本可用。</p></div>
<div class="card"><div class="k">WAF</div><h3>auth_request</h3><p>每个请求都会先问一次 StormOps，命中规则直接 403。</p></div>
</div></main>
<footer>STORMCREW demo site · 仅用于 StormOps 发布与 WAF 链路验证</footer>
</body></html>`

func main() {
	addr := flag.String("addr", "127.0.0.1:8081", "监听地址")
	forceBroken := flag.Bool("broken", false, "模拟故障版本：健康检查返回 500")
	flag.Parse()

	// 版本号里带 bad 的产物会被构建成"故障版本"。
	// 这样产物本身是自描述的：谁把它发到线上，谁就该预期健康检查会失败，
	// 不需要额外的元数据文件来解释"这个包是坏的"。
	broken := *forceBroken || strings.Contains(version, "bad")

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		if broken {
			// 模拟"进程起来了但依赖没就绪"的典型故障：
			// 端口在监听，但这台机器不可用。单次探测很容易被骗过去。
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprint(w, "db not ready")
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, "ok\n")
	})
	mux.HandleFunc("GET /api/version", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"version":%q,"buildTime":%q}`+"\n", version, buildTime)
	})
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, page, version, version)
	})

	srv := &http.Server{
		Addr:              *addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	log.Printf("stormsite %s 监听 %s（broken=%v）", version, *addr, broken)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("服务异常: %v", err)
	}
	_ = os.Stdout
}

var buildTime = "unknown"
