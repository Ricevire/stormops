# PM-004 控制台"样式全丢但数据正常"

| 项目 | 内容 |
| --- | --- |
| 编号 | PM-004 |
| 发现时间 | 2026-11-23 15:25 |
| 恢复时间 | 2026-11-23 15:51 |
| 影响时长 | 约 26 分钟（公网访问开始到修复） |
| 影响范围 | 通过 `/ops/` 访问的控制台完全无样式；接口偶发返回 HTML |
| 严重级别 | P2（运维入口不可用，业务站不受影响） |
| 状态 | 已修复，两处根因都已加防护 |

---

## 1. 现象

用户打开 `http://<host>/ops/`，看到的是这样：

- 页面是**浏览器默认样式**：黑字白底、蓝色链接、"总览"两个大字
- 但**数据是全的**：资产 4/4、告警 1 条、WAF 180/187、负载曲线也画出来了
- 刷新几次，有时图表会消失、有时数字变空白

关键观察：**结构在、数据在、样式不在。** 这个组合会把人引向"CSS 文件路径写错了"，而真实原因要深得多。

---

## 2. 排查

### 2.1 先看资源响应的 MIME

```bash
$ curl -s -o /dev/null -w '%{http_code} %{content_type}\n' http://<host>/style.css
200 text/html; charset=utf-8        ← 致命

$ curl -s -o /dev/null -w '%{http_code} %{content_type}\n' http://<host>/app.js
200 text/javascript; charset=utf-8  ← 正常，所以 JS 能跑

$ curl -s -o /dev/null -w '%{http_code} %{content_type}\n' http://<host>/ops/style.css
200 text/css; charset=utf-8         ← 正常
```

`/style.css` 返回的是 **text/html**。浏览器对样式表有严格的 MIME 检查：不是 `text/css` 就**直接不应用**，控制台里连个报错都不显眼。

这就解释了"结构在、样式不在"——HTML 正常返回，CSS 被当成 HTML 丢弃。

### 2.2 为什么 JS 能跑而 CSS 不行

因为两个请求**打到了不同的后端**。

看生成出来的 Nginx 配置：

```nginx
upstream stormops_backend {
    server 127.0.0.1:8081 max_fails=3 fail_timeout=10s;   # 演示站
    server 127.0.0.1:8080 max_fails=3 fail_timeout=10s;   # StormOps
    keepalive 16;
}

location / {
    auth_request /_waf_check;
    proxy_pass http://stormops_backend;
}
```

**两个完全不同的应用被塞进了同一个 upstream，Nginx 默认按轮询分发。**

于是每个静态资源请求都像掷骰子：

| 请求 | 落到 | 结果 |
| --- | --- | --- |
| `/style.css` | 8081 演示站 | 演示站有 catch-all，返回 HTML → **CSS 被拒** |
| `/app.js` | 8080 StormOps | 正常返回 JS → **能跑** |
| `/api/v1/overview` | 8081 | 返回 HTML → 前端 `JSON.parse` 抛错 → **偶发空白** |
| `/api/v1/overview` | 8080 | 正常 JSON |

刷新几次表现不同，正是因为轮询。

### 2.3 根因

`internal/gateway` 的 `Render` 把所有 prod 服务的 upstream 拍平成了一个列表：

```go
for _, s := range services {
    if s.Env != "prod" { continue }
    r.Upstreams = append(r.Upstreams, s.Upstreams...)   // ← 这里
}
```

写这段代码时的想法是"反正都是后端，放一起还能做负载均衡"。这是**把"同一服务的多个实例"和"不同服务的多个入口"混为一谈**。

正确的模型是：**一个服务 = 一个 upstream = 一个 location**。只有同一服务的多个副本才该进同一个池子。

---

## 3. 第二个 bug：改了没生效

修完生成逻辑、重新部署，`md5` 对得上、`apply` 返回 `ok: true`，但配置文件内容**一点没变**。

```bash
$ systemctl show stormops -p MainPID -p ExecMainStartTimestamp
MainPID=16013
ExecMainStartTimestamp=Mon 2026-11-23 14:49:19    ← 部署是 15:45

$ readlink /proc/16013/exe
/opt/stormops/bin/stormops (deleted)
```

进程跑的还是**被替换掉的旧二进制**。原因在安装脚本：

```bash
systemctl enable --now stormops.service
```

`enable --now` 的语义是"设为开机自启，**如果没在跑**就启动它"。服务本来就在跑，它什么都不做。

这和 [PM-001](PM-001-waf-bypass.md) 是同一个根因的第二次发作：**二进制换了，进程还是旧的**。上次以为是"忘了重新部署"，这次才看清是脚本本身从不重启。

---

## 4. 修复

### 4.1 网关：一服务一路由

`model.Service` 增加两个字段，把"挂在哪"变成显式声明：

```go
// Prefix 是这个服务在网关上的挂载路径（如 "/" 或 "/ops/"）
Prefix string `json:"prefix,omitempty"`
// SkipWAF 让这条路由绕过 auth_request
SkipWAF bool `json:"skipWaf,omitempty"`
```

配置里：

```json
{ "name": "官网/课程站",      "prefix": "/",     "upstreams": ["127.0.0.1:8081"] },
{ "name": "StormOps 控制台",  "prefix": "/ops/", "skipWaf": true, "upstreams": ["127.0.0.1:8080"] }
```

渲染结果：

```nginx
upstream svc_1 { server 127.0.0.1:8081 ...; }
upstream svc_2 { server 127.0.0.1:8080 ...; }

location / {
    auth_request /_waf_check;      # 业务站过 WAF
    proxy_pass http://svc_1;
}
location /ops/ {
    proxy_pass http://svc_2/;      # 控制台绕过 WAF，保留自救入口
}
```

没有再"猜"前缀：没写 `prefix` 的服务只有第一个能默认占 `/`，其余直接跳过 —— 猜错就是把两个应用混在一个入口上。

### 4.2 前端：不写死绝对路径

原来：

```html
<link rel="stylesheet" href="/style.css">
<script src="/app.js"></script>
```

挂在 `/ops/` 下时，这两个绝对路径会绕开前缀，直接打到业务站上。改成相对路径：

```html
<link rel="stylesheet" href="style.css">
<script src="app.js"></script>
```

API 也一样，不能写死 `/api/v1/...`：

```js
// 控制台可能挂在 "/"（本地）或 "/ops/"（生产）后面
const MOUNT = (() => {
  let p = location.pathname;
  if (!p.endsWith("/")) p = p.replace(/\/[^\/]*$/, "/");
  return p;
})();
function apiURL(path) { return MOUNT + String(path).replace(/^\//, ""); }
```

### 4.3 部署：强制重启 + 检测旧镜像

```bash
restart_svc() {
  systemctl enable "$1" >/dev/null 2>&1 || true
  systemctl restart "$1"
}
```

再加一道断言，部署结束时检查有没有进程还在跑 `(deleted)` 的二进制：

```bash
for u in stormops stormagent stormsites ...; do
  pid=$(systemctl show "$u" -p MainPID --value)
  case "$(readlink /proc/$pid/exe)" in
    *"(deleted)") echo "!! $u 仍在运行已被替换的旧二进制" ;;
  esac
done
```

---

## 5. 验收

```bash
$ curl -s -o /dev/null -w '%{http_code} %{content_type}\n' \
    http://<host>/ops/style.css
200 text/css; charset=utf-8

$ curl -s -o /dev/null -w '%{http_code} %{content_type}\n' \
    http://<host>/ops/api/v1/overview
200 application/json; charset=utf-8

$ curl -s -o /dev/null -w '%{http_code}\n' http://<host>/.env
403
```

页面恢复完整样式，刷新多次结果稳定（不再有轮询带来的随机性）。

---

## 6. 时间线

| 时间 | 事件 |
| --- | --- |
| 15:25 | 用户反馈"访问是这样的"，附无样式截图 |
| 15:28 | 发现 `/style.css` 返回 text/html，`/ops/style.css` 正常 |
| 15:32 | 读 Nginx 配置，看到两个应用在同一个 upstream 里轮询 |
| 15:38 | 改 `model.Service` + 网关模板 + 前端相对路径 |
| 15:45 | 部署，`apply` 返回成功但配置没变 |
| 15:48 | 发现 `MainPID` 启动时间早于部署，`/proc/PID/exe` 显示 deleted |
| 15:51 | 改为强制 restart，重新 apply，验证通过 |
| 15:56 | 安装脚本加 `restart_svc` 与旧镜像检测 |

---

## 7. 改进项

| 类型 | 措施 | 状态 |
| --- | --- | --- |
| 修复 | 一服务一 upstream，按前缀分流 | ✅ |
| 修复 | 前端资源改相对路径，API 按挂载点拼接 | ✅ |
| 修复 | 安装脚本改 `restart` | ✅ |
| 检测 | 部署后检测 `(deleted)` 进程 | ✅ |
| 检测 | 部署后校验资源 MIME（CSS 必须是 text/css） | ⬜ 待做 |
| 预防 | 网关渲染加表驱动测试：两个服务必须生成两个 upstream | ⬜ 待做 |
| 预防 | `systemctl is-active` 只能证明进程活着，应加 HTTP 层存活检查 | ⬜ 待做 |

最后一条值得展开：`is-active` 为 `active` 只说明进程存在，不说明它提供的是**正确的**服务。这次的旧进程就是 `active` 的。真正的健康判定要走 HTTP，并且要校验**响应内容**而不只是状态码 —— `/style.css` 返回 200 也是错的，只是错得不明显。

---

## 8. 三条教训

1. **"部分正常"比"全挂"更难查。** 如果 CSS 和 JS 一起挂，第一反应就是"静态资源没配"。一半正常一半不正常，才会让人怀疑到分流层。
2. **负载均衡只能用在同一个服务的多个副本上。** 不同应用共用一个 upstream，等于在生产环境里做 A/B 测试，只不过分的是"谁能拿到正确响应"。
3. **`systemctl is-active` 不是健康检查。** 它只回答"进程在不在"，不回答"它在不在干正确的事"。
