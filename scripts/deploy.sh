#!/usr/bin/env bash
# 本地构建并部署到服务器。
#   ./scripts/deploy.sh              # 全量部署
#   HOST=root@1.2.3.4 ./scripts/deploy.sh
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
HOST="${HOST:?请先设置目标主机，例如 HOST=root@1.2.3.4 ./scripts/deploy.sh}"
KEY="${KEY:-$HOME/.ssh/stormops_deploy}"
KNOWN="$ROOT/.known_hosts"
OUT="$ROOT/.deploy"
VERSION="${VERSION:-$(date +%Y.%m.%d-%H%M)}"

SSHOPT=(-i "$KEY" -o UserKnownHostsFile="$KNOWN" -o StrictHostKeyChecking=accept-new -o ConnectTimeout=15)

say() { echo -e "\n\033[1;33m==> $*\033[0m"; }

say "清理构建目录"
rm -rf "$OUT"
mkdir -p "$OUT/releases" "$OUT/units"

say "交叉编译 linux/amd64（版本 $VERSION）"
cd "$ROOT"
export CGO_ENABLED=0 GOOS=linux GOARCH=amd64
# 注意：这里必须用相对路径。Windows 上的 go.exe 是原生程序，
# 不认 MSYS 的 /c/Users/... 风格路径，会把它当成 C:\c\Users\... 写出去。
go build -trimpath -ldflags "-s -w -X main.version=$VERSION" -o .deploy/stormops   ./cmd/stormops
go build -trimpath -ldflags "-s -w -X main.version=$VERSION" -o .deploy/stormagent ./cmd/stormagent
go build -trimpath -ldflags "-s -w -X main.version=v1.0.0"   -o .deploy/stormsite  ./cmd/stormsite
ls -lh .deploy/stormops .deploy/stormagent .deploy/stormsite | awk '{print $9, $5}'

say "构建发布产物（含一个故意坏的版本，用于演示自动回滚）"
go build -trimpath -ldflags "-s -w -X main.version=v1.1.0"     -o .deploy/releases/v1.1.0     ./cmd/stormsite
go build -trimpath -ldflags "-s -w -X main.version=v1.2.0-bad" -o .deploy/releases/v1.2.0-bad ./cmd/stormsite
ls -1 .deploy/releases

say "整理待上传文件"
cp "$ROOT/configs/stormops.json"          "$OUT/config.json"
cp "$ROOT/configs/agent.json"             "$OUT/agent.json"
cp "$ROOT/deploy/vmalert/scrape.yml"      "$OUT/scrape.yml"
cp "$ROOT/deploy/vmalert/rules.yml"       "$OUT/rules.yml"
cp "$ROOT/deploy/alertmanager/alertmanager.yml" "$OUT/alertmanager.yml"
cp "$ROOT/deploy/sql/schema.sql"          "$OUT/schema.sql"
cp "$ROOT/deploy/sql/capacity_report.sql" "$OUT/capacity_report.sql"
cp "$ROOT/scripts/load-pg.sh"             "$OUT/load-pg.sh"
cp "$ROOT/scripts/install-server.sh"      "$OUT/install-server.sh"
cp "$ROOT"/deploy/systemd/*.service        "$OUT/units/"

tar czf "$OUT/pkg.tgz" -C "$OUT" \
  stormops stormagent stormsite releases units \
  config.json agent.json scrape.yml rules.yml alertmanager.yml \
  schema.sql capacity_report.sql load-pg.sh install-server.sh

say "上传到 $HOST"
scp "${SSHOPT[@]}" "$OUT/pkg.tgz" "$HOST:/tmp/stormops-pkg.tgz"

say "远端安装"
ssh "${SSHOPT[@]}" "$HOST" "rm -rf /tmp/deploy && mkdir -p /tmp/deploy && tar xzf /tmp/stormops-pkg.tgz -C /tmp/deploy && bash /tmp/deploy/install-server.sh" 2>&1 | tail -70

# 部署后必须校验，否则会出现"脚本跑完了但服务器还是旧二进制"这种
# 最难查的问题：本地改了逻辑、远端行为没变，人只会怀疑代码写错了。
say "校验远端二进制与本地一致"
LOCAL_MD5=$(md5sum "$OUT/stormops" | awk '{print $1}')
REMOTE_MD5=$(ssh "${SSHOPT[@]}" "$HOST" "md5sum /opt/stormops/bin/stormops" | awk '{print $1}')
echo "local=$LOCAL_MD5 remote=$REMOTE_MD5"
[ "$LOCAL_MD5" = "$REMOTE_MD5" ] || { echo "!! 二进制不一致，部署未生效"; exit 1; }

say "校验服务状态"
ssh "${SSHOPT[@]}" "$HOST" "for u in stormops stormagent stormsite victoria-metrics vmagent vmalert alertmanager nginx; do printf '%-18s %s\n' \$u \$(systemctl is-active \$u 2>/dev/null); done"

say "部署结束"
