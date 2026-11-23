# PM-001 WAF 参数类规则全量漏拦

| 项目 | 内容 |
| --- | --- |
| 编号 | PM-001 |
| 发现时间 | 2026-11-23 14:10 |
| 恢复时间 | 2026-11-23 14:49 |
| 影响时长 | 约 40 分钟（部署后到修复） |
| 影响范围 | SQL 注入、XSS 两类参数规则完全失效 |
| 严重级别 | P1（防护缺口，非业务中断） |
| 状态 | 已修复，已加回归用例 |

---

## 1. 现象

部署完成后做验收测试，路径类规则全部正常，参数类规则全部放行：

```text
=== WAF 测试 ===
/?id=1%27%20OR%20%271%27%3D%271                200   ← 应该 403
/search?q=%3Cscript%3Ealert(1)%3C/script%3E    200   ← 应该 403
/.env                                          403   ← 正常
/wp-login.php                                  403   ← 正常
/?cmd=;cat%20/etc/passwd                       403   ← 正常
/?page=2                                       200   ← 正常
```

规律很清晰：**`target=uri` 的规则有效，`target=args` 和带参数的 `uri` 规则无效。**

直接打内部判定端点，排除 Nginx 的干扰：

```bash
curl -i -H "X-Original-URI: /?id=1%27%20OR%20%271%27%3D%271" \
     http://127.0.0.1:8080/internal/waf/check
# HTTP/1.1 204 No Content   ← StormOps 自己就没拦
```

问题定位在 StormOps 内部，不在 Nginx。

---

## 2. 排查过程

### 2.1 第一层假设：编码没解码（错误方向）

规则 `W1001` 的正则是 `(union\s+select|or\s+1\s*=\s*1|'\s*or\s*')`。

拿到的 `X-Original-URI` 是 `/?id=1%27%20OR%20%271%27%3D%271`，规则里写的是 `' OR '`，字面上不匹配 —— 所以判断是「没有做 URL 解码」。

于是加了 `normalize()`，做两层 `QueryUnescape`：

```go
func normalize(s string) string {
    for i := 0; i < 2; i++ {
        d, err := url.QueryUnescape(s)
        if err != nil || d == s { break }
        s = d
    }
    return s
}
```

两层是因为双重编码（`%2527` → `%27` → `'`）是最常见的绕过手法。

改完、编译、部署、再测 —— **仍然 200**。

### 2.2 第二层假设：以为代码逻辑还是错的

这里差点走错方向。当时的推理是「`'\s*or\s*'` 可能匹配不到 `' OR '`」，准备去改正则。

如果真去改了正则，问题会被「修好」，但真正的根因会被永久掩盖 —— 因为下一次任何代码改动都会重新触发这个问题。

### 2.3 定位到真实原因：部署没有生效

写了一个最小用例在本地跑：

```go
d := Check(rules, Request{IP: "1.2.3.4", Method: "GET", URI: "/?id=1%27%20OR%20%271%27%3D%271"}, 0)
// blocked=true rule=W1001
```

**本地是对的。** 那问题只能在服务器上跑的东西和本地代码不一致。

对比 md5：

```
local : d330c4bc99240a9c4f4b6f31ec076415  .deploy/stormops
remote: 4d8fc9234ea95acfb85e1bf917201a14  /opt/stormops/bin/stormops
```

不一致。**服务器上跑的是没有 `normalize()` 的旧二进制。**

两个文件大小完全相同（8917176 字节），所以靠 `ls -la` 看不出差别 —— 这也是为什么前面几轮排查一直在怀疑代码。

---

## 3. 根因

两层原因叠加：

1. **直接原因**：`deploy.sh` 执行时，`normalize()` 的改动还没写进被编译的那份源码；之后又单独改代码但没有重新部署。部署脚本跑完只输出「部署结束」，没有任何一致性校验。
2. **放大器**：本地和远端二进制大小一致，导致「部署没生效」这个可能性被反复排除。

真正的教训不是「要记得重新部署」，而是：**部署流程缺少一个能证明「远端跑的就是本地这份代码」的环节。**

---

## 4. 修复

### 4.1 立即修复

```bash
scp .deploy/stormops root@host:/opt/stormops/bin/stormops.new
ssh root@host 'mv -f /opt/stormops/bin/stormops.new /opt/stormops/bin/stormops && systemctl restart stormops'
```

验证：

```text
=== 内部 WAF 判定 ===
/?id=1%27%20OR%20%271%27%3D%271               403  X-Waf-Rule: W1001 SQL 注入特征
/search?q=%3Cscript%3Ealert(1)%3C/script%3E   403  X-Waf-Rule: W1002 XSS 脚本注入
/?cmd=;cat%20/etc/passwd                      403  X-Waf-Rule: W1004 命令注入特征
/.env                                         403  X-Waf-Rule: W1003 高危路径扫描
/?page=2                                      204
```

五条规则全部按预期工作。

### 4.2 流程修复

在 `deploy.sh` 末尾加硬校验，不一致就 `exit 1`：

```bash
LOCAL_MD5=$(md5sum "$OUT/stormops" | awk '{print $1}')
REMOTE_MD5=$(ssh "${SSHOPT[@]}" "$HOST" "md5sum /opt/stormops/bin/stormops" | awk '{print $1}')
[ "$LOCAL_MD5" = "$REMOTE_MD5" ] || { echo "!! 二进制不一致，部署未生效"; exit 1; }
```

再加一道服务状态检查：

```bash
for u in stormops stormagent stormsite victoria-metrics vmagent vmalert alertmanager nginx; do
  printf '%-18s %s\n' $u $(systemctl is-active $u)
done
```

### 4.3 认知修复

验收 WAF 变更时，必须用**构造请求**验证规则能拦住，不能相信「配置改成功」的返回。这一条已经写进 [SOP-03](../sop/SOP-03-waf-change.md) 第 4 节。

---

## 5. 时间线

| 时间 | 事件 |
| --- | --- |
| 14:10 | 验收测试发现参数类规则全部放行 |
| 14:15 | 打内部端点确认问题在 StormOps 内部，非 Nginx |
| 14:20 | 判断为「未做 URL 解码」，加 `normalize()` |
| 14:28 | 部署，复测仍失败 |
| 14:35 | 本地最小用例验证 —— 代码逻辑正确 |
| 14:41 | 对比 md5，发现远端是旧二进制 |
| 14:49 | 上传新二进制，重启，五条规则全部生效 |
| 14:55 | 给 `deploy.sh` 加 md5 校验与服务状态检查 |

从发现问题到修复 39 分钟，其中 **21 分钟花在错误方向上**（怀疑代码而不是怀疑部署）。

---

## 6. 三条改进

| 类型 | 措施 | 状态 |
| --- | --- | --- |
| 检测 | `deploy.sh` 部署后强制比对 md5 | ✅ 已做 |
| 检测 | 部署后自动打印七个服务状态 | ✅ 已做 |
| 预防 | WAF 变更后用构造请求验证，写入 SOP | ✅ 已做 |
| 预防 | 给 WAF 判定加表驱动测试（4 类攻击 × 编码变体） | ⬜ 待做 |
| 预防 | 部署带上 git commit hash，`/api/v1/health` 返回它 | ⬜ 待做 |

最后一条是最有价值的：如果 `health` 接口能返回 commit hash，那么「远端跑的是哪份代码」这个问题永远不需要靠 md5 去猜。这应该是下一步。

---

## 7. 值得记住的一句话

> **「改了没生效」和「改错了」表现完全一样，但排查成本差十倍。**

在没有部署校验的流程里，人会本能地怀疑自己刚写的代码（因为那是最近变动的部分），而实际上最近变动的部分可能是「部署」这个动作本身。
