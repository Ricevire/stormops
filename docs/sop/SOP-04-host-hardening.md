# SOP-04 主机加固：SSH 与防火墙

> 适用场景：新机器上线前、或发现凭据可能外泄时。
> 顺序很重要：**先确认密钥能登，再关密码；先放行端口，再开防火墙。** 反了就是把自己关在门外。

---

## 1. 为什么这两步必须做

这台机器的 root 密码曾经出现在一张聊天截图里。对公网 IP + root + 密码这种组合，扫描器平均几分钟就会试一次。

做好两件事就把这个风险基本关掉：

1. **SSH 只允许密钥登录** —— 密码泄露等于作废，因为密码通道根本不存在
2. **主机防火墙默认拒绝入站** —— 只留 22（管理）和 80（业务）

第二件还有个副作用：以后任何服务不小心监听了 `0.0.0.0`，也不会立刻暴露到公网。这是"纵深防御"里最便宜的一层。

---

## 2. 加固前先确认密钥可用

不要跳过。后面所有操作都建立在"密钥能登"这个前提上。

```bash
ssh -i ~/.ssh/stormops_deploy root@<host> 'echo ok'
```

如果这条不通，**不要继续**。先把密钥装好（`ssh-copy-id` 或云厂商控制台），否则关掉密码登录就是永久锁死。

---

## 3. 关闭 SSH 密码登录

### 3.1 常见错误做法

直接改 `/etc/ssh/sshd_config` 里的 `PasswordAuthentication no`。

在 Ubuntu 24.04 上这**往往不生效**，而且不生效得毫无提示。

### 3.2 为什么不生效

sshd 的规则是：**同一个配置项，第一次出现的值生效，后面的全部忽略。**

Ubuntu 的 `sshd_config` 第 12 行有：

```nginx
Include /etc/ssh/sshd_config.d/*.conf
```

而云镜像通常会在那个目录里放一个 `50-cloud-init.conf`：

```bash
$ cat /etc/ssh/sshd_config.d/50-cloud-init.conf
PasswordAuthentication yes
```

文件按**字典序**加载，`50-` 排在 `99-` 前面。所以哪怕你写了 `99-hardening.conf` 里 `PasswordAuthentication no`，最终生效的还是 `50-` 里的 `yes`。

这次的真实结果：

```bash
$ sshd -T | grep -E '^(passwordauthentication|permitrootlogin)'
permitrootlogin without-password   ← 我写的生效了
passwordauthentication yes         ← 被 50-cloud-init 抢先
```

同一个文件里，`PermitRootLogin` 生效了而 `PasswordAuthentication` 没生效 —— 因为主配置文件第 33 行的 `PermitRootLogin yes` 在 Include 之后，而 `PasswordAuthentication` 在主文件里是注释掉的。

### 3.3 正确做法

用 `00-` 前缀，保证排在最前：

```bash
cat > /etc/ssh/sshd_config.d/00-hardening.conf <<'EOF'
# 密码曾以截图形式外泄，改为仅允许密钥登录。
# 保留 root 但禁止密码（prohibit-password），
# 否则密钥丢失后只能走云厂商的 VNC 控制台。
PasswordAuthentication no
KbdInteractiveAuthentication no
PubkeyAuthentication yes
PermitRootLogin prohibit-password
EOF

sshd -t && systemctl reload ssh
```

**用 `reload` 不要用 `restart`**：reload 不会断开已有连接，万一配置有问题还有回旋余地。

### 3.4 验证

```bash
$ sshd -T | grep -E '^(passwordauthentication|permitrootlogin|pubkeyauthentication)'
passwordauthentication no
permitrootlogin without-password
pubkeyauthentication yes

# 用密码通道连一次，应该直接被拒
$ ssh -o PubkeyAuthentication=no -o PreferredAuthentications=password \
      -o BatchMode=yes root@<host>
root@<host>: Permission denied (publickey).
```

同时用密钥连一次，确认没把自己锁死。

### 3.5 回滚

```bash
rm /etc/ssh/sshd_config.d/00-hardening.conf
sshd -t && systemctl reload ssh
```

如果已经锁死：走云厂商控制台的 VNC / 救援模式，改回上面的文件。

---

## 4. 主机防火墙

### 4.1 现状检查

```bash
$ ufw status
Status: inactive

$ iptables -S | head -3
-P INPUT ACCEPT
-P FORWARD ACCEPT
-P OUTPUT ACCEPT
```

全开。此时唯一挡住扫描的是"服务恰好只监听了 127.0.0.1"，这是运气，不是策略。

### 4.2 启用（顺序不能反）

```bash
# 1) 先定默认策略
ufw default deny incoming
ufw default allow outgoing

# 2) 再放行必要端口 —— 必须在 enable 之前
ufw allow 22/tcp comment 'ssh'
ufw allow 80/tcp  comment 'http'

# 3) 最后才启用
ufw --force enable
```

如果先 `enable` 再放行 22，SSH 会立刻断，而且你没法再连上去放行。

### 4.3 验证

```bash
$ ufw status verbose
Status: active
Default: deny (incoming), allow (outgoing), disabled (routed)
To                         Action      From
--                         ------      ----
22/tcp                     ALLOW IN    Anywhere
80/tcp                     ALLOW IN    Anywhere
```

两个都要测：

```bash
ssh -i ~/.ssh/stormops_deploy root@<host> 'echo ok'   # 22 通
curl -s -o /dev/null -w '%{http_code}\n' http://<host>/ops/   # 80 通
```

### 4.4 为什么只开 80

因为所有后端都只监听回环：

```
0.0.0.0:80          nginx
127.0.0.1:8080      stormops
127.0.0.1:8081      stormsite
127.0.0.1:8428      victoria-metrics
127.0.0.1:8880      vmalert
127.0.0.1:9093      alertmanager
127.0.0.1:5432      postgresql
```

这是"两道锁"：服务本身只听回环是第一道，ufw 是第二道。任何一道单独失效，另一道还在。

如果以后要开 443，记得同时改两处：`ufw allow 443/tcp`，以及 Nginx 的 `listen 443 ssl`。

---

## 5. 还没做的

诚实列出来，这些是已知缺口而不是遗漏：

| 项 | 为什么重要 | 现状 |
| --- | --- | --- |
| fail2ban | 密钥登录下爆破无效，但大量连接仍会占用 sshd 资源 | 未安装 |
| 非 root 运维账号 | 日常操作不该用 root | 未建，当前全用 root |
| 登录审计（`lastb` / `auth.log` 巡检） | 发现异常尝试 | 未做定期巡检 |
| 出站白名单 | 防止被入侵后回连 C2 | ufw 默认放行全部出站 |
| 自动安全更新 | 内核/CVE 补丁 | 未启用 `unattended-upgrades` |

按优先级，下一个该做的是**非 root 账号 + sudo**，因为它同时改善安全和审计（谁在什么时候做了什么）。fail2ban 在纯密钥环境下收益不大。

---

## 6. 一句话总结

> **加固的每一个动作都可能把自己锁在门外，所以顺序永远是：先建立新通道并验证，再关闭旧通道。**

这条规则不只适用于 SSH，也适用于改防火墙、改 Nginx、改 WAF —— 任何"先断后路再修路"的操作，都该反过来。
