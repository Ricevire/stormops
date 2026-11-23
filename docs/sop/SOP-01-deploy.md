# SOP-01 发布流程

> 适用：通过 StormOps 发布 `stormsite` 到生产。
> 前置：控制台可访问，目标版本产物已在 `releasesDir` 中。

## 0. 发布前检查（2 分钟）

- [ ] `/api/v1/health` 返回 200，`stormops_up = 1`
- [ ] 当前没有 `status = running` 的发布（控制台「服务与发布」页顶部）
- [ ] 目标版本号不与现有版本重复（重复会被 `precheck` 拒绝）
- [ ] 变更窗口内没有其他计划内操作
- [ ] 已知会失败的健康检查项已确认（比如依赖服务是否正常）

## 1. 触发

控制台「服务与发布」→ 选择服务 → 选择产物 → 「开始发布」。

等价 API：

```bash
curl -sX POST http://127.0.0.1:8080/api/v1/releases \
  -H 'Content-Type: application/json' \
  -d '{"service":"official-site","version":"v1.1.0"}'
```

## 2. 观察七个阶段

| 阶段 | 正常耗时 | 失败意味着 |
| --- | --- | --- |
| `precheck` | < 1 s | 参数不对、产物不存在、已有发布在跑 |
| `backup` | 1–3 s | 磁盘不可写、权限问题 |
| `stage` | < 1 s | 产物解包失败 |
| `verify` | < 1 s | 产物校验和不匹配（文件损坏） |
| `switch` | < 1 s | 软链切换失败 |
| `health` | 6–8 s | 新版本起不来或依赖未就绪 |
| `rollback` | 6–8 s | 回滚后仍不健康 —— **立即升级** |

`health` 阶段要连续 3 次 200，间隔 2 秒，所以正常也要 6 秒以上。**看到 `health` 停在那里不要慌，这是设计。**

## 3. 成功判定

```
status = success
detail 中 health 步骤为「连续 3 次探测通过」
```

验证：

```bash
curl -s http://127.0.0.1:8081/api/version   # 应返回新版本
curl -s http://127.0.0.1/api/v1/health      # 经 Nginx 也应正常
```

## 4. 失败处理

### 4.1 `health` 失败，已自动回滚

```
status = failed
detail = 回滚完成，当前版本已恢复为 <prev>
```

要做的事：

1. 确认线上确实恢复：`curl /api/version` 应是旧版本
2. **不要立刻重试**。先看新版本为什么起不来：
   ```bash
   journalctl -u stormsite -n 100 --no-pager
   ls -la /opt/stormops/releases/
   ```
3. 定位原因后修产物，重新构建再发

### 4.2 回滚后仍不健康（最坏情况）

```
status = failed
detail = 回滚后健康检查仍失败
```

这是 P1，立即人工介入：

```bash
# 1. 看当前指向
ls -la /opt/stormops/services/official-site/current

# 2. 手工切回上一个确定可用的版本
ln -sfn /opt/stormops/releases/v1.0.0 /opt/stormops/services/official-site/current
systemctl restart stormsite

# 3. 确认
curl -s http://127.0.0.1:8081/api/version
```

平台**不会**在回滚失败后自动重试。自动重试会把一个明确的问题变成一串无意义的动作，掩盖真实原因。这时候需要人。

## 5. 发布后（10 分钟）

- [ ] 控制台「总览」观察 5 分钟：QPS、错误率、P95 无异常
- [ ] 若有告警，确认是发布引起还是偶发
- [ ] 记录：版本号、开始结束时间、是否回滚、异常现象

## 6. 演示自动回滚（面试用）

产物里内置了一个坏版本 `v1.2.0-bad`，它的 `/healthz` 恒返回 500：

```bash
# 选 v1.2.0-bad 发布，观察 health 失败 → 自动 rollback → 恢复 v1.1.0
```

这个演示的价值在于：**回滚是自动的，不需要人在 4 秒内反应过来。**
