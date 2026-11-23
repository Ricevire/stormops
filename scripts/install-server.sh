#!/usr/bin/env bash
# StormOps 服务器端安装脚本。
# 前置：/tmp/deploy 下已经放好二进制、配置和 unit 文件（由 scripts/deploy.sh 上传）。
set -euo pipefail
export DEBIAN_FRONTEND=noninteractive
SRC=/tmp/deploy

say() { echo -e "\n\033[1;32m==> $*\033[0m"; }

say "1/9 创建系统用户"
for u in stormsite vmetrics alertmanager; do
  id -u "$u" >/dev/null 2>&1 || useradd -r -s /usr/sbin/nologin "$u"
done

say "2/9 创建目录"
mkdir -p /opt/stormops/bin /opt/stormops/sql
mkdir -p /opt/stormsite/bin /opt/stormsite/releases /opt/stormsite/stage
mkdir -p /var/lib/stormops/pgexport /var/lib/victoriametrics /var/lib/vmagent /var/lib/alertmanager
mkdir -p /var/log/stormops /var/log/stormsite
mkdir -p /etc/stormops /etc/vmagent /etc/vmalert /etc/alertmanager
chown -R vmetrics:vmetrics /var/lib/victoriametrics /var/lib/vmagent
chown -R alertmanager:alertmanager /var/lib/alertmanager
chown -R stormsite:stormsite /opt/stormsite /var/log/stormsite
chmod 755 /var/lib/stormops /var/lib/stormops/pgexport

say "3/9 安装二进制"
install -m 0755 "$SRC/stormops"    /opt/stormops/bin/stormops
install -m 0755 "$SRC/stormagent"  /opt/stormops/bin/stormagent
install -m 0755 "$SRC/stormsite"   /opt/stormsite/bin/stormsite
cp "$SRC"/releases/* /opt/stormsite/releases/ 2>/dev/null || true
chmod 755 /opt/stormsite/releases/* 2>/dev/null || true
chown -R stormsite:stormsite /opt/stormsite

say "4/9 安装配置"
install -m 0644 "$SRC/config.json"       /etc/stormops/config.json
install -m 0644 "$SRC/agent.json"        /etc/stormops/agent.json
install -m 0644 "$SRC/scrape.yml"        /etc/vmagent/scrape.yml
install -m 0644 "$SRC/rules.yml"         /etc/vmalert/rules.yml
install -m 0644 "$SRC/alertmanager.yml"  /etc/alertmanager/alertmanager.yml
install -m 0644 "$SRC/schema.sql"        /opt/stormops/sql/schema.sql
install -m 0644 "$SRC/capacity_report.sql" /opt/stormops/sql/capacity_report.sql
install -m 0755 "$SRC/load-pg.sh"        /opt/stormops/bin/load-pg.sh

say "5/9 安装 systemd 单元"
cp "$SRC"/units/*.service /etc/systemd/system/
systemctl daemon-reload

say "6/9 初始化 PostgreSQL"
su - postgres -c "psql -tAc \"SELECT 1 FROM pg_database WHERE datname='stormops'\"" | grep -q 1 \
  || su - postgres -c "createdb -O postgres stormops"
su - postgres -c "psql -q -d stormops -f /opt/stormops/sql/schema.sql"
# 给分析查询用的只读角色：报表/BI 接进来时不应该拿到写权限
su - postgres -c "psql -q -d stormops -c \"
  DO \\\$\\\$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname='stormops_ro') THEN
      CREATE ROLE stormops_ro LOGIN PASSWORD 'ro_$(head -c 6 /dev/urandom | base64 | tr -dc 'a-zA-Z0-9')';
    END IF;
  END \\\$\\\$;\"" || true
su - postgres -c "psql -q -d stormops -c 'GRANT USAGE ON SCHEMA ops TO stormops_ro'"
su - postgres -c "psql -q -d stormops -c 'GRANT SELECT ON ALL TABLES IN SCHEMA ops TO stormops_ro'"

say "7/9 启动监控组件"
systemctl enable --now victoria-metrics.service
systemctl enable --now alertmanager.service
systemctl enable --now vmagent.service
systemctl enable --now vmalert.service

say "8/9 启动业务服务"
systemctl enable --now stormsite.service
systemctl enable --now stormops.service
sleep 2
systemctl enable --now stormagent.service

say "9/9 配置 Nginx"
if nginx -V 2>&1 | grep -q with-http_auth_request_module; then
  echo "auth_request 模块存在，WAF 旁路可用"
else
  echo "!! 警告：nginx 未编译 auth_request 模块，WAF 链路不可用"
fi
rm -f /etc/nginx/sites-enabled/default
nginx -t
systemctl enable --now nginx

say "完成。等待 StormOps 生成并应用 Nginx 配置…"
sleep 3
# 这一步失败不该让整个部署算失败：配置应用有独立的回滚保护，可以稍后重试。
curl -s -m 15 -X POST http://127.0.0.1:8080/api/v1/gateway/apply | head -c 500 || echo "(gateway apply 未调用成功)"
echo
echo "--- 状态 ---"
for s in victoria-metrics vmagent vmalert alertmanager stormops stormagent stormsite nginx postgresql; do
  printf "%-20s %s\n" "$s" "$(systemctl is-active $s 2>/dev/null)"
done
free -m | head -2
