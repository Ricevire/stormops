# SOP-02 告警响应

> 原则：**先止损，再定位，最后写复盘。**
> 不要在看不懂告警的情况下先重启 —— 重启会销毁现场，也会让同一个问题明天再来一次。

## 通用第一步

```bash
# 1. 现在到底怎么了
curl -s http://127.0.0.1:8080/api/v1/overview | python3 -m json.tool

# 2. 哪些告警在 firing
curl -s http://127.0.0.1:8080/api/v1/alerts | python3 -c "
import sys,json
for a in json.load(sys.stdin)['alerts']:
    if a['state']=='firing': print(a['severity'], a['host'], a['ruleName'], a['value'])"
```

---

## #cpu CPU 使用率过高

**触发**：`stormops_host_cpu_pct > 85` 持续 2 分钟。

**先分清是「真忙」还是「卡住」**：

```bash
top -b -n1 | head -15          # 谁在吃 CPU
uptime                          # load average 与核数对比（2 核 => load > 2 就过载）
vmstat 1 5                      # us/sy/wa 比例
```

判读：

| 现象 | 含义 | 动作 |
| --- | --- | --- |
| `us` 高，进程是业务进程 | 真忙，流量涨了 | 看 QPS，考虑限流或扩容 |
| `sy` 高 | 系统调用密集，可能是网络或锁 | `perf top`，看是不是 syscall 风暴 |
| `wa` 高 | 在等 IO，CPU 其实闲着 | 转 #disk 排查 |
| `load` 高但 CPU 不高 | 大量进程在 D 状态（不可中断睡眠） | 大概率 IO 卡住，查 `dmesg` |

**不要**看到 CPU 高就加机器。2 核机器上 CPU 打到 100% 而 `wa` 很高，加核没用。

---

## #memory 内存使用率过高

**触发**：`stormops_host_mem_pct > 90` 持续 3 分钟。

```bash
free -m
ps aux --sort=-rss | head -10        # RSS 排行
cat /proc/meminfo | grep -E 'MemAvailable|Dirty|Slab'
systemctl status <可疑服务> | grep Memory
```

关键判读：**看 `MemAvailable` 而不是 `MemFree`。** Linux 会用空闲内存做 page cache，`MemFree` 低是正常的。`MemAvailable` 才是应用真正能拿到的量。

如果 `MemAvailable < 100 MB`：

```bash
# 找内存大户
smem -rs rss 2>/dev/null | head
# 或者按 cgroup 看
systemd-cgls --unit stormops
```

止损顺序：

1. 重启内存泄漏的服务（先记录 `MemoryCurrent` 峰值，这是复盘的关键数据）
2. 不是重启就完事 —— 泄漏会回来。查 `MemoryMax` 是否触发过：
   ```bash
   journalctl -u <服务> | grep -i 'oom\|memory'
   ```

---

## #disk 磁盘水位过高

**触发**：`stormops_host_disk_pct > 85`。

```bash
df -h
du -xh --max-depth=1 / 2>/dev/null | sort -rh | head -10
# 找大文件
find / -xdev -type f -size +1G -printf '%s %p\n' 2>/dev/null | sort -rn | head
# 已删除但被进程占用的文件（df 和 du 不一致时必查）
lsof +L1 2>/dev/null | head
```

**最阴的一种情况**：`df` 说 90%，`du` 加起来只有 40%。说明有进程持有已删除文件的句柄，空间没释放。

```bash
lsof +L1 | awk '{print $1, $2, $7, $9}' | sort -k3 -rn | head
# 重启对应进程即可释放
```

清理优先级：

1. 日志（`journalctl --vacuum-size=200M`）
2. 旧的发布产物（`/opt/stormops/releases/` 里保留最近 3 个）
3. VM 数据（`/var/lib/victoria-metrics/`，调 `-retentionPeriod`）
4. **不要**动 `/var/lib/stormops/state.json`

清理后必须确认：

```bash
df -h /
curl -s http://127.0.0.1:8080/api/v1/capacity | python3 -m json.tool
```

---

## #service 服务探测失败

**触发**：`stormops_svc_up < 1` 持续 1 分钟。

```bash
systemctl status <服务>
journalctl -u <服务> -n 100 --no-pager
ss -lntp | grep <端口>
curl -v http://127.0.0.1:<端口>/healthz
```

四种常见原因，按出现频率排序：

1. **进程没起来** → 看 journal，多半是配置错或端口被占
2. **进程在但端口没监听** → 启动逻辑卡住（等数据库、等配置中心）
3. **端口在但健康检查失败** → 依赖不可用，应用自己返回 500
4. **全都正常但探测失败** → 探测目标写错了，或者防火墙挡了回环

第 4 种最容易被忽略。先手工 `curl` 一次探测地址，确认探测配置本身是对的。

---

## #stormops StormOps 自身不可达

**触发**：`up{job="stormops"} == 0` 持续 1 分钟。

这条告警的存在意义：**平台挂了，控制台看不见，只能靠外部告警兜底。**

```bash
systemctl status stormops
journalctl -u stormops -n 200 --no-pager
```

影响面（必须清楚）：

| 能力 | 是否受影响 |
| --- | --- |
| 业务站访问 | 不受影响（WAF 走 fail-open） |
| WAF 防护 | **失效中** |
| 发布 | 不可用 |
| 控制台 | 不可用 |
| 历史指标查询（VM） | 不受影响 |

所以 StormOps 挂了的处置优先级是「恢复 WAF 防护」，不是「恢复控制台」。如果短时间内起不来，应当临时在 Nginx 里摘掉 `auth_request`，明确进入「无 WAF 状态」并记录时间，而不是让它半死不活地挡着流量。

---

## #waf WAF 拦截量突增

**触发**：`sum(stormops_waf_blocked_24h) > 500` 持续 5 分钟。

先分清是**攻击**还是**误杀**：

```bash
curl -s 'http://127.0.0.1:8080/api/v1/waf/attacks' | python3 -c "
import sys,json
d=json.load(sys.stdin)
from collections import Counter
print('按规则:', Counter(e['ruleId'] for e in d['events']).most_common(5))
print('按IP:', Counter(e['ip'] for e in d['events']).most_common(5))"
```

| 特征 | 判断 | 动作 |
| --- | --- | --- |
| 集中在少数 IP，UA 是扫描器 | 真实攻击 | 加 IP 封禁，或调低该规则灰度到 100% |
| 分散在很多 IP，集中在某一条规则 | 大概率误杀 | 把该规则改成 `log`，观察 1 小时 |
| 集中在某个业务路径 | 可能是正常业务参数触发了规则 | 看具体请求，修规则而不是关规则 |

**误杀处理必须走「先降级为 log，再改规则」**，不能直接删规则。直接删等于把防护打穿，而且没人知道删掉的那条拦的是什么。
