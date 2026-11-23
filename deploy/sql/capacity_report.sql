-- StormOps 分析库常用查询
--
-- 用法：psql -U stormops -d stormops -f capacity_report.sql
--
-- 这些查询存在的意义：控制台能给"当前状态"，但给不了"趋势与归因"。
-- 排行榜、同环比、分位数这类问题，SQL 写出来是 10 行，Go 里写是 200 行且容易错。

\echo '=== 1. 存储增长与剩余可用天数（线性回归，与 StormOps 内部算法同源） ==='

WITH base AS (
    SELECT volume,
           day,
           bytes,
           -- 以每卷第一天为原点换算成"第几天"，避免日期直接参与回归
           (day - MIN(day) OVER (PARTITION BY volume)) AS x
    FROM ops.capacity_snapshot
),
fit AS (
    SELECT volume,
           -- regr_slope / regr_r2 是 PostgreSQL 内置的线性回归聚合函数，
           -- 用它做容量趋势比自己手写最小二乘稳，且能直接拿到拟合优度
           regr_slope(bytes, x) AS slope_per_day,
           regr_r2(bytes, x)    AS r2,
           MAX(bytes)           AS current_bytes
    FROM base
    GROUP BY volume
)
SELECT volume,
       pg_size_pretty(current_bytes::bigint)                        AS current,
       pg_size_pretty(slope_per_day::bigint)                        AS growth_per_day,
       round(r2::numeric, 3)                                        AS r2,
       CASE WHEN slope_per_day > 0
            THEN ceil((20e12 - current_bytes) / slope_per_day)
            ELSE NULL
       END                                                          AS days_to_20tb,
       CASE
           WHEN r2 > 0.95 THEN '可信（增长接近线性）'
           WHEN r2 > 0.80 THEN '参考（有波动，需结合业务口径）'
           ELSE '不可信（增长非线性，回归无意义）'
       END                                                          AS confidence
FROM fit
ORDER BY current_bytes DESC;

\echo ''
\echo '=== 2. 近 14 天日增量与 7 日移动平均（找异常导入） ==='

WITH d AS (
    SELECT volume,
           day,
           bytes,
           bytes - LAG(bytes) OVER (PARTITION BY volume ORDER BY day) AS delta_bytes
    FROM ops.capacity_snapshot
    WHERE day >= CURRENT_DATE - INTERVAL '14 days'
)
SELECT volume,
       day,
       pg_size_pretty(delta_bytes::bigint) AS daily_growth,
       pg_size_pretty(avg(delta_bytes) OVER (
           PARTITION BY volume ORDER BY day
           ROWS BETWEEN 6 PRECEDING AND CURRENT ROW
       )::bigint)                           AS ma7,
       -- 单日增量超过 7 日均值 2 倍 => 大概率是一次集中素材导入
       CASE WHEN delta_bytes > 2 * avg(delta_bytes) OVER (
                PARTITION BY volume ORDER BY day
                ROWS BETWEEN 6 PRECEDING AND CURRENT ROW)
            THEN '异常增长'
            ELSE ''
       END AS flag
FROM d
ORDER BY volume, day DESC;

\echo ''
\echo '=== 3. WAF 攻击来源排行（含首次/末次出现与被命中规则） ==='

SELECT ip,
       count(*)                                   AS hits,
       count(*) FILTER (WHERE action = 'block')   AS blocked,
       min(ts)                                    AS first_seen,
       max(ts)                                    AS last_seen,
       string_agg(DISTINCT rule_id, ',' ORDER BY rule_id) AS rules_hit,
       mode() WITHIN GROUP (ORDER BY ua)          AS typical_ua
FROM ops.waf_audit
WHERE ts >= now() - INTERVAL '24 hours'
GROUP BY ip
HAVING count(*) >= 2
ORDER BY hits DESC
LIMIT 10;

\echo ''
\echo '=== 4. 疑似误报识别：内网来源命中拦截规则 ==='
-- 内网 IP 命中 block 类规则，八成是业务自己的合法请求被规则误伤。
-- 这条查询是"每周安全例会"的固定输入。

SELECT ip,
       rule_id,
       rule_name,
       count(*)          AS hits,
       min(ts)           AS first_seen,
       max(path)         AS sample_path
FROM ops.waf_audit
WHERE ts >= now() - INTERVAL '7 days'
  AND action = 'block'
  AND (ip << '10.0.0.0/8' OR ip << '192.168.0.0/16' OR ip << '172.16.0.0/12')
GROUP BY ip, rule_id, rule_name
ORDER BY hits DESC;

\echo ''
\echo '=== 5. 发布健康度：按月成功率与耗时 P95 ==='

SELECT date_trunc('month', started_at)                             AS month,
       count(*)                                                    AS total,
       count(*) FILTER (WHERE status = 'success')                  AS success,
       count(*) FILTER (WHERE status = 'rolled_back')               AS rolled_back,
       round(100.0 * count(*) FILTER (WHERE status = 'success')
             / NULLIF(count(*), 0), 1)                             AS success_rate_pct,
       percentile_cont(0.95) WITHIN GROUP (ORDER BY duration_sec)   AS p95_seconds,
       max(duration_sec)                                           AS max_seconds
FROM ops.deploy_release
WHERE started_at IS NOT NULL
GROUP BY 1
ORDER BY 1 DESC;

\echo ''
\echo '=== 6. 连续失败发布（同一服务连续 rolled_back / failed） ==='
-- 连续失败说明问题不在"发布动作"而在"版本本身"，应该停下来查代码而不是重试。

WITH r AS (
    SELECT service, version, status, started_at,
           ROW_NUMBER() OVER (PARTITION BY service ORDER BY started_at) AS rn,
           CASE WHEN status IN ('failed', 'rolled_back') THEN 1 ELSE 0 END AS bad
    FROM ops.deploy_release
)
SELECT service, version, status, started_at
FROM r
WHERE bad = 1
ORDER BY service, started_at DESC;
