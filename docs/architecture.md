# 架构说明

## 1. 设计前提

先把约束摆出来，后面所有设计都是从这几条推出来的：

| 约束 | 具体数字 | 推出来的设计 |
| --- | --- | --- |
| 机器规格 | 2 vCPU / 2 GB / 40 GB / 5 Mbps | 不能上 K8s，不能跑 Elastic，每个进程必须有内存上限 |
| 维护人力 | 1–2 人 | 组件数量要能一只手数完，出问题要能在一屏内看完 |
| 业务流量 | 官网/课程站 QPS 个位数，峰值几十 | WAF 可以被每个请求同步调用，不需要异步队列 |
| 素材体量 | 单条 4K 素材 20–80 GB，日增 100 GB 级 | 容量预测必须是"剩余天数"，不能是"使用率" |
| 网络 | 5 Mbps 上行 | 监控数据必须本地落盘，不能往云上打 |

最后一条经常被忽略：5 Mbps 意味着一天最多传 54 GB。如果把监控数据发到云端 SaaS，光指标就能吃掉整条链路。所以 VictoriaMetrics 必须跑在本机，只用 57 MB 内存。

---

## 2. 进程模型

七个进程，各管一件事，互不内嵌：

| 进程 | 端口 | MemoryMax | 实际占用 | 职责 |
| --- | --- | --- | --- | --- |
| `stormops` | 8080 | 320 MB | 8 MB | 平台主进程：API、告警判定、发布编排、WAF 判定 |
| `stormagent` | — | 96 MB | 4 MB | 采集 CPU/内存/磁盘/网络/负载/服务探测，POST 上报 |
| `stormsite` | 8081 | 128 MB | 1 MB | 被发布、被 WAF 保护的业务站 |
| `victoria-metrics` | 8428 | 400 MB | 57 MB | 指标长期存储（默认保留 90 天） |
| `vmagent` | 8429 | 160 MB | 19 MB | 15s 抓取 `/metrics`，remote write 到 VM |
| `vmalert` | 8880 | 160 MB | 10 MB | PromQL 规则评估，告警推 Alertmanager |
| `alertmanager` | 9093 | 128 MB | 15 MB | 分组、抑制、静默、去重 |
| `postgresql` | 5432 | 未限 | — | 分析库，每日 03:20 全量重建 |
| `nginx` | 80 | 未限 | 3 MB | 入口，配置由 StormOps 生成 |

`MemoryMax` 加起来 1.39 GB，实际占用约 117 MB。这个 8 倍差距是有意的：**限额是防泄漏的护栏，不是运行目标**。真正需要盯的是 `MemoryCurrent` 的趋势，不是限额本身。

### 为什么 PostgreSQL 和 Nginx 不设限额

PostgreSQL 的 `shared_buffers` 和 page cache 会主动吃满可用内存，这是它正常工作的方式，设上限反而会拖慢查询。Nginx 是 master + worker 模型，内存随连接数走。这两个组件用 `MemoryHigh` 软限更合适，硬限会制造假故障。这是当前的一个已知缺口，见文末。

---

## 3. 请求路径

### 3.1 业务请求（被 WAF 判定）

```
客户端
  │ GET /?id=1' OR '1'='1
  ▼
Nginx :80
  │ ① auth_request /_waf_check
  │    子请求：GET /_waf_check
  │    X-Original-URI: /?id=1%27%20OR%20%271%27%3D%271
  │    X-Real-IP: <client>
  ▼
StormOps :8080 /internal/waf/check
  │ ② 归一化：两层 QueryUnescape
  │    匹配 subject（uri / args / headers / body）
  │    命中 W1001 → 403
  ▼
Nginx 收到 403 → 走 @waf_block，返回 403 页面
```

放行时 StormOps 返回 204，Nginx 继续 `proxy_pass` 到 `stormsite`。整条链路每个请求多一次本地回环调用，实测 P99 增量在 1 ms 以内。

**关键设计**：`/internal/waf/check` 这条路径上不做任何磁盘 IO、不写日志、不加全局锁。判定只用内存里预编译好的正则（`sync.Map` 缓存）。任何一处 write 都会让它在 QPS 上来时变成瓶颈。

### 3.2 控制台请求

`/ops/` 前缀在 Nginx 里显式绕开 `auth_request`，直接 `proxy_pass` 到 StormOps。这不是疏忽，是安全边界的一部分 —— 详见 [PM-002](postmortem/PM-002-waf-lockout.md)。

```
Nginx: location /ops/  →  proxy_pass http://127.0.0.1:8080/
```

StormOps 的静态资源用 `embed.FS` 打进二进制，所以控制台不依赖任何外部文件，`scp` 一个二进制就能跑。

### 3.3 指标路径

```
stormagent  ──POST /internal/ingest──►  StormOps 内存指标环
                    （30s）                 │
                                           ├─ 告警引擎（秒级，驱动 UI）
                                           │
                                           └─ GET /metrics（自研文本格式）
                                                    │
                                            vmagent 15s 抓取
                                                    │
                                            VictoriaMetrics（90 天）
                                                    │
                                              vmalert 30s 评估
                                                    │
                                              Alertmanager
```

指标在 StormOps 内部保留 6 小时（内存环 + 落盘），超过 6 小时的查询走 VictoriaMetrics。这个分界点选 6 小时是因为控制台所有图表的最大窗口就是 6 小时，再长就走「趋势分析」页，那页读的是 PostgreSQL。

---

## 4. 状态与持久化

`internal/store` 是整个平台唯一的可变状态持有者。

```go
View(fn)    // 读锁，fn 里只能读
Update(fn)  // 写锁，标记 dirty
```

后台 flusher 每 3 秒检查 dirty，有变化就落盘：

```
写 state.json.tmp  →  fsync  →  rename 到 state.json
                              →  旧文件转 state.json.bak
```

用 `rename` 而不是直接覆写，是因为直接覆写在断电时会留下一个合法的空 JSON，下次启动解析出空状态、把整个 CMDB 清掉 —— 这是最坏的一种失败。`rename` 在同一个文件系统上是原子的。

启动顺序：先读 `state.json`，解析失败则读 `state.json.bak`，都失败才用配置里的初始数据。这条回退链保证"状态文件损坏"不会变成"平台空库"。

### 为什么不用 SQLite

考虑过。放弃的原因不是性能，是**部署形态**：SQLite 需要一个可写的 db 文件、需要考虑 WAL 模式和锁竞争、`scp` 单个二进制就能跑的便利性会丢掉。当前状态量级（4 台资产、2 个服务、5000 条攻击事件上限）用内存 + JSON 完全够，而且代码里 `store` 的接口是按"以后换成 PostgreSQL 直读"来抽象的。

数据量到十万级就该换，这一点写在 README 的已知限制里。

---

## 5. 发布编排

状态机是固定的七步，顺序不可跳：

```
precheck ──► backup ──► stage ──► verify ──► switch ──► health
                                                        │
                                              失败 ◄────┘
                                                │
                                                ▼
                                            rollback ──► health（再次确认）
```

每一步都会写 `ReleaseStep{Name, Status, StartedAt, EndedAt, Detail}`，前端轮询 `/api/v1/releases` 就能看到进度。不允许并发：`Start()` 里检查是否存在 `running` 的发布，有就拒绝。

### 为什么 health 要连续 3 次 200

单次 200 会骗人。进程刚起来、监听端口通了，但数据库连接池还没建好、配置还没从 etcd 拉下来 —— 这时候探测一次 200，运维以为发布成功，30 秒后真实流量打进来全 500。

所以 `waitHealthy()` 要求连续 3 次成功，间隔 2 秒：

```go
for i := 0; i < 3; i++ {
    if !probe(url) { return false }
    time.Sleep(2 * time.Second)
}
```

代价是发布慢 4 秒。这个钱值得花。

### 回滚

`switch` 之前失败：直接清掉 stage 目录，线上无感。
`switch` 之后失败：把 `current` 软链指回 `previous`，再跑一次 `health` 确认恢复。

回滚本身也可能失败。这时候 `Release.Status = "failed"` 且 `Detail` 里写明「回滚后健康检查仍失败」，并且**不再自动重试** —— 自动重试会把一个明确的问题变成一串无意义的动作，掩盖真实原因。

---

## 6. 网关配置生成

`internal/gateway` 负责把 CMDB 里的服务定义变成 Nginx 配置文本。

`Apply()` 的顺序容易写错，这里记录正确顺序和原因：

```
1. 渲染配置写到 gateway.conf.new
2. 备份现有 gateway.conf → gateway.conf.bak
3. 用 .new 替换 gateway.conf
4. nginx -t
5. nginx -s reload
6. 失败则从 .bak 还原并再次 nginx -t
```

第 3 步必须在第 4 步之前。如果先 `nginx -t` 再替换，校验的是**旧文件**，等于没校验 —— 这个 bug 真实发生过，改完才发现之前所有「校验通过」都是假的。

`Lint()` 是一个轻量的花括号配对检查，挡的是模板渲染出错导致的残缺配置。它不能替代 `nginx -t`，只是在写文件之前先挡一道。

---

## 7. 告警判定

两套引擎的分工见 [ADR-003](decisions/ADR-003-metrics-split.md)。这里只说内部引擎的实现要点。

```go
func EvalAll(st *model.State, now int64) EvalResult
```

单条规则的生命周期：

```
采样 → 是否超阈值 → 是否持续 forSec → 是否被静默 → 生成/更新告警
```

三个容易写错的地方：

1. **幂等**：同一 `(ruleID, host)` 只保留一条活跃告警。否则每轮评估都会新建一条，控制台会被同一个问题刷屏。
2. **持续时长容差**：判断「持续 2 分钟」时用 `0.8 * forSec` 作为下界。上报间隔是 30 秒，如果严格按 120 秒算，采样点抖动会导致永远凑不满，规则形同失效。
3. **静默**：静默按 `(ruleID, host)` 或全局匹配，到点自动失效，不留下永久静默。

---

## 8. 故障模式与影响

| 故障 | 影响 | 现有缓解 | 缺口 |
| --- | --- | --- | --- |
| StormOps 进程挂 | 控制台、发布、WAF 判定全停 | `Restart=always`，3s 重启 | 重启期间 WAF 请求全部走 fail-open 分支，等于无防护 |
| StormOps 卡死（不退出但不响应） | 同上，且 systemd 不会重启 | 无 | **应加 `WatchdogSec` + `/healthz` 心跳**，当前未做 |
| state.json 损坏 | 状态丢失 | `.bak` 回退 | 若两者都坏，CMDB 要从配置重建，历史告警全丢 |
| VictoriaMetrics OOM | 历史指标断档 | `MemoryMax=400M` | 断档期间 vmalert 判不出「无数据」，需要 `absent()` 规则，当前未加 |
| PostgreSQL 挂 | 分析页 500 | 不在读路径，控制台不受影响 | timer 会每天失败，但没有告警 —— 应当给 `systemd` 失败加 `OnFailure=` |
| 磁盘写满 | 全部服务异常 | 有容量预测，86% 就告警 | 预测是「趋势」，突发写入挡不住 |
| Nginx 配置错误 | 全站 502 | `nginx -t` + 回滚 | 若 `nginx` 二进制本身出问题，无兜底 |

这张表是主动列出来的。面试时被问「你这个平台有什么问题」，直接照这张表讲，比硬编一个「没有明显问题」可信得多。

---

## 9. 下一步（按优先级）

1. **给 StormOps 加 `WatchdogSec`**：现在的 `Restart=always` 只能处理进程退出，处理不了卡死。
2. **给 PostgreSQL 失败加告警**：`OnFailure=stormops-alert@%n.service`，把 timer 失败也接进 Alertmanager。
3. **`absent()` 规则**：VM 断档时也要告警，否则「没数据」会被误认为「没问题」。
4. **WAF 请求体检测**：`mirror` + 异步判定，补上 POST 类攻击的盲区。
5. **发布支持多机**：目标机列表 + 分批 + 每批之间观察期。
