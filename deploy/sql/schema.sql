-- StormOps 分析库 Schema（PostgreSQL 16）
--
-- 定位：只读分析库，不承载在线请求。
-- 数据来源：stormops -export-csv 导出的 CSV，由 systemd timer 每天加载一次。
--
-- 之所以这么设计：
--   1. 控制台的读路径不能依赖 PG —— PG 做一次大查询把 IO 打满，不该连带控制台卡死；
--   2. 分析类查询（排行、同比、窗口函数）在 PG 上写比在 Go 里写舒服得多；
--   3. 数据可重放：CSV 留着，任何时候都能重建整张表，不依赖备份链。

CREATE SCHEMA IF NOT EXISTS ops;

-- ---------------------------------------------------------------- CMDB
CREATE TABLE IF NOT EXISTS ops.cmdb_asset (
    id         text PRIMARY KEY,
    hostname   text NOT NULL,
    ip         inet,
    role       text,
    zone       text,
    os         text,
    cpu_cores  int,
    mem_gb     int,
    disk_gb    int,
    status     text,
    last_seen  timestamptz,
    loaded_at  timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_asset_role ON ops.cmdb_asset (role);
CREATE INDEX IF NOT EXISTS idx_asset_status ON ops.cmdb_asset (status) WHERE status <> 'online';

CREATE TABLE IF NOT EXISTS ops.service (
    name        text PRIMARY KEY,
    domain      text,
    env         text,
    version     text,
    owner       text,
    health_path text,
    updated_at  timestamptz,
    loaded_at   timestamptz NOT NULL DEFAULT now()
);

-- ---------------------------------------------------------------- 发布
CREATE TABLE IF NOT EXISTS ops.deploy_release (
    id           text PRIMARY KEY,
    service      text NOT NULL,
    version      text NOT NULL,
    prev_version text,
    strategy     text,
    status       text,
    operator     text,
    started_at   timestamptz,
    ended_at     timestamptz,
    duration_sec int
);

-- 发布成功率是 SRE 的核心指标之一，按月的统计直接依赖这个索引
CREATE INDEX IF NOT EXISTS idx_release_started ON ops.deploy_release (started_at DESC);
CREATE INDEX IF NOT EXISTS idx_release_status ON ops.deploy_release (status, started_at DESC);

-- ---------------------------------------------------------------- WAF 审计
-- 按月分区。这里用 DEFAULT 分区兜底，保证忘建分区时写入不会失败；
-- 但要注意：DEFAULT 分区会让"按期裁剪"变慢（优化器必须扫它），
-- 所以生产上应提前 3 个月建好分区，DEFAULT 只作为安全网。
CREATE TABLE IF NOT EXISTS ops.waf_audit (
    ts        timestamptz NOT NULL,
    ip        inet,
    country   text,
    method    text,
    path      text,
    rule_id   text,
    rule_name text,
    category  text,
    action    text,
    ua        text
) PARTITION BY RANGE (ts);

CREATE TABLE IF NOT EXISTS ops.waf_audit_default PARTITION OF ops.waf_audit DEFAULT;

CREATE INDEX IF NOT EXISTS idx_waf_ts ON ops.waf_audit (ts DESC);
CREATE INDEX IF NOT EXISTS idx_waf_ip ON ops.waf_audit (ip, ts DESC);

-- 装载用的暂存表已不需要：导出时时间戳统一写成 RFC3339，
-- psql 的 \copy 可以直接写进 timestamptz，省掉一次中转。

-- ---------------------------------------------------------------- 容量快照
CREATE TABLE IF NOT EXISTS ops.capacity_snapshot (
    volume   text NOT NULL,
    day      date NOT NULL,
    bytes    numeric NOT NULL,
    loaded_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (volume, day)
);

ANALYZE ops.cmdb_asset;
