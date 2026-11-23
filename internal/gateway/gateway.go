// Package gateway 把 CMDB 里的服务注册信息渲染成 Nginx 配置。
//
// 核心原则：Nginx 配置是"生成物"，不是"手写物"。
// 任何人手工改 /etc/nginx/conf.d/stormops.conf 都会在下一次 apply 时被覆盖，
// 配置的唯一真相源是 StormOps 的服务台账 —— 这是把"配置漂移"从流程问题变成结构问题的关键。
package gateway

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/template"
	"time"

	"github.com/Ricevire/stormops/internal/config"
	"github.com/Ricevire/stormops/internal/model"
)

const confTemplate = `# ============================================================
# 本文件由 StormOps 自动生成，请勿手工修改。
# 生成时间: {{.GeneratedAt}}
# 变更方式: 修改 StormOps 中的服务台账 -> 预览 -> 应用
# 回滚方式: cp {{.ConfPath}}.bak {{.ConfPath}} && nginx -t && nginx -s reload
# ============================================================
#
# 每个服务一个独立 upstream + 一个 location，按路径前缀分流。
#
# 这里踩过一个很贵的坑：早期版本把所有 prod 服务的 upstream 合并成一个池子，
# Nginx 就按轮询把请求分给两个完全不同的应用 —— /style.css 被转到了演示站，
# 返回 HTML，浏览器拒绝当样式表用，页面光秃秃的；而 /app.js 恰好转到
# StormOps，于是"样式全丢、数据正常"。看起来像前端 bug，实际是网关分流错。
# 复盘见 docs/postmortem/PM-004-upstream-mixing.md。

{{range .Routes}}
upstream {{.Upstream}} {
    {{- range .Servers}}
    server {{.}} max_fails=3 fail_timeout=10s;
    {{- end}}
    keepalive 16;
}
{{end}}

server {
    listen {{.ListenPort}};
    server_name {{.ServerName}};

    # WAF 旁路检测端点：Nginx 用 auth_request 调它，
    # StormOps 返回 204 放行 / 403 拦截。请求体不转发，只送元信息，
    # 避免把用户上传的大文件在 WAF 链路上再复制一次。
    location = /_waf_check {
        internal;
        proxy_pass {{.WAFCheckURL}};
        proxy_pass_request_body off;
        proxy_set_header Content-Length "";
        proxy_set_header X-Original-URI  $request_uri;
        proxy_set_header X-Original-Method $request_method;
        proxy_set_header X-Real-IP       $remote_addr;
        proxy_set_header User-Agent      $http_user_agent;
        proxy_connect_timeout 200ms;
        proxy_read_timeout    200ms;
        # 关于 fail-open / fail-closed 的取舍：
        # 本地回环 + 200ms 超时，正常情况下这个子请求的成本可以忽略。
        # 但 StormOps 若整体不可用，auth_request 会返回 502，nginx 把非 2xx 视为拒绝，
        # 也就是 **fail-closed**（整站不可用）。这是刻意选择：WAF 是安全边界，
        # 默认不在安全组件故障时静默放行全部流量。
        # 代价与缓解：stormops 由 systemd Restart=always 托管、与业务进程隔离、
        # 且控制台路径完全绕过 WAF，保证运维仍有自救入口。
        proxy_next_upstream off;
    }

    access_log /var/log/nginx/stormops.access.log;
    error_log  /var/log/nginx/stormops.error.log warn;

    client_max_body_size 512m;      # 素材上传需要大 body
    keepalive_timeout 65s;

    gzip on;
    gzip_types text/css application/javascript application/json image/svg+xml;
    gzip_min_length 1024;

    location = /_stormops_health {
        access_log off;
        return 200 "ok\n";
    }
{{range .Routes}}
    # {{.Label}}  ->  {{.Upstream}}
    location {{.Prefix}} {
{{- if not .SkipWAF}}
        auth_request /_waf_check;
        auth_request_set $waf_rule $upstream_http_x_waf_rule;

        error_page 403 = @waf_block;
{{- end}}

        proxy_http_version 1.1;
        proxy_set_header Connection "";
        proxy_set_header Host              $host;
        proxy_set_header X-Real-IP         $remote_addr;
        proxy_set_header X-Forwarded-For   $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
        proxy_read_timeout 60s;
        # 带前缀的 location 要在 proxy_pass 里补一个 /，否则前缀不会被剥掉
        proxy_pass http://{{.Upstream}}{{if .Strip}}/{{end}};
    }
{{end}}

    location @waf_block {
        default_type text/html;
        return 403 '<!doctype html><meta charset="utf-8"><title>403 请求被安全策略拦截</title><body style="font-family:system-ui;padding:60px;line-height:1.8"><h2>403 请求被安全策略拦截</h2><p>本次请求命中了 Web 应用防火墙规则。</p><p style="color:#666">Request-ID: $request_id &nbsp;|&nbsp; Rule: $waf_rule</p><p style="color:#666">如果这是误报，请把 Request-ID 提供给运维同学调整规则灰度。</p></body>';
    }
}
`

// routeView 是模板里的一条路由。
//
// 一个服务一条路由，绝不把多个服务的 upstream 混进同一个池子 ——
// 混池会让 Nginx 轮询到错误的应用程序上。
type routeView struct {
	Prefix   string
	Upstream string
	Servers  []string
	SkipWAF  bool
	Strip    bool
	Label    string
}

// RenderInput 是渲染模板所需的输入。
type RenderInput struct {
	GeneratedAt string
	ConfPath    string
	Routes      []routeView
	ListenPort  int
	ServerName  string
	WAFCheckURL string
}

// Render 生成 Nginx 配置文本。
func Render(cfg config.GatewayConfig, services []model.Service) (string, error) {
	r := RenderInput{
		GeneratedAt: time.Now().Format("2006-01-02 15:04:05"),
		ConfPath:    defaultStr(cfg.ConfPath, "/etc/nginx/conf.d/stormops.conf"),
		ListenPort:  orInt(cfg.ListenPort, 80),
		ServerName:  defaultStr(cfg.ServerName, "_"),
		WAFCheckURL: defaultStr(cfg.WAFCheckURL, "http://127.0.0.1:8080/internal/waf/check"),
	}

	used := map[string]bool{}
	for _, s := range services {
		if s.Env != "prod" || len(s.Upstreams) == 0 {
			continue
		}
		prefix := strings.TrimSpace(s.Prefix)
		if prefix == "" {
			// 没显式声明前缀的服务，只有"第一个"能默认占据根路径。
			// 其余的不去猜 —— 猜错就是把两个应用混在一个入口上。
			if len(r.Routes) > 0 {
				continue
			}
			prefix = "/"
		}
		if !strings.HasPrefix(prefix, "/") {
			prefix = "/" + prefix
		}
		if prefix != "/" && !strings.HasSuffix(prefix, "/") {
			prefix += "/"
		}
		if used[prefix] {
			continue
		}
		used[prefix] = true
		r.Routes = append(r.Routes, routeView{
			Prefix:   prefix,
			Upstream: fmt.Sprintf("svc_%d", len(r.Routes)+1),
			Servers:  s.Upstreams,
			SkipWAF:  s.SkipWAF,
			Strip:    prefix != "/",
			Label:    s.Name,
		})
	}

	if len(r.Routes) == 0 {
		r.Routes = []routeView{{
			Prefix:   "/",
			Upstream: "svc_1",
			Servers:  []string{"127.0.0.1:8081"},
			Label:    "默认站点",
		}}
	}

	// 没有任何服务占据根路径时，把第一个服务也挂到 / 上，
	// 否则直接访问域名会 404 —— 这是运维最难解释的一种"站点打不开"。
	if !used["/"] {
		first := r.Routes[0]
		first.Prefix = "/"
		first.Strip = false
		first.Label = first.Label + "（默认入口）"
		r.Routes = append([]routeView{first}, r.Routes...)
	}

	var buf bytes.Buffer
	tmpl, err := template.New("nginx").Parse(confTemplate)
	if err != nil {
		return "", fmt.Errorf("模板解析失败: %w", err)
	}
	if err := tmpl.Execute(&buf, r); err != nil {
		return "", fmt.Errorf("模板渲染失败: %w", err)
	}
	return buf.String(), nil
}

// Lint 做基础语法自检，作为 nginx -t 不可用时的退路。
func Lint(conf string) error {
	if !strings.Contains(conf, "server {") {
		return fmt.Errorf("缺少 server 块")
	}
	depth := 0
	for i, line := range strings.Split(conf, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") {
			continue
		}
		depth += strings.Count(trimmed, "{")
		depth -= strings.Count(trimmed, "}")
		if depth < 0 {
			return fmt.Errorf("第 %d 行出现多余的右花括号", i+1)
		}
	}
	if depth != 0 {
		return fmt.Errorf("花括号不配对，差值 %d", depth)
	}
	return nil
}

// ApplyResult 是一次配置应用的结果。
type ApplyResult struct {
	OK          bool   `json:"ok"`
	Changed     bool   `json:"changed"`
	Backup      string `json:"backup"`
	Message     string `json:"message"`
	ValidateOut string `json:"validateOut"`
	ReloadOut   string `json:"reloadOut"`
}

// Apply 把配置写入目标文件：先备份，再写，再校验，校验失败自动还原。
//
// 顺序很关键。校验对象必须是"即将生效的那份配置"，所以流程是：
//
//	写 .new -> 旧文件改名成 .bak -> .new 改名为 .conf -> nginx -t
//	                                                    ├─ 通过 -> reload
//	                                                    └─ 失败 -> 还原 .bak（此时 nginx 内存里仍是旧配置，服务不受影响）
//
// 如果反过来先校验再替换，校验的其实是旧配置，等于没校验。
func Apply(conf string, cfg config.GatewayConfig) ApplyResult {
	path := defaultStr(cfg.ConfPath, "/etc/nginx/conf.d/stormops.conf")
	res := ApplyResult{}

	if err := Lint(conf); err != nil {
		res.Message = "本地语法自检未通过: " + err.Error()
		return res
	}

	existing, _ := os.ReadFile(path)
	if string(existing) == conf {
		res.OK = true
		res.Message = "配置无变化，跳过应用"
		return res
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		res.Message = "创建目录失败: " + err.Error()
		return res
	}

	tmp := path + ".new"
	if err := os.WriteFile(tmp, []byte(conf), 0o644); err != nil {
		res.Message = "写入临时文件失败: " + err.Error()
		return res
	}

	backup := ""
	if len(existing) > 0 {
		backup = fmt.Sprintf("%s.bak.%s", path, time.Now().Format("20060102-150405"))
		if err := os.Rename(path, backup); err != nil {
			_ = os.Remove(tmp)
			res.Message = "备份旧配置失败: " + err.Error()
			return res
		}
		res.Backup = backup
	}

	if err := os.Rename(tmp, path); err != nil {
		if backup != "" {
			_ = os.Rename(backup, path)
		}
		res.Message = "落盘失败: " + err.Error()
		return res
	}

	if out, err := runCmd(defaultStr(cfg.ValidateCmd, "nginx -t"), 20*time.Second); err != nil {
		res.ValidateOut = out
		_ = os.Remove(path)
		if backup != "" {
			_ = os.Rename(backup, path)
			res.Backup = ""
		}
		res.Message = "nginx -t 未通过，已还原旧配置（线上服务未受影响）"
		return res
	} else {
		res.ValidateOut = out
	}
	res.Changed = true

	if out, err := runCmd(defaultStr(cfg.ReloadCmd, "nginx -s reload"), 20*time.Second); err != nil {
		res.ReloadOut = out
		if backup != "" {
			if b, e := os.ReadFile(backup); e == nil {
				_ = os.WriteFile(path, b, 0o644)
				_, _ = runCmd(defaultStr(cfg.ReloadCmd, "nginx -s reload"), 20*time.Second)
			}
		}
		res.Message = "reload 失败，已回滚到上一版配置"
		return res
	} else {
		res.ReloadOut = out
	}

	res.OK = true
	res.Message = "配置已应用并 reload"
	return res
}

func runCmd(cmd string, timeout time.Duration) (string, error) {
	if strings.TrimSpace(cmd) == "" {
		return "", nil
	}
	c := exec.Command("sh", "-c", cmd)
	done := make(chan struct{})
	var out []byte
	var err error
	go func() {
		out, err = c.CombinedOutput()
		close(done)
	}()
	select {
	case <-done:
		return string(out), err
	case <-time.After(timeout):
		if c.Process != nil {
			_ = c.Process.Kill()
		}
		return string(out), fmt.Errorf("命令超时(%s): %s", timeout, cmd)
	}
}

func defaultStr(v, d string) string {
	if strings.TrimSpace(v) == "" {
		return d
	}
	return v
}

func orInt(v, d int) int {
	if v == 0 {
		return d
	}
	return v
}
