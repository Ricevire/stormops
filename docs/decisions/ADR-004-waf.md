# ADR-004 WAF：自研判定服务 + `auth_request` 旁路

- 状态：已采纳
- 日期：2026-11-23
- 决策者：平台负责人

## 背景

需求：给官网、课程站、电商落地页加 WAF，要求能按业务调整规则，并且运维自己能改。

候选方案：

1. **Nginx + ModSecurity + OWASP CRS**
2. **云 WAF**（阿里云/Cloudflare）
3. **Nginx `auth_request` + 自研判定服务**
4. **OpenResty + Lua 脚本**

约束：

- 2 vCPU / 2 GB，Nginx 已经在这台机器上
- 规则要能频繁调整（业务方经常反馈误杀）
- 只有 1–2 个运维，不能引入需要专职维护的安全组件
- 流量不大（QPS 个位数到几十）

## 决策

**采用方案 3：Nginx `auth_request` 把每个请求旁路到 StormOps 的 `/internal/waf/check`，StormOps 用内存正则判定，返回 204 放行 / 403 拦截。**

```nginx
location = /_waf_check {
    internal;
    proxy_pass http://127.0.0.1:8080/internal/waf/check;
    proxy_pass_request_body off;          # 子请求不带 body
    proxy_set_header X-Original-URI $request_uri;
    proxy_set_header X-Original-Method $request_method;
    proxy_set_header X-Real-IP $remote_addr;
}

location / {
    auth_request /_waf_check;
    auth_request_set $waf_rule $upstream_http_x_waf_rule;
    proxy_pass http://stormsite;
}

location @waf_block {
    return 403;
}
```

## 理由

### 为什么不选 ModSecurity

**编译成本。** ModSecurity 需要单独编译 Nginx 连接器（`ngx_http_modsecurity_module`），意味着：

- 不能用发行版的 `nginx` 包，得自己编
- 每次 Nginx 安全更新都要重新编译
- 对一台 2C2G 的边缘机，这个维护负担不成比例

**规则调整成本。** 改一条规则 = 改 `.conf` 文件 + `nginx -t` + `reload`。对「业务方说这条误杀了，赶紧放开」这种场景，反馈链太长。

**内存成本。** ModSecurity + CRS 全套规则，常驻内存在 100–200 MB 量级。在这台机器上是可以接受的，但不是无代价。

### 为什么不选云 WAF

- 5 Mbps 的链路，所有流量绕一圈云端，延迟增加明显
- 按量计费对不确定的流量不好估算
- 最关键：**这个项目的目的是证明「我能自己做 WAF 运维」**，接一个云 WAF 只能证明「我会点控制台」

最后一条是诚实的原因。如果这是生产环境而不是求职项目，云 WAF 其实是更合理的选择 —— 见文末。

### 为什么不用 OpenResty + Lua

技术上更优雅（判定在 Nginx 进程内，没有额外一跳）。否决理由：

- 需要换 Nginx 发行版（OpenResty），又是一次编译/替换
- 规则判定逻辑用 Lua 写，和平台其他部分（Go）分裂成两种语言
- 判定逻辑要读 StormOps 的内存状态，跨进程还是要走 HTTP，等于没省那一跳

### `auth_request` 的关键优势

1. **规则即内存**：StormOps 判定用的是内存里的规则对象，改规则是一次 `PATCH` 请求，下一次请求立即生效，**不需要 reload**。
2. **可观测**：每次判定都能记录到审计日志，规则命中数直接从日志聚合。
3. **可灰度**：`rolloutPct` 字段让新规则能按 IP 灰度，这是 ModSecurity 原生不支持的。
4. **可导出**：`Compile()` 把规则转成标准 `SecRule` 语法，需要时可以迁回 ModSecurity。

第 4 点很重要 —— 它意味着这个决策是**可逆的**。规则库不是锁死在自研格式里。

## 后果

### 好的

- 规则改动即时生效，不需要 reload Nginx
- 支持灰度（`hash(ip|ruleID) % 100 < pct`）
- 判定逻辑可单元测试（纯函数 `Check(rules, req, now)`）
- 规则可导出为 ModSecurity 格式，迁移路径清晰
- 内存占用小（正则编译后缓存，几 MB）

### 坏的

- **拿不到请求体。** `auth_request` 子请求的 body 是空的（`proxy_pass_request_body off`），所以只覆盖 URI / args / headers 三个位置。POST 类注入（比如 JSON body 里的 SQL 注入）完全漏掉。
  要做 body 检测，得换成 `mirror` + 异步判定，或者回到 ModSecurity。这是当前最大的功能缺口。
- **没有 anomaly scoring。** CRS 的核心是「多条规则各加几分，超过阈值才拦」。当前是「命中任意一条 block 规则就拦」，误杀率必然更高。
- **正则性能。** 每条规则都要对整个 subject 跑一次正则。当前 5 条规则无所谓，50 条就要考虑合并或者用 RE2 的前缀过滤。
- **多一跳。** 每个请求多一次本地回环 HTTP 调用。实测 P99 增量 < 1 ms，但确实存在。
- **fail-open 的边界。** StormOps 不可用时，`auth_request` 拿到 502，Nginx 视为拒绝。但 StormOps 的 panic 恢复分支显式返回 204 —— 也就是**代码 bug 导致 panic 时是放行，进程挂掉时是拒绝**。这个不一致是有意的（见 PM-002），但很容易被误解为 bug。

### 这个不一致必须解释清楚

```go
// 进程活着但代码 panic → 放行（fail-open）
if r.URL.Path == "/internal/waf/check" {
    w.WriteHeader(http.StatusNoContent)
    return
}
```

```
# 进程挂了 → 502 → Nginx 拒绝（fail-closed）
proxy_pass http://127.0.0.1:8080/internal/waf/check;
```

看起来矛盾，实际是权衡：

- **代码 panic** 是局部的、可能只影响某条规则。为此让全站 403，代价远大于收益。
- **进程挂掉** 是全局的，此时没有任何判定能力。如果还放行，等于「攻击者只要打挂 WAF 就能绕过 WAF」。

但这个逻辑不直观。更好的做法是：panic 时返回 204 但**同时打一条 critical 日志并触发告警**，让「正在 fail-open」这件事可见。当前只做了前者，后者没做。

## 如果这是真实生产环境

会选云 WAF 或者 ModSecurity + CRS，理由是：

1. 安全组件的价值在于**规则库的持续更新**（新 CVE 出来当天就有规则），自研做不到
2. WAF 的正确性极难自证，「我觉得能拦住」和「真的能拦住」之间差距很大
3. 出安全事件时，自研 WAF 无法免责

自研方案的价值在于**可解释、可调整、可灰度**，适合「流量不大、规则要频繁改、需要理解每一层在做什么」的场景。这也是这个项目选择它的真实理由 —— 它是一个学习载体，同时具备实际可用性。

## 备选方案对照

| 维度 | ModSecurity+CRS | 云 WAF | auth_request 自研 | OpenResty+Lua |
| --- | --- | --- | --- | --- |
| 规则质量 | 高（OWASP CRS） | 高 | 低（自己写） | 低 |
| 改动成本 | 高（改文件+reload） | 低 | **极低（一次 API）** | 低 |
| body 检测 | 支持 | 支持 | **不支持** | 支持 |
| 灰度 | 不支持 | 部分支持 | **支持** | 需自己实现 |
| 内存 | 100–200 MB | 0 | **< 10 MB** | < 20 MB |
| 可迁移性 | — | 锁定 | **可导出 SecRule** | 锁定 |
| 编译/替换 Nginx | 需要 | 不需要 | **不需要** | 需要 |
