# PM-002 近失：给 `/ops/` 加 WAF 会把控制台锁在门外

| 项目 | 内容 |
| --- | --- |
| 编号 | PM-002 |
| 类型 | **近失事件**（在造成影响前被设计评审拦下） |
| 发现阶段 | 网关模板编写阶段 |
| 影响范围 | 若未处理：StormOps 不可用时，无法通过控制台自救 |
| 严重级别 | P1（潜在） |
| 状态 | 已通过架构隔离规避，风险仍在文档中登记 |

> 这份复盘记录的是一个**没有真正发生**的事故。
> 写它的理由：这类问题的特征是「上线当天一切正常，出事那天才发现没有退路」，
> 等它真发生再复盘就晚了。

---

## 1. 设计时的直觉

WAF 的目标是「所有外部请求都过一遍规则」。按这个直觉，Nginx 配置应该是：

```nginx
location / {
    auth_request /_waf_check;
    proxy_pass http://stormsite;
}

location /ops/ {
    auth_request /_waf_check;     # ← 问题在这
    proxy_pass http://127.0.0.1:8080/;
}
```

看起来很对：连控制台也被保护了。

---

## 2. 推演一遍故障链

假设某次改 WAF 规则，写错了一条正则：

```
① 运维通过 /ops/ 提交新规则
② 规则里有一条匹配范围过宽，命中了 /ops/ 自身的静态资源请求
   （比如规则里写了 "\.js" 或者某个参数名）
③ /ops/ 的后续请求被 StormOps 自己判为攻击，返回 403
④ Nginx 把 403 返回给浏览器
⑤ 控制台打不开
⑥ 想改回去？只能通过控制台改 —— 但控制台打不开
```

第 ⑥ 步是这个设计的致命处：**修复手段依赖被破坏的东西本身。**

这跟防火墙配错把自己 SSH 断开是同一类问题，只不过 SSH 至少还有 console/VNC 兜底，而这里没有。

### 更糟的一种：StormOps 挂了

```
① StormOps 进程异常退出
② Nginx 调 /_waf_check → 连接被拒 → 502
③ auth_request 把非 2xx 视为拒绝 → 所有请求 403
④ 控制台打不开（因为 /ops/ 也走 auth_request）
⑤ 想重启 StormOps？得先能登上机器 —— 可以
⑥ 但运维如果只会用控制台，就彻底没办法了
```

这里暴露的是同一个问题的另一面：**auth_request 是 fail-closed 的。**

---

## 3. fail-open 还是 fail-closed

这是 WAF 设计里最需要明确表态的一个选择，没有中间答案。

| 策略 | 行为 | 优点 | 缺点 |
| --- | --- | --- | --- |
| fail-open | WAF 不可用时**放行** | 业务不中断 | WAF 挂了等于没防护，且没人知道 |
| fail-closed | WAF 不可用时**拒绝** | 安全性不降级 | WAF 一挂全站 403，等于自己 DDoS 自己 |

**选择：对业务流量 fail-open，对管理面隔离。**

理由：

1. 这台机器上跑的是官网和课程站。**网站打不开的代价 > 短时间无 WAF 的代价。** 被扫描是常态，被打穿是小概率；而网站 403 是 100% 的用户流失。
2. WAF 挂掉这件事本身有独立告警（`StormOpsDown`），不需要靠「全站 403」来通知。
3. 但 fail-open 必须留下痕迹，否则会变成「悄悄失去防护」。所以 StormOps 重启期间要有明确的状态标记。

---

## 4. 最终方案

```nginx
# 业务路径：过 WAF
location / {
    auth_request /_waf_check;
    auth_request_set $waf_rule $upstream_http_x_waf_rule;
    proxy_pass http://stormsite;
}

# 管理路径：显式绕过
location /ops/ {
    proxy_pass http://127.0.0.1:8080/;
}
```

同时在 StormOps 侧做两件事：

1. **`/internal/waf/check` 的 panic 恢复返回 204 而不是 500。**
   一个空指针不应该让整站 403。
   ```go
   if r.URL.Path == "/internal/waf/check" {
       w.WriteHeader(http.StatusNoContent)  // fail-open
       return
   }
   ```
2. **`/ops/` 的路径在 StormOps 里也是白名单。** 即使规则匹配到了，也不拦控制台自身。

这套组合的效果：**管理面永远可达，业务面优先可用。**

---

## 5. 这个方案的代价（必须说清楚）

`/ops/` 绕过 WAF，意味着**控制台是一个不受 WAF 保护的入口**。

如果控制台有登录功能，攻击者可以直接对它做暴力破解、SQL 注入，WAF 一条都拦不住。

所以这个设计成立的前提是：**`/ops/` 不能暴露在公网，或者必须另有认证。**

当前状态：**`/ops/` 通过公网 80 端口可访问，没有 Basic Auth，没有 IP 白名单。**

这是一个真实的、已知的、尚未修复的安全缺口。修法很明确：

```nginx
location /ops/ {
    # 方案 A：IP 白名单
    allow 203.0.113.0/24;
    deny all;

    # 方案 B：Basic Auth
    auth_basic "StormOps";
    auth_basic_user_file /etc/nginx/.htpasswd;

    proxy_pass http://127.0.0.1:8080/;
}
```

选 B 更实际，因为运维的出口 IP 经常变。这条已经列入待办。

---

## 6. 沉淀出的规则

> **任何「保护措施」上线前，先问：如果它误判了我自己，我怎么关掉它？**

如果答案依赖被保护的东西本身，这个设计就是错的。

同类问题清单（值得在每次做安全变更时过一遍）：

- 防火墙规则改错 → 断 SSH
- WAF 规则改错 → 锁控制台
- 证书过期 → 管理面 HTTPS 打不开
- 认证服务挂了 → 无法登录去修认证服务
- 配置中心挂了 → 无法下发配置去修配置中心

共同解法：**留一条不经过被保护组件的带外通道**（本地 console、独立管理端口、物理带外管理）。在这里，那条通道就是「SSH 上机器 + 手工改 Nginx」。
