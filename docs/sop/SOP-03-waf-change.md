# SOP-03 WAF 规则变更

> WAF 变更的最大风险不是「漏拦」，是**「把自己人拦了」**。
> 所以流程的核心不是「怎么加规则」，而是「怎么加得可撤回」。

## 1. 规则变更的四种类型

| 类型 | 例子 | 风险 | 流程 |
| --- | --- | --- | --- |
| 新增观察规则 | 给路径穿越加 `log` | 低 | 直接上，`action=log` |
| 新增拦截规则 | 给某个新 CVE 的特征加 `block` | 中 | 必须灰度 |
| 收紧现有规则 | 把 `or 1=1` 扩成 `or\s+\d+\s*=\s*\d+` | **高** | 灰度 + 盯 30 分钟 |
| 放宽/删除规则 | 误杀严重，先降为 `log` | 中 | 先降级观察，确认无攻击再利用再删 |

「收紧」最危险，因为它会命中以前能过的请求。历史流量里总有一些奇怪的参数，收紧之后才会暴露。

---

## 2. 灰度机制

每条规则有 `rolloutPct` 字段（0–100）。判定：

```go
h = FNV-1a(ip + "|" + ruleID)
inRollout = (h % 100) < rolloutPct
```

用哈希而不是随机数，是为了**让同一个客户端在灰度期间的判定稳定**。

如果改用 `rand.Intn(100) < pct`，攻击者只要重试 3 次就有 70% 概率穿过一条 30% 灰度的规则 —— 灰度就变成了「随机放行」，失去意义。

用 `ip|ruleID` 而不是只用 `ip`，是为了让不同规则对同一个 IP 的灰度互相独立。否则一个 IP 一旦落在某规则的 30% 里，会对所有规则都落在同一侧，灰度样本不独立。

---

## 3. 标准流程

### 3.1 加一条拦截规则

```bash
# ① 先以 log 模式上线，rolloutPct=100
curl -sX PATCH http://127.0.0.1:8080/api/v1/waf/rules/W1006 \
  -H 'Content-Type: application/json' \
  -d '{"action":"log","enabled":true,"rolloutPct":100}'

# ② 观察 1 小时，看命中量
curl -s 'http://127.0.0.1:8080/api/v1/waf/attacks' | python3 -c "
import sys,json
from collections import Counter
d=json.load(sys.stdin)
print(Counter(e['ruleId'] for e in d['events'] if e['ruleId']=='W1006'))"
```

判断标准：

| 观察结果 | 判断 | 下一步 |
| --- | --- | --- |
| 0 命中 | 规则可能写错了，或者真的没攻击 | 用 `curl` 构造一个应该命中的请求验证规则本身 |
| 少量命中，URI 明显是攻击 | 正常 | 进灰度 |
| 命中里有正常业务路径 | 误杀 | 改正则，不要进灰度 |

**0 命中不等于安全，很可能是规则根本没生效。** 这是踩过的坑，见 [PM-001](../postmortem/PM-001-waf-bypass.md)。

```bash
# ③ 进灰度：10% → 30% → 100%，每档至少 30 分钟
curl -sX PATCH ... -d '{"action":"block","rolloutPct":10}'
curl -sX PATCH ... -d '{"rolloutPct":30}'
curl -sX PATCH ... -d '{"rolloutPct":100}'
```

每档之间要确认：业务站 5xx 没有上升、没有用户反馈访问异常。

### 3.2 误杀时的降级

**唯一正确做法：先降级为 log，不要直接删。**

```bash
curl -sX PATCH http://127.0.0.1:8080/api/v1/waf/rules/W1002 \
  -d '{"action":"log"}'
```

为什么不能直接删：

1. 删了之后这条攻击特征就完全没人管了，可能当天晚上就被打
2. 没有观察数据，你无法知道「误杀」和「真实攻击」的比例
3. 降级为 log 之后，审计日志还在，能复盘出到底是哪类请求被误伤

---

## 4. 变更前后必查

### 变更前

```bash
# 记下基线，用于对比
curl -s http://127.0.0.1:8080/api/v1/overview | python3 -m json.tool > /tmp/before.json
```

### 变更后（5 分钟内）

```bash
# ① 规则确实生效了吗？用构造请求验证，不要相信"改成功了"的返回
curl -s -o /dev/null -w '%{http_code}\n' \
  -H "X-Original-URI: /?id=1' OR '1'='1" \
  http://127.0.0.1:8080/internal/waf/check
# 期望 403

# ② 正常请求还能过吗？
curl -s -o /dev/null -w '%{http_code}\n' -H "X-Original-URI: /?page=2" \
  http://127.0.0.1:8080/internal/waf/check
# 期望 204

# ③ 业务站整体还正常吗？
curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1/about   # 期望 200
```

第 ① 步是必须的。规则改完返回 `ok` 只代表「写进内存了」，不代表「能拦住」。

---

## 5. 规则变更记录

每次变更都要能回答三个问题：改了什么、为什么改、怎么撤回。

```
时间：2026-11-23 14:52
规则：W1001 SQL 注入特征
变更：修复参数类请求漏拦（匹配前补归一化）
灰度：100%（修复类变更，不灰度）
验证：/?id=1%27%20OR%20%271%27%3D%271 → 403
回滚：git revert + 重新部署；无配置层回滚
关联：PM-001
```

没有这条记录，三个月后没人说得清这条规则为什么长这样。

---

## 6. `/ops/` 例外路径

Nginx 里 `/ops/` 显式绕开 `auth_request`。这条规则本身就是一个安全边界，变更时必须知道：

- 绕过 WAF 的路径 = 攻击面。所以 `/ops/` 必须只给内网或加 Basic Auth。当前**没有加认证**，是已知缺口。
- 如果为了「让 WAF 全量覆盖」而给 `/ops/` 也加上 `auth_request`，会导致 StormOps 不可用时无法通过控制台自救。见 [PM-002](../postmortem/PM-002-waf-lockout.md)。
