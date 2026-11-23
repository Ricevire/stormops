#!/usr/bin/env bash
# 把 StormOps 状态快照加载进 PostgreSQL 分析库。
#
# 由 stormops-pgload.timer 每天 03:20 触发，也可以手工执行。
# 先全量导出 CSV 再 TRUNCATE + COPY：数据量在万级，
# 全量重载比增量对账简单得多，而且天然具备"任意时刻可重建"的能力。
set -euo pipefail
DIR=/var/lib/stormops/pgexport
mkdir -p "$DIR"
chmod 755 "$DIR"

/opt/stormops/bin/stormops -config /etc/stormops/config.json -export-csv "$DIR"
chmod 644 "$DIR"/*.csv

run() { su - postgres -c "psql -q -v ON_ERROR_STOP=1 -d stormops -c \"$1\""; }

run "TRUNCATE ops.cmdb_asset, ops.service, ops.deploy_release, ops.capacity_snapshot"
run "DELETE FROM ops.waf_audit"

su - postgres -c "psql -q -v ON_ERROR_STOP=1 -d stormops" <<SQL
\copy ops.cmdb_asset (id,hostname,ip,role,zone,os,cpu_cores,mem_gb,disk_gb,status,last_seen) FROM '$DIR/assets.csv' WITH (FORMAT csv, HEADER true)
\copy ops.service (name,domain,env,version,owner,health_path,updated_at) FROM '$DIR/services.csv' WITH (FORMAT csv, HEADER true)
\copy ops.deploy_release (id,service,version,prev_version,strategy,status,operator,started_at,ended_at,duration_sec) FROM '$DIR/releases.csv' WITH (FORMAT csv, HEADER true)
\copy ops.waf_audit (ts,ip,country,method,path,rule_id,rule_name,category,action,ua) FROM '$DIR/waf_audit.csv' WITH (FORMAT csv, HEADER true)
\copy ops.capacity_snapshot (volume,day,bytes) FROM '$DIR/capacity.csv' WITH (FORMAT csv, HEADER true)
ANALYZE ops.cmdb_asset;
ANALYZE ops.waf_audit;
SQL

su - postgres -c "psql -q -d stormops -c \"
  SELECT 'assets' AS t, count(*) FROM ops.cmdb_asset
  UNION ALL SELECT 'services', count(*) FROM ops.service
  UNION ALL SELECT 'releases', count(*) FROM ops.deploy_release
  UNION ALL SELECT 'waf_audit', count(*) FROM ops.waf_audit
  UNION ALL SELECT 'capacity', count(*) FROM ops.capacity_snapshot\""
