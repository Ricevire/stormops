# PM-003 vmalert 无限重启（装错了二进制）

| 项目 | 内容 |
| --- | --- |
| 编号 | PM-003 |
| 发现时间 | 2026-11-23 13:52 |
| 恢复时间 | 2026-11-23 14:36 |
| 影响时长 | 约 44 分钟 |
| 影响范围 | 告警规则未评估，vmalert 未产生任何告警 |
| 严重级别 | P2（监控链路缺一段，业务无影响） |
| 状态 | 已修复，安装脚本已加自检 |

---

## 1. 现象

首次部署后检查服务状态，七个组件里六个 `active`，只有 vmalert 是 `activating`：

```text
vmalert            activating
```

`activating` 这个状态很误导 —— 它听起来像「正在启动」，实际含义是「反复启动失败，systemd 还在重试」。

看真实数据：

```bash
$ systemctl show vmalert -p NRestarts -p ExecMainStatus -p ActiveState
NRestarts=125
ExecMainStatus=1
ActiveState=activating
```

**125 次重启。** 每隔 3 秒重启一次，说明进程启动后立刻退出（exit code 1）。

---

## 2. 排查

### 2.1 看日志

```bash
$ journalctl -u vmalert -n 50 --no-pager
```

关键行：

```text
Incorrect Usage: flag provided but not defined: -rule

NAME:
   vmalert-tool - VMAlert command-line tool
```

两件事同时暴露了：

1. 进程不认 `-rule` 参数
2. **它自称 `vmalert-tool`**

### 2.2 手工运行确认

```bash
$ /opt/stormops/bin/vmalert -version
vmalert-tool-20261106-174742-tags-v1.156.0
```

确认了：`/opt/stormops/bin/vmalert` 这个文件，内容是 **vmalert-tool**，不是 vmalert。

### 2.3 对比体积

```bash
$ ls -la /tmp/vmdl/vu/
-rwxr-xr-x 1 root root 28084584 Nov  7 01:48 vmalert-prod
-rwxr-xr-x 1 root root 36524448 Nov  7 01:48 vmalert-tool-prod

$ ls -la /opt/stormops/bin/vmalert
-rwxr-xr-x 1 root root 36524448 Nov 23 13:49 /opt/stormops/bin/vmalert
```

36524448 = `vmalert-tool-prod` 的体积。装错了。

---

## 3. 根因

`install-vm.sh` 里这一行：

```bash
AL=$(find vu -type f -name 'vmalert*' | head -1); [ -n "$AL" ] && cp "$AL" "$BIN/vmalert"
```

`vmutils` 压缩包里同时有：

```
vmalert-prod
vmalert-tool-prod
```

`find -name 'vmalert*'` 两个都匹配，`head -1` 取目录遍历顺序的第一个 —— 恰好是 `vmalert-tool-prod`。

**`find` 的输出顺序不保证。** 这次取到 tool，换台机器可能取到本体，这种「有时对有时错」的 bug 最难查。

### 为什么 `vmalert-tool` 看起来也能跑

`vmalert-tool` 是 VictoriaMetrics 提供的规则测试工具（类似 `promtool test rules`），它和 `vmalert` 是**同一个代码库编出来的两个入口**，共享大量参数解析代码，所以：

- 能被 systemd 启动
- 会读取配置文件
- 但参数集不同：它没有 `-rule`（单数），只有 `-rule.templates`、`-rule.validateExpressions` 之类

结果就是启动即退出，`Restart=always` 无限循环。

---

## 4. 修复

```bash
$ cp -f /tmp/vmdl/vu/vmalert-prod /opt/stormops/bin/vmalert
$ chmod +x /opt/stormops/bin/vmalert
$ /opt/stormops/bin/vmalert -version
vmalert-20261106-174742-tags-v1.156.0-0-g0c1bf87a0c   ← 注意没有 "-tool"
$ systemctl restart vmalert
$ systemctl is-active vmalert
active
```

`NRestarts=0`，`ExecMainStatus=0`。

验证规则真的在跑：

```bash
$ curl -s http://127.0.0.1:8880/api/v1/rules | python3 -m json.tool | head -20
{
    "state": "inactive",
    "name": "HostCpuHigh",
    "query": "stormops_host_cpu_pct > 85",
    "duration": 120,
    "health": "ok",
    "lastEvaluation": "2026-11-23T14:50:37+08:00",
    "lastSeriesFetched": 4
}
```

`health: ok` + `lastSeriesFetched: 4` 说明规则确实抓到了 4 条时间序列。

告警链路端到端确认：

```bash
$ curl -s http://127.0.0.1:8880/api/v1/alerts
{"state":"firing","name":"HostDiskHigh","value":"86.64",
 "labels":{"host":"media-nas-01","severity":"warning"}}

$ curl -s http://127.0.0.1:9093/api/v2/alerts
[{"annotations":{"summary":"media-nas-01 磁盘水位 86.64%"},"status":{"state":"active"}}]
```

vmalert 判定 firing → Alertmanager 收到 active。链路通了。

---

## 5. 脚本修复

```bash
# 必须按准确文件名挑：vmutils 包里同时有 vmalert-prod 和 vmalert-tool-prod，
# 用 'vmalert*' + head -1 会碰运气选到 vmalert-tool（它不认 -rule 参数）
AL=$(find vu -type f -name 'vmalert-prod' | head -1); [ -n "$AL" ] && cp "$AL" "$BIN/vmalert"

# 装完立刻自检：拿到的必须是 vmalert 本体，不是 vmalert-tool
if ! "$BIN/vmalert" -version 2>&1 | head -1 | grep -q '^vmalert-'; then
  echo "FATAL: $BIN/vmalert 不是 vmalert 本体，实际是: $("$BIN/vmalert" -version 2>&1 | head -1)"
  exit 1
fi
```

自检这一步是关键。原来的脚本 `cp` 完就往下走，`chmod +x` 也成功了，**没有任何地方会失败** —— 安装脚本「成功」了，装的东西是错的。

---

## 6. 为什么这个故障危险

它不报错。整条链路看起来是绿的：

- `deploy.sh` 退出码 0
- `install-server.sh` 每一步都是 OK
- 文件存在，权限正确，`systemctl start` 也没报错
- `systemctl status` 显示 `activating`，容易被理解成「还在启动」

**唯一能暴露它的是「主动去查告警有没有产生」。**

如果没查，那么：

- 磁盘涨到 95% —— 没人知道
- 服务挂了 —— 没人知道
- 一直到某天业务真的出问题，才会发现监控从来没工作过

这是「监控系统的监控」缺失的典型。所以下一步应该加一条自监控：

```
vmalert 超过 5 分钟没有成功评估 => 告警
```

用 `vmalert_iteration_total` 或者 `vmalert_alerting_rules_last_evaluation_samples` 判。当前没做，登记为待补。

---

## 7. 改进项

| 类型 | 措施 | 状态 |
| --- | --- | --- |
| 修复 | 安装正确的 `vmalert-prod` | ✅ 已做 |
| 预防 | 脚本按精确文件名选二进制 | ✅ 已做 |
| 预防 | 安装后 `-version` 自检，不匹配即 `exit 1` | ✅ 已做 |
| 检测 | `deploy.sh` 打印七个服务状态 | ✅ 已做 |
| 检测 | vmalert 评估停滞告警 | ⬜ 待做 |
| 预防 | 安装脚本对每个二进制都做 `-version` 冒烟 | ⬜ 待做（当前只做了 vmalert） |

---

## 8. 一句话总结

> **安装脚本的成功退出码，只证明「命令跑完了」，不证明「装对了」。**

凡是「复制/解压/挑选」这类操作，都要在最后加一步「拿到的东西对不对」的断言。尤其是压缩包里存在多个名字相近的可执行文件时。
