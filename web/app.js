// StormOps 控制台
// 无框架，纯 DOM 操作。原因：整个前端只有 7 个视图，
// 引入构建工具链会让"单二进制部署"这个优势消失。

const $ = (s) => document.querySelector(s);
const $$ = (s) => Array.from(document.querySelectorAll(s));

const TABS = {
  overview: { title: "总览", sub: "基础设施健康度一览" },
  assets: { title: "资产台账", sub: "CMDB · 服务器 / 存储 / 网络" },
  deploy: { title: "服务与发布", sub: "发布编排 · 灰度 · 自动回滚" },
  alerts: { title: "告警", sub: "规则引擎 · 静默 · 认领" },
  capacity: { title: "容量规划", sub: "存储增长预测 · 带宽水位" },
  gateway: { title: "网关与 WAF", sub: "Nginx 配置生成 · 安全策略灰度" },
  docs: { title: "文档与 SOP", sub: "操作手册 · 故障复盘 · 架构决策" },
};

const app = {
  tab: "overview",
  data: {},
  timer: null,
};

// ---------- 基础工具 ----------

// 控制台有两种挂载方式：
//   1. 直接访问 stormops（本地开发）—— 页面在 "/"
//   2. 生产环境挂在 Nginx 的 "/ops/" 前缀后面
// 所以接口地址不能写死成 "/api/v1/..."，否则在 /ops/ 下会打到业务站上，
// 拿回一段 HTML 而不是 JSON（这个坑的真实表现是"整页无数据"）。
const MOUNT = (() => {
  let p = location.pathname;
  if (!p.endsWith("/")) p = p.replace(/\/[^\/]*$/, "/");
  return p;
})();

function apiURL(path) {
  return MOUNT + String(path).replace(/^\//, "");
}

async function api(path, opt = {}) {
  const res = await fetch(apiURL(path), {
    headers: { "Content-Type": "application/json" },
    ...opt,
    body: opt.body ? JSON.stringify(opt.body) : undefined,
  });
  const text = await res.text();
  let json = null;
  try {
    json = text ? JSON.parse(text) : null;
  } catch (e) {
    throw new Error("响应不是合法 JSON: " + text.slice(0, 120));
  }
  if (!res.ok) throw new Error((json && json.error) || `HTTP ${res.status}`);
  return json;
}

function toast(msg, isErr) {
  const el = $("#toast");
  el.textContent = msg;
  el.className = "toast show" + (isErr ? " err" : "");
  clearTimeout(el._t);
  el._t = setTimeout(() => (el.className = "toast"), 3200);
}

const nf = (v, d = 1) =>
  v === null || v === undefined || Number.isNaN(Number(v)) ? "—" : Number(v).toFixed(d);

function bytes(b) {
  if (!b) return "—";
  const u = ["B", "KB", "MB", "GB", "TB", "PB"];
  let i = 0;
  let v = b;
  while (v >= 1024 && i < u.length - 1) {
    v /= 1024;
    i++;
  }
  return v.toFixed(1) + " " + u[i];
}

function hhmm(ts) {
  if (!ts) return "—";
  return new Date(ts * 1000).toLocaleTimeString("zh-CN", {
    hour: "2-digit",
    minute: "2-digit",
  });
}

function dateTime(ts) {
  if (!ts) return "—";
  const d = new Date(ts * 1000);
  return `${d.getMonth() + 1}/${d.getDate()} ${String(d.getHours()).padStart(2, "0")}:${String(
    d.getMinutes()
  ).padStart(2, "0")}`;
}

function ago(ts) {
  if (!ts) return "—";
  const s = Math.floor(Date.now() / 1000 - ts);
  if (s < 60) return s + " 秒前";
  if (s < 3600) return Math.floor(s / 60) + " 分钟前";
  if (s < 86400) return Math.floor(s / 3600) + " 小时前";
  return Math.floor(s / 86400) + " 天前";
}

function esc(s) {
  return String(s == null ? "" : s).replace(/[&<>"']/g, (c) => ({
    "&": "&amp;",
    "<": "&lt;",
    ">": "&gt;",
    '"': "&quot;",
    "'": "&#39;",
  }[c]));
}

function barCell(v, cls) {
  const w = Math.max(0, Math.min(100, v || 0));
  return `<div class="row"><div class="bar ${cls || ""}" style="width:64px"><i style="width:${w}%"></i></div><span class="num muted" style="font-size:11.5px">${nf(v, 1)}%</span></div>`;
}

// ---------- 图表 ----------

function chart(series, opts = {}) {
  const H = opts.height || 176;
  const W = 640;
  const padL = 36;
  const padR = 10;
  const padT = 10;
  const padB = 20;

  const all = series.flatMap((s) => s.points || []);
  if (!all.length) return `<div class="empty">暂无数据</div>`;

  const minT = Math.min(...all.map((p) => p.ts));
  const maxT = Math.max(...all.map((p) => p.ts));
  let maxV = opts.yMax;
  if (maxV == null) {
    maxV = Math.max(...all.map((p) => p.value));
    maxV = maxV <= 0 ? 10 : maxV * 1.15;
  }
  const minV = 0;

  const x = (t) =>
    padL + (maxT === minT ? 0 : ((t - minT) / (maxT - minT)) * (W - padL - padR));
  const y = (v) =>
    padT + (1 - (Math.max(minV, Math.min(maxV, v)) - minV) / (maxV - minV)) * (H - padT - padB);

  let grid = "";
  for (let i = 0; i <= 3; i++) {
    const gy = padT + ((H - padT - padB) / 3) * i;
    const gv = maxV - (maxV / 3) * i;
    grid += `<line x1="${padL}" y1="${gy}" x2="${W - padR}" y2="${gy}" stroke="#eef0f2" stroke-width="1"/>`;
    grid += `<text x="${padL - 6}" y="${gy + 3}" text-anchor="end" font-size="9.5" fill="#9ca3af">${opts.fmtY ? opts.fmtY(gv) : gv.toFixed(0)}</text>`;
  }

  let paths = "";
  series.forEach((s) => {
    const pts = (s.points || []).filter((p) => p.value != null);
    if (!pts.length) return;
    const d = pts.map((p, i) => `${i ? "L" : "M"}${x(p.ts).toFixed(1)},${y(p.value).toFixed(1)}`).join(" ");
    if (s.area !== false) {
      const areaD =
        d +
        ` L${x(pts[pts.length - 1].ts).toFixed(1)},${(H - padB).toFixed(1)} L${x(pts[0].ts).toFixed(1)},${(H - padB).toFixed(1)} Z`;
      paths += `<path d="${areaD}" fill="${s.color}" opacity="0.07"/>`;
    }
    paths += `<path d="${d}" fill="none" stroke="${s.color}" stroke-width="1.7" stroke-linejoin="round" stroke-linecap="round" ${s.dash ? 'stroke-dasharray="4 3"' : ""}/>`;
  });

  let labels = "";
  const ticks = 5;
  const fx = opts.fmtX || hhmm;
  for (let i = 0; i < ticks; i++) {
    const t = minT + ((maxT - minT) / (ticks - 1)) * i;
    labels += `<text x="${x(t).toFixed(1)}" y="${H - 5}" text-anchor="${i === 0 ? "start" : i === ticks - 1 ? "end" : "middle"}" font-size="9.5" fill="#9ca3af">${fx(Math.round(t))}</text>`;
  }

  return `<div class="chart-wrap"><svg class="chart" viewBox="0 0 ${W} ${H}" preserveAspectRatio="none">${grid}${paths}${labels}</svg></div>`;
}

// ---------- 路由 ----------

function route() {
  const hash = location.hash.replace("#/", "") || "overview";
  app.tab = TABS[hash] ? hash : "overview";
  $$("#nav a").forEach((a) => a.classList.toggle("active", a.dataset.tab === app.tab));
  $("#page-title").textContent = TABS[app.tab].title;
  $("#page-sub").textContent = TABS[app.tab].sub;
  render();
}

async function render() {
  const view = $("#view");
  view.innerHTML = `<div class="empty">加载中…</div>`;
  try {
    if (app.tab === "overview") await renderOverview(view);
    else if (app.tab === "assets") await renderAssets(view);
    else if (app.tab === "deploy") await renderDeploy(view);
    else if (app.tab === "alerts") await renderAlerts(view);
    else if (app.tab === "capacity") await renderCapacity(view);
    else if (app.tab === "gateway") await renderGateway(view);
    else renderDocs(view);
  } catch (e) {
    view.innerHTML = `<div class="empty">加载失败：${esc(e.message)}</div>`;
  }
}

// ---------- 总览 ----------

async function renderOverview(view) {
  const d = await api("/api/v1/overview");
  app.data.overview = d;

  $("#sf-env").textContent = d.env || "生产环境";
  $("#sf-ver").textContent = "版本 " + (d.version || "dev");
  $("#sf-host").textContent = d.host || "";

  const st = (d.storage || [])[0];
  const alertCls = d.alerts.critical > 0 ? "color:var(--critical)" : "";
  const recent = d.lastRelease;

  const html = `
  <div class="grid kpis">
    <div class="kpi">
      <div class="label">纳管资产</div>
      <div class="val">${d.assets.online}<small>/${d.assets.total}</small></div>
      <div class="foot">${
        d.assets.offline > 0
          ? `<span style="color:var(--critical)">${d.assets.offline} 台心跳超时</span>`
          : d.assets.pending > 0
          ? `${d.assets.pending} 台已登记待接入`
          : "全部实时在线"
      }</div>
    </div>
    <div class="kpi">
      <div class="label">服务健康</div>
      <div class="val">${d.services.healthy}<small>/${d.services.total}</small></div>
      <div class="foot">探测间隔 15s · 判定建议</div>
    </div>
    <div class="kpi">
      <div class="label">活跃告警</div>
      <div class="val" style="${alertCls}">${d.alerts.firing}</div>
      <div class="foot">${d.alerts.critical} 严重 · ${d.alerts.warning} 警告${d.alerts.acked ? ` · ${d.alerts.acked} 已认领` : ""}</div>
    </div>
    <div class="kpi">
      <div class="label">24h WAF 拦截</div>
      <div class="val">${d.waf.blocked}<small>/${d.waf.total}</small></div>
      <div class="foot">命中 ${d.waf.total} 次，拦截率 ${d.waf.total ? ((d.waf.blocked / d.waf.total) * 100).toFixed(0) : 0}%</div>
    </div>
  </div>

  <div class="grid two-col" style="margin-bottom:14px">
    <div class="card">
      <div class="card-h">
        <h2>集群负载（最近 6 小时）</h2>
        <span class="hint">5 分钟聚合 · ${d.series.cpu_pct ? d.series.cpu_pct.length : 0} 个点</span>
      </div>
      ${chart(
        [
          { name: "CPU", color: "#0f766e", points: d.series.cpu_pct || [] },
          { name: "内存", color: "#7c3aed", points: d.series.mem_used_pct || [] },
        ],
        { height: 178, yMax: 100, fmtY: (v) => v.toFixed(0) + "%" }
      )}
      <div class="chart-legend">
        <span><i style="background:#0f766e"></i>CPU 使用率</span>
        <span><i style="background:#7c3aed"></i>内存使用率</span>
      </div>
    </div>

    <div class="card">
      <div class="card-h"><h2>出口带宽</h2><span class="hint">Mbps</span></div>
      ${chart([{ name: "带宽", color: "#1d4ed8", points: d.series.bw_mbps || [] }], {
        height: 178,
        fmtY: (v) => v.toFixed(0),
      })}
      <div class="chart-legend">
        <span><i style="background:#1d4ed8"></i>入出向合计</span>
        <span class="muted">凌晨低谷、工作时段抬升</span>
      </div>
    </div>
  </div>

  <div class="grid two-col">
    <div class="card">
      <div class="card-h">
        <h2>最近一次发布</h2>
        <span class="hint">${recent ? dateTime(recent.startedAt) : "—"}</span>
      </div>
      <div class="card-b">
        ${
          recent
            ? `<div class="row" style="margin-bottom:12px">
                <span class="strong">${esc(recent.service)}</span>
                <span class="badge ${recent.status === "success" ? "ok" : recent.status === "running" ? "info" : "critical"}">${
                { success: "发布成功", running: "发布中", failed: "发布失败", rolled_back: "已回滚" }[recent.status] || recent.status
              }</span>
                <span class="spacer"></span>
                <span class="mono muted">${esc(recent.prevVersion || "—")} → ${esc(recent.version)}</span>
              </div>
              <div class="steps">${renderSteps(recent.steps)}</div>`
            : `<div class="empty">还没有发布记录</div>`
        }
      </div>
    </div>

    <div class="card">
      <div class="card-h"><h2>攻击来源 Top</h2><span class="hint">最近 24 小时</span></div>
      <div class="card-b tight">
        ${
          (d.topAttackers || []).length
            ? (d.topAttackers || [])
                .map(
                  (a) => `<div class="list-row">
                    <span class="mono">${esc(a.ip)}</span>
                    <span class="spacer"></span>
                    <span class="num muted" style="font-size:11.5px">${ago(a.last)}</span>
                    <span class="badge warning">${a.count} 次</span>
                  </div>`
                )
                .join("")
            : `<div class="empty">暂无攻击记录</div>`
        }
      </div>
    </div>
  </div>

  <div class="card" style="margin-top:14px">
    <div class="card-h">
      <h2>存储容量水位</h2>
      <span class="hint">线性回归预测 · 双判据（水位 + 剩余天数）</span>
    </div>
    <div class="card-b tight">
      <table>
        <thead><tr><th>卷</th><th>已用</th><th>总量</th><th>水位</th><th>日增长</th><th>预计耗尽</th><th>状态</th></tr></thead>
        <tbody>
          ${(d.storage || [])
            .map(
              (v) => `<tr>
                <td class="strong">${esc(v.volume)}</td>
                <td class="mono">${bytes(v.current)}</td>
                <td class="mono muted">${bytes(v.capacity)}</td>
                <td>${barCell(v.usedPct, v.status === "critical" ? "crit" : v.status === "warn" ? "warn" : "")}</td>
                <td class="mono">${bytes(v.growthPerDay)}/天</td>
                <td class="mono">${v.runwayDate || "—"}<span class="muted"> (${v.daysLeft >= 3650 ? ">10年" : v.daysLeft + "天"})</span></td>
                <td><span class="badge ${v.status === "critical" ? "critical" : v.status === "warn" ? "warning" : "ok"}">${
                { critical: "危急", warn: "预警", ok: "正常" }[v.status]
              }</span></td>
              </tr>`
            )
            .join("")}
        </tbody>
      </table>
    </div>
  </div>`;

  view.innerHTML = html;
}

function renderSteps(steps) {
  return (steps || [])
    .filter((s) => s.status !== "skipped")
    .map(
      (s) => `<div class="step ${s.status}">
        <div class="ic"></div>
        <div>
          <div class="name">${STEP_LABEL[s.name] || s.name}</div>
          ${s.detail ? `<div class="detail">${esc(s.detail)}</div>` : ""}
        </div>
      </div>`
    )
    .join("");
}

const STEP_LABEL = {
  precheck: "前置校验",
  backup: "备份当前版本",
  stage: "暂存新版本",
  verify: "产物校验",
  switch: "切换并重启",
  health: "健康检查",
  rollback: "自动回滚",
};

// ---------- 资产 ----------

async function renderAssets(view) {
  const assets = await api("/api/v1/assets");
  const roleName = { edge: "网关", web: "业务", db: "数据库", storage: "存储", worker: "计算" };
  view.innerHTML = `
  <div class="card">
    <div class="card-h"><h2>服务器台账</h2><span class="hint">共 ${assets.length} 台 · 心跳超时 120s 判定离线</span></div>
    <div class="card-b tight">
      <table>
        <thead><tr><th>主机名</th><th>角色</th><th>IP</th><th>系统</th><th>规格</th><th>磁盘水位</th><th>状态</th><th>最后心跳</th></tr></thead>
        <tbody>
          ${assets
            .map(
              (a) => `<tr>
                <td class="strong mono">${esc(a.hostname)}</td>
                <td><span class="badge accent">${roleName[a.role] || a.role}</span></td>
                <td class="mono">${esc(a.ip)}</td>
                <td class="muted">${esc(a.os)}</td>
                <td class="mono">${a.cpuCores}C / ${a.memGb}G / ${a.diskGb}G</td>
                <td class="muted">—</td>
                <td><span class="badge ${
                  a.status === "online" ? "ok" : a.status === "pending" ? "muted" : "critical"
                }">${a.status === "online" ? "在线" : a.status === "pending" ? "待接入" : "离线"}</span></td>
                <td class="muted">${a.lastSeen ? ago(a.lastSeen) : "—"}</td>
              </tr>`
            )
            .join("")}
        </tbody>
      </table>
    </div>
  </div>
  <p class="muted" style="margin-top:10px;font-size:12px">
    说明：资产由 stormagent 心跳自动维护，不依赖人工录入。手工录入的台账一定会过期，这是我们不做的原因。
  </p>`;
}

// ---------- 发布 ----------

async function renderDeploy(view) {
  const [svc, rel, arts] = await Promise.all([
    api("/api/v1/services"),
    api("/api/v1/releases"),
    api("/api/v1/artifacts").catch(() => ({ artifacts: [] })),
  ]);
  app.data.services = svc;
  const running = (rel.releases || []).find((r) => r.status === "running");

  view.innerHTML = `
  <div class="grid three-col" style="margin-bottom:14px">
    ${svc
      .map(
        (s) => `<div class="card">
          <div class="card-b">
            <div class="row"><span class="strong">${esc(s.name)}</span><span class="spacer"></span>
              <span class="badge ${s.up ? "ok" : "critical"}">${s.up ? "运行中" : "不可用"}</span></div>
            <div class="muted mono" style="font-size:11.5px;margin-top:4px">${esc(s.domain)}</div>
            <div class="row" style="margin-top:12px;gap:18px">
              <div><div class="muted" style="font-size:11px">当前版本</div><div class="mono strong">${esc(s.version || "—")}</div></div>
              <div><div class="muted" style="font-size:11px">6h 可用率</div><div class="mono strong">${nf(s.availability, 2)}%</div></div>
              <div><div class="muted" style="font-size:11px">P95 延迟</div><div class="mono strong">${nf(s.p95Latency, 0)}ms</div></div>
            </div>
          </div>
        </div>`
      )
      .join("")}
  </div>

  <div class="grid two-col" style="margin-bottom:14px">
    <div class="card">
      <div class="card-h"><h2>发起发布</h2><span class="hint">${running ? "有发布进行中" : "健康检查连续 3 次通过才判定成功"}</span></div>
      <div class="card-b">
        <div class="row" style="flex-wrap:wrap;gap:10px">
          <label style="font-size:12px;color:var(--text-2)">产物</label>
          <select id="art-sel" style="min-width:240px">
            ${
              (arts.artifacts || []).length
                ? (arts.artifacts || [])
                    .map((a) => `<option value="${esc(a.path)}" data-v="${esc(a.version)}">${esc(a.name)} · ${bytes(a.size)}</option>`)
                    .join("")
                : `<option value="">产物目录为空：${esc(arts.dir || "未配置")}</option>`
            }
          </select>
          <button class="btn primary" id="btn-deploy" ${running ? "disabled" : ""}>发布</button>
        </div>
        ${
          (arts.artifacts || []).some((a) => a.broken)
            ? `<p class="muted" style="font-size:11.5px;margin:10px 0 0">
                提示：列表里有一个故意构建的"坏版本"，用于演示健康检查失败后的自动回滚。
               </p>`
            : ""
        }
      </div>
    </div>

    <div class="card">
      <div class="card-h"><h2>当前执行进度</h2><span class="hint">${running ? "进行中" : "空闲"}</span></div>
      <div class="card-b">${
        running ? `<div class="steps">${renderSteps(running.steps)}</div>` : `<div class="empty">没有正在进行的发布</div>`
      }</div>
    </div>
  </div>

  <div class="card">
    <div class="card-h"><h2>发布历史</h2><span class="hint">保留最近 100 条</span></div>
    <div class="card-b tight">
      <table>
        <thead><tr><th>时间</th><th>服务</th><th>版本</th><th>策略</th><th>状态</th><th>耗时</th><th>操作人</th></tr></thead>
        <tbody>
          ${(rel.releases || [])
            .map(
              (r) => `<tr>
                <td class="muted mono">${dateTime(r.startedAt)}</td>
                <td class="strong">${esc(r.service)}</td>
                <td class="mono">${esc(r.prevVersion || "—")} → ${esc(r.version)}</td>
                <td class="muted">${r.strategy === "canary" ? "灰度" : "滚动"}</td>
                <td><span class="badge ${r.status === "success" ? "ok" : r.status === "running" ? "info" : r.status === "rolled_back" ? "warning" : "critical"}">${
                { success: "成功", running: "进行中", failed: "失败", rolled_back: "已回滚" }[r.status] || r.status
              }</span></td>
                <td class="mono muted">${r.endedAt ? r.endedAt - r.startedAt + "s" : "—"}</td>
                <td class="muted">${esc(r.operator)}</td>
              </tr>`
            )
            .join("") || `<tr><td colspan="7"><div class="empty">暂无记录</div></td></tr>`}
        </tbody>
      </table>
    </div>
  </div>`;

  const btn = $("#btn-deploy");
  if (btn) {
    btn.onclick = async () => {
      const sel = $("#art-sel");
      const opt = sel.options[sel.selectedIndex];
      if (!opt) return toast("没有可用产物", true);
      btn.disabled = true;
      btn.textContent = "发布中…";
      try {
        await api("/api/v1/releases", {
          method: "POST",
          body: {
            service: (svc[0] && svc[0].name) || "官网/课程站",
            version: opt.dataset.v,
            artifact: opt.value,
            strategy: "rolling",
            operator: "oncall",
          },
        });
        toast("发布已启动，正在执行编排");
        // 轮询进度
        let n = 0;
        const poll = setInterval(async () => {
          n++;
          const r = await api("/api/v1/releases");
          const cur = r.releases.find((x) => x.status === "running") || r.releases[0];
          const done = !r.releases.some((x) => x.status === "running");
          if (app.tab === "deploy") {
            const holder = document.querySelector(".card-b .steps");
            if (holder && cur) holder.outerHTML = `<div class="steps">${renderSteps(cur.steps)}</div>`;
          }
          if (done || n > 40) {
            clearInterval(poll);
            if (cur) toast(`发布结束：${cur.status === "success" ? "成功" : cur.status === "rolled_back" ? "已自动回滚" : "失败"}`);
            render();
          }
        }, 1200);
      } catch (e) {
        toast(e.message, true);
        btn.disabled = false;
        btn.textContent = "发布";
      }
    };
  }
}

// ---------- 告警 ----------

async function renderAlerts(view) {
  const [al, rules, sil] = await Promise.all([
    api("/api/v1/alerts"),
    api("/api/v1/rules"),
    api("/api/v1/silences"),
  ]);

  view.innerHTML = `
  <div class="card" style="margin-bottom:14px">
    <div class="card-h"><h2>活跃告警</h2><span class="hint">${al.active.length} 条</span></div>
    <div class="card-b tight">
      ${
        al.active.length
          ? `<table>
        <thead><tr><th>级别</th><th>规则</th><th>对象</th><th>当前值</th><th>阈值</th><th>持续</th><th>状态</th><th></th></tr></thead>
        <tbody>
        ${al.active
          .map(
            (a) => `<tr>
              <td><span class="badge ${a.severity}">${{ critical: "严重", warning: "警告", info: "提示" }[a.severity] || a.severity}</span></td>
              <td class="strong">${esc(a.ruleName)}</td>
              <td class="mono">${esc(a.host)}</td>
              <td class="mono">${nf(a.value, 1)}</td>
              <td class="mono muted">${a.threshold}</td>
              <td class="muted">${ago(a.startedAt)}</td>
              <td>${a.status === "acked" ? `<span class="badge muted">${esc(a.ackedBy)} 已认领</span>` : `<span class="badge critical">待处理</span>`}</td>
              <td>${a.status === "firing" ? `<button class="btn sm" onclick="ackAlert('${a.id}')">认领</button>` : ""}</td>
            </tr>`
          )
          .join("")}
        </tbody></table>`
          : `<div class="empty">没有活跃告警。规则引擎每 15 秒评估一次。</div>`
      }
    </div>
  </div>

  <div class="grid two-col">
    <div class="card">
      <div class="card-h"><h2>告警规则</h2><span class="hint">${rules.length} 条 · 可导出为 vmalert</span></div>
      <div class="card-b tight">
        <table>
          <thead><tr><th>规则</th><th>条件</th><th>持续</th><th>级别</th><th>SOP</th></tr></thead>
          <tbody>
          ${rules
            .map(
              (r) => `<tr>
                <td class="strong">${esc(r.name)}</td>
                <td class="mono">${esc(r.metric)} ${esc(r.op)} ${r.threshold}</td>
                <td class="muted">${r.forSec}s</td>
                <td><span class="badge ${r.severity}">${{ critical: "严重", warning: "警告", info: "提示" }[r.severity]}</span></td>
                <td class="muted mono" style="font-size:11px">${esc(r.runbook || "—")}</td>
              </tr>`
            )
            .join("")}
          </tbody>
        </table>
      </div>
    </div>

    <div class="card">
      <div class="card-h"><h2>静默</h2><span class="hint">${sil.length} 条生效中</span></div>
      <div class="card-b tight">
        ${
          sil.length
            ? sil
                .map(
                  (s) => `<div class="list-row">
                    <div>
                      <div class="mono strong">${esc(s.host)} / ${esc(s.rule)}</div>
                      <div class="muted" style="font-size:11.5px">${esc(s.reason || "未填写原因")} · ${esc(s.by)}</div>
                    </div>
                    <div class="row">
                      <span class="badge muted">剩 ${Math.max(0, Math.round((s.until - Date.now() / 1000) / 60))} 分钟</span>
                      <button class="btn sm" onclick="delSilence('${s.id}')">取消</button>
                    </div>
                  </div>`
                )
                .join("")
            : `<div class="empty">没有生效中的静默</div>`
        }
        <div class="card-b" style="border-top:1px solid var(--line)">
          <div class="row" style="gap:8px;flex-wrap:wrap">
            <input type="text" id="sil-host" placeholder="主机（* 表示全部）" style="width:170px">
            <input type="text" id="sil-reason" placeholder="静默原因" style="width:170px">
            <button class="btn" onclick="addSilence()">新增静默 60 分钟</button>
          </div>
        </div>
      </div>
    </div>
  </div>

  <div class="card" style="margin-top:14px">
    <div class="card-h"><h2>已恢复</h2><span class="hint">最近 20 条</span></div>
    <div class="card-b tight">
      ${
        al.resolved.length
          ? `<table><thead><tr><th>规则</th><th>对象</th><th>开始</th><th>恢复</th><th>持续</th></tr></thead><tbody>
          ${al.resolved
            .map(
              (a) => `<tr>
              <td>${esc(a.ruleName)}</td><td class="mono">${esc(a.host)}</td>
              <td class="muted mono">${dateTime(a.startedAt)}</td>
              <td class="muted mono">${dateTime(a.updatedAt)}</td>
              <td class="mono muted">${Math.max(1, Math.round((a.updatedAt - a.startedAt) / 60))} 分钟</td>
            </tr>`
            )
            .join("")}
          </tbody></table>`
          : `<div class="empty">暂无</div>`
      }
    </div>
  </div>`;
}

async function ackAlert(id) {
  try {
    await api(`/api/v1/alerts/${id}/ack?by=oncall`, { method: "POST" });
    toast("已认领");
    render();
  } catch (e) {
    toast(e.message, true);
  }
}

async function addSilence() {
  const host = $("#sil-host").value.trim() || "*";
  const reason = $("#sil-reason").value.trim() || "计划内维护";
  try {
    await api("/api/v1/silences", { method: "POST", body: { host, rule: "*", reason, minutes: 60, by: "oncall" } });
    toast("已静默 60 分钟");
    render();
  } catch (e) {
    toast(e.message, true);
  }
}

async function delSilence(id) {
  await api(`/api/v1/silences/${id}`, { method: "DELETE" });
  toast("已取消静默");
  render();
}

// ---------- 容量 ----------

async function renderCapacity(view) {
  const d = await api("/api/v1/capacity");
  const vols = d.volumes || [];
  view.innerHTML = `
  <div class="grid kpis" style="grid-template-columns:repeat(3,minmax(0,1fr))">
    ${vols
      .map(
        (v) => `<div class="kpi">
          <div class="label">${esc(v.volume)} 剩余可写入</div>
          <div class="val" style="${v.status === "critical" ? "color:var(--critical)" : v.status === "warn" ? "color:var(--warn)" : ""}">${
          v.daysLeft >= 3650 ? ">10 年" : v.daysLeft + "<small>天</small>"
        }</div>
        <div class="foot">日增 ${bytes(v.growthPerDay)} · 拟合 R²=${nf(v.r2, 3)}</div>
      </div>`
      )
      .join("")}
    <div class="kpi">
      <div class="label">出口带宽 P95（6h）</div>
      <div class="val">${nf(d.bandwidth.p95, 1)}<small>Mbps</small></div>
      <div class="foot">峰值 ${nf(d.bandwidth.peak, 1)} · 链路 ${d.bandwidth.link} Mbps</div>
    </div>
  </div>

  ${vols
    .map(
      (v) => `<div class="card" style="margin-bottom:14px">
      <div class="card-h">
        <h2>${esc(v.volume)} · 90 天增长与 30 天预测</h2>
        <span class="hint">容量上限 ${bytes(v.capacity)} · 当前 ${bytes(v.current)} · 预计 ${esc(v.runwayDate)} 写满</span>
      </div>
      ${chart(
        [
          {
            name: "历史",
            color: "#0f766e",
            points: (v.history || []).map((p) => ({ ts: Date.parse(p.date + "T00:00:00") / 1000, value: p.bytes })),
          },
          {
            name: "预测",
            color: "#b45309",
            dash: true,
            area: false,
            points: [
              {
                ts: Date.parse((v.history[v.history.length - 1] || {}).date + "T00:00:00") / 1000,
                value: v.current,
              },
              ...(v.trend || []).map((p) => ({ ts: Date.parse(p.date + "T00:00:00") / 1000, value: p.bytes })),
            ].filter((p) => !Number.isNaN(p.ts)),
          },
        ],
        { height: 200, fmtY: (x) => bytes(x), fmtX: (ts) => {
            const d = new Date(ts * 1000);
            return `${d.getMonth() + 1}/${d.getDate()}`;
          } }
      )}
      <div class="chart-legend">
        <span><i style="background:#0f766e"></i>实际占用（90 天）</span>
        <span><i style="background:#b45309"></i>线性回归预测（30 天）</span>
        <span class="muted">R² 越接近 1 说明增长越线性，预测越可信；低于 0.8 时应改用业务口径估算</span>
      </div>
    </div>`
    )
    .join("")}

  <div class="card">
    <div class="card-h"><h2>带宽说明</h2></div>
    <div class="card-b">
      <p style="margin:0;color:var(--text-2);font-size:12.5px;line-height:1.8">
        ${esc(d.bandwidth.note)}<br>
        当前链路标称 <strong>${d.bandwidth.link} Mbps</strong>，P95 = <strong>${nf(d.bandwidth.p95, 2)} Mbps</strong>，
        水位 <strong>${nf((d.bandwidth.p95 / d.bandwidth.link) * 100, 1)}%</strong>。
        超过 70% 就应评估升级链路或开启 CDN 回源收敛，而不是等到丢包再处理。
      </p>
    </div>
  </div>`;
}

// ---------- 网关与 WAF ----------

async function renderGateway(view) {
  const [gw, rules, atk] = await Promise.all([
    api("/api/v1/gateway"),
    api("/api/v1/waf/rules"),
    api("/api/v1/waf/attacks"),
  ]);

  const catName = { sqli: "SQL 注入", xss: "XSS", rce: "命令注入", scanner: "扫描器", custom: "自定义" };

  view.innerHTML = `
  <div class="grid two-col" style="margin-bottom:14px">
    <div class="card">
      <div class="card-h">
        <h2>Nginx 配置（自动生成）</h2>
        <div class="row">
          <span class="badge ${gw.applied ? "ok" : "warning"}">${gw.applied ? "已与线上一致" : "有未应用变更"}</span>
          <button class="btn sm primary" id="btn-apply">应用并 reload</button>
        </div>
      </div>
      <pre class="code" style="border-radius:0 0 var(--radius) var(--radius)">${esc(gw.conf)}</pre>
    </div>

    <div class="card">
      <div class="card-h"><h2>WAF 命中统计（24h）</h2><span class="hint">拦截 ${atk.blocked} · 观察 ${atk.logged}</span></div>
      <div class="card-b tight">
        ${(atk.categories || [])
          .map(
            (c) => `<div class="list-row">
              <span class="badge accent">${catName[c.category] || c.category}</span>
              <span class="spacer"></span>
              <span class="num muted" style="font-size:11.5px">拦截 ${c.blocked}</span>
              <span class="strong num">${c.count}</span>
            </div>`
          )
          .join("") || `<div class="empty">暂无</div>`}
      </div>
      <div class="card-h" style="border-top:1px solid var(--line)">
        <h2>疑似误报</h2><span class="hint">内网出口命中"路径穿越"，规则处于观察模式</span>
      </div>
      <div class="card-b">
        <p style="margin:0;font-size:12px;color:var(--text-2);line-height:1.75">
          内网出口 IP <span class="mono">203.0.113.10</span> 因素材预览接口的 <span class="mono">../</span> 路径被规则
          <span class="mono">W1005</span> 命中。该规则当前 <strong>只记录不拦截</strong>，
          等确认业务侧改用规范路径后再逐步放开灰度。
        </p>
      </div>
    </div>
  </div>

  <div class="card" style="margin-bottom:14px">
    <div class="card-h"><h2>WAF 规则与灰度</h2><span class="hint">灰度按 hash(IP+规则ID) 稳定分桶，不会因重试穿透</span></div>
    <div class="card-b tight">
      <table>
        <thead><tr><th>规则</th><th>类型</th><th>目标</th><th>动作</th><th>灰度</th><th>命中</th><th>启用</th></tr></thead>
        <tbody>
        ${rules
          .map(
            (r) => `<tr>
              <td><div class="strong">${esc(r.name)}</div><div class="mono muted" style="font-size:10.5px">${esc(r.id)}</div></td>
              <td><span class="badge accent">${catName[r.category] || r.category}</span></td>
              <td class="mono muted">${esc(r.target)}</td>
              <td><span class="badge ${r.action === "block" ? "critical" : "muted"}">${r.action === "block" ? "拦截" : "观察"}</span></td>
              <td>
                <div class="row">
                  <input type="range" min="0" max="100" step="5" value="${r.rolloutPct}"
                    onchange="setRollout('${r.id}', this.value)">
                  <span class="mono num" style="font-size:11.5px;width:38px">${r.rolloutPct}%</span>
                </div>
              </td>
              <td class="mono num">${r.hits}</td>
              <td><span class="badge ${r.enabled ? "ok" : "muted"}">${r.enabled ? "启用" : "停用"}</span></td>
            </tr>`
          )
          .join("")}
        </tbody>
      </table>
    </div>
  </div>

  <div class="card">
    <div class="card-h"><h2>最近命中事件</h2><span class="hint">${atk.events.length} 条（最多显示 60）</span></div>
    <div class="card-b tight">
      <table>
        <thead><tr><th>时间</th><th>来源 IP</th><th>方法</th><th>路径</th><th>命中规则</th><th>动作</th></tr></thead>
        <tbody>
        ${atk.events
          .slice(0, 40)
          .map(
            (e) => `<tr>
              <td class="muted mono">${hhmm(e.ts)}</td>
              <td class="mono">${esc(e.ip)}</td>
              <td class="muted mono">${esc(e.method)}</td>
              <td class="mono" style="max-width:280px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap">${esc(e.path)}</td>
              <td>${esc(e.ruleName)}</td>
              <td><span class="badge ${e.action === "block" ? "critical" : "muted"}">${e.action === "block" ? "拦截" : "记录"}</span></td>
            </tr>`
          )
          .join("") || `<tr><td colspan="6"><div class="empty">暂无事件</div></td></tr>`}
        </tbody>
      </table>
    </div>
  </div>`;

  const btn = $("#btn-apply");
  if (btn) {
    btn.onclick = async () => {
      btn.disabled = true;
      btn.textContent = "应用中…";
      try {
        const r = await api("/api/v1/gateway/apply", { method: "POST" });
        toast(r.message + (r.backup ? `（备份：${r.backup}）` : ""));
      } catch (e) {
        toast(e.message, true);
      }
      render();
    };
  }
}

async function setRollout(id, val) {
  try {
    await api(`/api/v1/waf/rules/${id}`, { method: "PATCH", body: { rolloutPct: Number(val) } });
    toast(`规则 ${id} 灰度调整为 ${val}%`);
  } catch (e) {
    toast(e.message, true);
  }
}

// ---------- 文档 ----------

function renderDocs(view) {
  const docs = [
    ["README.md", "快速开始 · 部署 · 目录结构 · 设计取舍", "项目入口"],
    ["docs/architecture.md", "分层架构、数据流、组件职责边界", "架构说明"],
    ["docs/decisions/ADR-001-storage.md", "为什么用 JSON 文件而不是 PostgreSQL 起步", "决策记录"],
    ["docs/decisions/ADR-002-alert-rules.md", "告警规则为什么不用 PromQL", "决策记录"],
    ["docs/decisions/ADR-003-metrics-split.md", "自有指标链路与 VictoriaMetrics 的分工", "决策记录"],
    ["docs/decisions/ADR-004-waf.md", "为什么自研 WAF 检测而不是上 ModSecurity", "决策记录"],
    ["docs/sop/SOP-01-deploy.md", "标准发布流程（含回滚）", "SOP"],
    ["docs/sop/SOP-02-alert.md", "告警响应流程与升级路径", "SOP"],
    ["docs/sop/SOP-03-waf-change.md", "WAF 规则变更流程（灰度 → 观察 → 全量）", "SOP"],
    ["docs/postmortem/PM-001-disk.md", "素材盘写满导致转码任务堆积", "故障复盘"],
    ["docs/postmortem/PM-002-waf-lockout.md", "WAF 规则写错把运维后台也拦了", "故障复盘"],
  ];

  view.innerHTML = `
  <div class="card">
    <div class="card-h"><h2>文档索引</h2><span class="hint">全部随仓库版本管理，改动走 PR</span></div>
    <div class="card-b tight">
      <table>
        <thead><tr><th>文件</th><th>内容</th><th>类型</th></tr></thead>
        <tbody>
        ${docs
          .map(
            ([p, d, t]) => `<tr>
              <td class="mono strong">${esc(p)}</td>
              <td class="muted">${esc(d)}</td>
              <td><span class="badge ${t === "故障复盘" ? "warning" : t === "SOP" ? "accent" : t === "决策记录" ? "info" : "muted"}">${esc(t)}</span></td>
            </tr>`
          )
          .join("")}
        </tbody>
      </table>
    </div>
  </div>

  <div class="card" style="margin-top:14px">
    <div class="card-h"><h2>这个平台解决什么问题</h2></div>
    <div class="card-b">
      <p style="margin:0 0 10px;font-size:12.5px;line-height:1.9;color:var(--text-2)">
        一家 150 人的内容公司，基础设施规模大概是这样：几台云主机、一台 NAS 素材库、一条 5Mbps 出口、
        四五个对外站点（官网、课程、电商落地页、活动页）。这种规模最尴尬 —— 买商业方案不划算，
        纯手工又迟早出事。
      </p>
      <p style="margin:0;font-size:12.5px;line-height:1.9;color:var(--text-2)">
        StormOps 的做法是把重复动作固化下来：资产自动发现、配置生成而非手改、发布必须能回滚、
        告警必须带 SOP、WAF 规则必须能灰度。目标不是"做一个大平台"，而是
        <strong>让一个人能安全地管住这些机器</strong>。
      </p>
    </div>
  </div>`;
}

// ---------- 启动 ----------

function tick() {
  $("#clock").textContent = new Date().toLocaleString("zh-CN", {
    month: "2-digit",
    day: "2-digit",
    hour: "2-digit",
    minute: "2-digit",
    second: "2-digit",
  });
}

window.addEventListener("hashchange", route);
$("#btn-refresh").onclick = () => {
  render();
  toast("已刷新");
};

tick();
setInterval(tick, 1000);
route();
