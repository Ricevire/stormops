#!/usr/bin/env bash
set -x
BIN=/opt/stormops/bin
mkdir -p "$BIN"
cd /tmp || exit 1
rm -rf /tmp/vmdl && mkdir -p /tmp/vmdl && cd /tmp/vmdl || exit 1

API=https://api.github.com/repos/VictoriaMetrics/VictoriaMetrics/releases/latest
curl -sL "$API" -o rel.json
echo "=== 可用单机版资产 ==="
jq -r '.assets[].name' rel.json | grep -E 'amd64' | grep -vE 'cluster|enterprise' | head -20

VM_URL=$(jq -r '.assets[].browser_download_url' rel.json | grep -E '/victoria-metrics-linux-amd64-v[0-9]+\.[0-9]+\.[0-9]+\.tar\.gz$' | head -1)
VU_URL=$(jq -r '.assets[].browser_download_url' rel.json | grep -E '/vmutils-linux-amd64-v[0-9]+\.[0-9]+\.[0-9]+\.tar\.gz$' | head -1)
echo "PICK_VM=$VM_URL"
echo "PICK_VU=$VU_URL"

if [ -n "$VM_URL" ]; then
  curl -sL "$VM_URL" -o vm.tar.gz
  mkdir -p vm && tar xzf vm.tar.gz -C vm
  cp "$(find vm -type f | head -1)" "$BIN/victoria-metrics"
fi

if [ -n "$VU_URL" ]; then
  curl -sL "$VU_URL" -o vmutils.tar.gz
  mkdir -p vu && tar xzf vmutils.tar.gz -C vu
  # 必须按准确文件名挑：vmutils 包里同时有 vmalert-prod 和 vmalert-tool-prod，
  # 用 'vmalert*' + head -1 会碰运气选到 vmalert-tool（它不认 -rule 参数，
  # 表现是 systemd 无限重启 + "flag provided but not defined: -rule"）。
  AG=$(find vu -type f -name 'vmagent-prod' | head -1); [ -n "$AG" ] && cp "$AG" "$BIN/vmagent"
  AL=$(find vu -type f -name 'vmalert-prod' | head -1); [ -n "$AL" ] && cp "$AL" "$BIN/vmalert"

  # 装完立刻自检：拿到的必须是 vmalert 本体，不是 vmalert-tool。
  if ! "$BIN/vmalert" -version 2>&1 | head -1 | grep -q '^vmalert-'; then
    echo "FATAL: $BIN/vmalert 不是 vmalert 本体，实际是: $("$BIN/vmalert" -version 2>&1 | head -1)"
    exit 1
  fi
fi

AM_URL=$(curl -sL https://api.github.com/repos/prometheus/alertmanager/releases/latest | jq -r '.assets[].browser_download_url' | grep -E 'linux-amd64\.tar\.gz$' | head -1)
echo "PICK_AM=$AM_URL"
if [ -n "$AM_URL" ]; then
  curl -sL "$AM_URL" -o am.tar.gz
  mkdir -p am && tar xzf am.tar.gz -C am
  AM=$(find am -type f -name 'alertmanager' | head -1); [ -n "$AM" ] && cp "$AM" "$BIN/alertmanager"
fi

chmod +x "$BIN"/* 2>/dev/null
echo "=== 结果 ==="
ls -lh "$BIN"
"$BIN/victoria-metrics" --version 2>&1 | head -3
"$BIN/vmagent" --version 2>&1 | head -2
"$BIN/alertmanager" --version 2>&1 | head -2
echo VMBIN_DONE
