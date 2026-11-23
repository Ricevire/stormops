#!/usr/bin/env bash
set -x
export DEBIAN_FRONTEND=noninteractive

timedatectl set-timezone Asia/Shanghai || true

apt-get update -qq
apt-get install -y -qq postgresql postgresql-contrib nginx unzip ca-certificates jq curl

mkdir -p /opt/stormops/bin /opt/stormops/data /opt/stormops/logs /opt/stormops/releases /opt/stormops/conf
mkdir -p /opt/stormsite/bin /opt/stormsite/www

cd /tmp || exit 1
API=https://api.github.com/repos/VictoriaMetrics/VictoriaMetrics/releases/latest

VM_URL=$(curl -sL "$API" | jq -r '[.assets[]|select(.name|test("victoria-metrics-linux-amd64"))|.browser_download_url][0]')
VU_URL=$(curl -sL "$API" | jq -r '[.assets[]|select(.name|test("vmutils-linux-amd64"))|.browser_download_url][0]')
echo "VM_URL=$VM_URL"
echo "VU_URL=$VU_URL"

if [ -n "$VM_URL" ] && [ "$VM_URL" != "null" ]; then
  curl -sL "$VM_URL" -o vm.tar.gz && tar xzf vm.tar.gz -C /tmp/vmx 2>/dev/null || { mkdir -p /tmp/vmx && tar xzf vm.tar.gz -C /tmp/vmx; }
  find /tmp/vmx -type f -name 'victoria-metrics*' -exec cp {} /opt/stormops/bin/victoria-metrics \;
fi

if [ -n "$VU_URL" ] && [ "$VU_URL" != "null" ]; then
  mkdir -p /tmp/vux
  curl -sL "$VU_URL" -o vmutils.tar.gz && tar xzf vmutils.tar.gz -C /tmp/vux
  for b in vmagent vmalert vmctl; do
    find /tmp/vux -type f -name "${b}*" -exec cp {} /opt/stormops/bin/${b} \; 2>/dev/null
  done
  find /tmp/vux -type f -name 'vmagent*' -exec cp {} /opt/stormops/bin/vmagent \;
  find /tmp/vux -type f -name 'vmalert*' -exec cp {} /opt/stormops/bin/vmalert \;
fi

AM_URL=$(curl -sL https://api.github.com/repos/prometheus/alertmanager/releases/latest | jq -r '[.assets[]|select(.name|test("linux-amd64.tar.gz"))|.browser_download_url][0]')
echo "AM_URL=$AM_URL"
if [ -n "$AM_URL" ] && [ "$AM_URL" != "null" ]; then
  mkdir -p /tmp/amx
  curl -sL "$AM_URL" -o am.tar.gz && tar xzf am.tar.gz -C /tmp/amx
  find /tmp/amx -type f -name 'alertmanager' -exec cp {} /opt/stormops/bin/alertmanager \;
  find /tmp/amx -type f -name 'amtool' -exec cp {} /opt/stormops/bin/amtool \;
fi

chmod +x /opt/stormops/bin/* 2>/dev/null
systemctl disable --now nginx >/dev/null 2>&1
systemctl disable postgresql >/dev/null 2>&1

echo "=== BIN ==="
ls -lh /opt/stormops/bin/
echo "=== VER ==="
/opt/stormops/bin/victoria-metrics --version 2>&1 | head -3
/opt/stormops/bin/vmagent --version 2>&1 | head -2
/opt/stormops/bin/alertmanager --version 2>&1 | head -2
psql --version
nginx -v 2>&1
echo BOOTSTRAP_DONE
