// PoolMate 面板前端逻辑（无框架单页应用）
const $ = (s, el) => (el || document).querySelector(s);
const $$ = (s, el) => [...(el || document).querySelectorAll(s)];
let ADMIN_KEY = sessionStorage.getItem("pm_key") || "";
let TAB = location.hash.slice(1) || "overview";
let REFRESH_TIMER = null;

// ---------- 基础工具 ----------
async function api(path, opts = {}) {
  opts.headers = Object.assign({ "X-Admin-Key": ADMIN_KEY }, opts.headers);
  if (opts.body && typeof opts.body !== "string") {
    opts.headers["Content-Type"] = "application/json";
    opts.body = JSON.stringify(opts.body);
  }
  const r = await fetch(path, opts);
  if (r.status === 401) { logout(); throw new Error("密钥无效"); }
  const ct = r.headers.get("Content-Type") || "";
  const data = ct.includes("json") ? await r.json() : await r.text();
  if (!r.ok) throw new Error((data && data.error) || ("HTTP " + r.status));
  return data;
}
function toast(msg, ms = 2600) {
  const t = $("#toast"); t.textContent = msg; t.style.display = "block";
  clearTimeout(t._h); t._h = setTimeout(() => t.style.display = "none", ms);
}
function modal(html, wide) { const m = $("#modal"); m.style.width = wide ? "min(780px, 94vw)" : ""; m.innerHTML = html; $("#modal-mask").classList.add("show"); }
function closeModal() { $("#modal-mask").classList.remove("show"); }
$("#modal-mask").addEventListener("click", e => { if (e.target.id === "modal-mask") closeModal(); });
function fmtN(n) { return n == null ? "-" : Number(n).toLocaleString("zh-CN", { maximumFractionDigits: 2 }); }
function fmtTime(t) { if (!t || t.startsWith("0001")) return "-"; const d = new Date(t); return d.toLocaleString("zh-CN", { hour12: false, month: "2-digit", day: "2-digit", hour: "2-digit", minute: "2-digit" }); }
function fmtMs(ms) { return ms > 1000 ? (ms / 1000).toFixed(1) + "s" : ms + "ms"; }
function esc(s) { return String(s ?? "").replace(/[&<>"']/g, c => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c])); }
// js 生成可安全放进 on* 事件属性里的 JS 字符串字面量（先 JSON 转义，再 HTML 转义）。
function js(s) { return esc(JSON.stringify(String(s ?? ""))); }
function gateSubmit() { ADMIN_KEY = $("#key-input").value.trim(); sessionStorage.setItem("pm_key", ADMIN_KEY); boot(); }
function logout() { sessionStorage.removeItem("pm_key"); ADMIN_KEY = ""; location.reload(); }
function toggleTheme() {
  const h = document.documentElement;
  const v = h.dataset.theme === "light" ? "" : "light";
  h.dataset.theme = v; localStorage.setItem("pm_theme", v);
}

// ---------- 路由 ----------
const TABS = [
  ["overview", "概览"], ["accounts", "账号"], ["growth", "成长任务"], ["keys", "密钥"], ["models", "模型"],
  ["logs", "请求日志"], ["usage", "用量分析"], ["playground", "调试台"],
  ["tasks", "任务"], ["settings", "设置"],
];
function renderTabs() {
  $("#tabs").innerHTML = TABS.map(([id, name]) =>
    `<button class="${id === TAB ? "active" : ""}" onclick="go(${js(id)})">${name}</button>`).join("");
}
function go(tab) { TAB = tab; location.hash = tab; render(); }
window.addEventListener("hashchange", () => { TAB = location.hash.slice(1) || "overview"; render(); });
async function render() {
  renderTabs();
  clearInterval(REFRESH_TIMER);
  const page = $("#page");
  page.innerHTML = '<p style="color:var(--fg3)">加载中…</p>';
  try {
    await PAGES[TAB]();
  } catch (e) {
    page.innerHTML = `<div class="panel"><b>加载失败：</b>${esc(e.message)}</div>`;
  }
  if (TAB === "overview") REFRESH_TIMER = setInterval(render, 30000);
}

// ---------- 概览 ----------
async function pageOverview() {
  const d = await api("/api/overview");
  const succ = todaySucc(d.today);
  $("#page").innerHTML = `
  <div class="cards">
    <div class="card"><div class="l">可用账号</div><div class="v">${d.accounts_ready}<span style="font-size:14px;color:var(--fg3)"> / ${d.accounts_total}</span></div></div>
    <div class="card"><div class="l">总余额（积分）</div><div class="v">${fmtN(d.balance)}</div></div>
    <div class="card"><div class="l">7 天内到期</div><div class="v" style="color:var(--warn)">${fmtN(d.expiring_7d)}</div></div>
    <div class="card"><div class="l">预计浪费</div><div class="v" style="color:var(--bad)">${fmtN(d.waste_forecast)}</div></div>
    <div class="card"><div class="l">今日请求</div><div class="v">${d.today.requests}</div><div class="sub">成功率 ${succ}%</div></div>
    <div class="card"><div class="l">今日消耗（积分）</div><div class="v">${fmtN(d.today.credits)}</div></div>
    <div class="card"><div class="l">今日 Tokens</div><div class="v">${fmtK(d.today.tokens)}</div><div class="sub">缓存命中 ${fmtK(d.today.cache_hits)}</div></div>
  </div>
  ${(d.attention || []).length ? `<div class="panel"><h3>需要关注</h3>${d.attention.map(a => `<div class="attention ${a.level}">${esc(a.text)}</div>`).join("")}</div>` : ""}
  <div class="grid2">
    <div class="panel"><h3>近 24 小时请求</h3>${barChart(d.last24h || [])}</div>
    <div class="panel"><h3>最近请求</h3>${recentTable(d.recent || [])}</div>
  </div>`;
}
function todaySucc(t) { return t.requests ? Math.round((t.requests - t.errors) * 1000 / t.requests) / 10 : 100; }
function fmtK(n) { return n >= 10000 ? (n / 1000).toFixed(1) + "k" : fmtN(n); }
function barChart(hours) {
  const max = Math.max(1, ...hours.map(h => h.requests));
  return `<div class="bars">${hours.map((h, i) => {
    const hh = (new Date().getHours() - 23 + i + 48) % 24;
    return `<div class="b" style="height:${Math.max(2, h.requests * 100 / max)}%"><div class="tip">${hh}时：${h.requests} 次 / ${fmtN(h.credits)} 分</div></div>`;
  }).join("")}</div>`;
}
function recentTable(logs) {
  if (!logs.length) return '<p style="color:var(--fg3)">暂无请求</p>';
  return `<table><tr><th>时间</th><th>模型</th><th>账号</th><th>状态</th><th>积分</th><th>耗时</th></tr>${logs.map(l =>
    `<tr><td>${fmtTime(l.time)}</td><td>${esc(l.model)}</td><td>${esc(l.account_name || "-")}</td>
     <td><span class="tag ${l.status}">${l.status}</span></td><td>${fmtN(l.credits)}</td><td>${fmtMs(l.total_ms)}</td></tr>`).join("")}</table>`;
}

// ---------- 账号 ----------
let acctSel = new Set();
let acctTag = "", acctQ = "";
const PAY_TEXT = { free: "免费版", pro: "专业版", enterprise: "企业版", team: "团队版" };
function payText(p) { return p ? (PAY_TEXT[p] || p) : ""; }
async function pageAccounts() {
  const d = await api("/api/accounts");
  const all = d.accounts || [];
  const allTags = [...new Set(all.flatMap(a => a.Tags || []))].sort();
  if (acctTag && !allTags.includes(acctTag)) acctTag = "";
  const q = acctQ.trim().toLowerCase();
  const list = all.filter(a => (!acctTag || (a.Tags || []).includes(acctTag)) &&
    (!q || (a.Name || "").toLowerCase().includes(q) || a.UID.toLowerCase().includes(q)));
  const gstats = d.growth_stats || {};
  $("#page").innerHTML = `
  <div class="toolbar">
    <button class="primary" onclick="addAccountDlg()">＋ 添加账号</button>
    <button onclick="localImportDlg()">从本机客户端导入</button>
    <button onclick="importDlg()">导入凭证</button>
    <button onclick="exportDlg()">导出配置</button>
    <input id="acct-q" placeholder="搜索名称 / UID" value="${esc(acctQ)}" style="width:150px" oninput="acctQ=this.value;clearTimeout(window._aq);window._aq=setTimeout(render,300)">
    <select onchange="acctTag=this.value;render()" title="按标签筛选">
      <option value="">全部标签</option>${allTags.map(t => `<option value="${esc(t)}" ${t === acctTag ? "selected" : ""}>${esc(t)}</option>`).join("")}
    </select>
    <div class="spacer"></div>
    <span style="color:var(--fg3);font-size:12px" id="sel-info"></span>
    <button class="mini" onclick="batch('refresh',this)">刷新</button>
    <button class="mini" onclick="batch('refresh_token',this)" title="立即用 refresh token 续期所选账号的 access token">刷新 Token</button>
    <button class="mini" onclick="batch('checkin',this)">签到</button>
    <button class="mini" onclick="batch('claim_growth',this)">领奖</button>
    <button class="mini" onclick="batch('accept_growth',this)">接受任务</button>
    <button class="mini" onclick="batch('wake',this)" title="发一次最小请求，提前触发额度重置周期">唤醒</button>
    <button class="mini" onclick="tagBatchDlg('add_tag')">加标签</button>
    <button class="mini" onclick="tagBatchDlg('remove_tag')">去标签</button>
    <button class="mini" onclick="exportDlg(true)">导出所选</button>
    <button class="mini" onclick="batch('pause',this)">暂停</button>
    <button class="mini" onclick="batch('resume',this)">恢复</button>
    <button class="mini" onclick="batch('revive',this)">解冻</button>
    <button class="mini danger" onclick="batch('delete',this)">删除</button>
  </div>
  <div class="panel"><table>
    <tr><th><input type="checkbox" onchange="selAll(this.checked)"></th><th>状态</th><th>账号</th><th>域</th>
    <th>余额 / 已用</th><th>积分包明细（到期时间）</th><th>今日变化</th><th>Token 有效期</th><th>操作</th></tr>
    ${list.map(a => `<tr>
      <td><input type="checkbox" class="sel" data-uid="${esc(a.UID)}" onchange="selOne(${js(a.UID)},this.checked)" ${acctSel.has(a.UID) ? "checked" : ""}></td>
      <td><span class="tag ${a.Status}" title="${esc(a.StatusWhy || "")}">${statusText(a.Status)}</span></td>
      <td><b>${esc(a.Name || a.UID.slice(0, 8))}</b>
        ${a.PaymentType ? `<span class="tag ready" style="margin-left:6px" title="上游付费类型">${esc(payText(a.PaymentType))}</span>` : ""}
        ${a.EnterpriseID ? `<span class="tag cooling" style="margin-left:4px" title="企业 ID：${esc(a.EnterpriseID)}">企业</span>` : ""}
        ${a.DosageCode ? `<div style="font-size:11px;color:var(--warn)" title="上游用量提醒">⚠ ${esc(a.DosageMsg || "用量提醒 " + a.DosageCode)}</div>` : ""}
        ${(a.Tags || []).length ? `<div style="margin-top:2px">${a.Tags.map(t => `<span class="tag paused" style="margin-right:4px;cursor:pointer" onclick="acctTag=${js(t)};render()">${esc(t)}</span>`).join("")}</div>` : ""}
        ${(gstats[a.UID] || {}).today_credit ? `<div style="font-size:11px;color:var(--ok)" title="成长任务今日领取（自动 +${gstats[a.UID].auto_credit}）">今日领奖 +${gstats[a.UID].today_credit}</div>` : ""}</td>
      <td><span class="tag ${a.Realm}">${a.Realm === "global" ? "国际版" : "国内版"}</span></td>
      <td style="min-width:150px">${balanceCell(a)}</td>
      <td style="min-width:260px">${packsCell(a)}</td>
      <td style="min-width:130px;font-size:12px">${todayCell(a)}</td>
      ${tokenCell(a)}
      <td style="white-space:nowrap">
        <button class="mini primary" data-uid="${esc(a.UID)}" onclick="balanceDlg(this.dataset.uid)">明细</button>
        <button class="mini" onclick="acct(${js(a.UID)},'refresh')">刷新</button>
        <button class="mini" onclick="acct(${js(a.UID)},'checkin')">签到</button>
        <button class="mini" data-uid="${esc(a.UID)}" data-tags="${esc((a.Tags || []).join(","))}" onclick="tagDlg(this.dataset.uid,this.dataset.tags)">标签</button>
        <button class="mini" title="${a.WokeAt && !a.WokeAt.startsWith("0001") ? "上次唤醒 " + fmtTime(a.WokeAt) : "发最小请求触发额度重置周期"}" onclick="acct(${js(a.UID)},'wake')">唤醒</button>
        <button class="mini" data-uid="${esc(a.UID)}" data-name="${esc(a.Name)}" onclick="growthDlg(this.dataset.uid,this.dataset.name)">任务</button>
        <button class="mini" title="把这个账号写入本机 CodeBuddy / WorkBuddy 客户端" data-uid="${esc(a.UID)}" data-name="${esc(a.Name)}" data-realm="${esc(a.Realm)}" onclick="switchDlg(this.dataset.uid,this.dataset.name,this.dataset.realm)">切号</button>
        ${a.Status === "paused" ? `<button class="mini" onclick="acct(${js(a.UID)},'resume')">恢复</button>` : `<button class="mini" onclick="acct(${js(a.UID)},'pause')">暂停</button>`}
        <button class="mini" onclick="acct(${js(a.UID)},'revive')">解冻</button>
        <button class="mini danger" data-uid="${esc(a.UID)}" data-name="${esc(a.Name)}" onclick="delAcct(this.dataset.uid,this.dataset.name)">删</button>
      </td></tr>`).join("")}
  </table>${!list.length ? '<p style="color:var(--fg3);padding:20px;text-align:center">还没有账号，点击「添加账号」开始</p>' : ""}</div>`;
  updateSelInfo();
  if (acctQ) { const qi = $("#acct-q"); if (qi) { qi.focus(); qi.setSelectionRange(acctQ.length, acctQ.length); } }
}
// ---------- 积分展示：余额进度条 / 积分包明细 / 今日变化 / 变动明细 ----------
// Token 有效期：会被自动续期，所以只在续期失败或已逼近过期（自动续期没跟上）时标红/标橙。
function tokenLeft(d) {
  if (d <= 0) return "已过期";
  if (d < 1) return `${Math.max(1, Math.round(d * 24))} 小时`;
  return `${d.toFixed(0)} 天`;
}
function tokenCell(a) {
  const d = a.ExpiryDays, err = a.TokenRefreshErr || "";
  const last = a.TokenRefreshedAt && !a.TokenRefreshedAt.startsWith("0001") ? a.TokenRefreshedAt : "";
  const color = err || d < 0.5 ? "var(--bad)" : d < 1 ? "var(--warn)" : "inherit";
  const tip = [
    `access token ${fmtTime(a.TokenExpiresAt)} 到期`,
    last ? `最近续期成功：${fmtTime(last)}` : "本次启动后尚未续期",
    err ? `最近一次续期失败：${err}` : "",
    "剩余不足 1 天会自动用 refresh token 续期，每天 22 点保活也会续期；点击可立即续期",
  ].filter(Boolean).join("\n");
  const sub = err ? '<div style="font-size:11px">续期失败</div>'
    : last ? `<div style="font-size:11px;color:var(--fg3)">${fmtTime(last)} 已续期</div>` : "";
  return `<td style="color:${color};cursor:pointer;white-space:nowrap" title="${esc(tip)}" data-uid="${esc(a.UID)}" onclick="refreshToken(this.dataset.uid,this)">${tokenLeft(d)}${sub}</td>`;
}
async function refreshToken(uid, el) {
  if (el.dataset.busy) return;
  el.dataset.busy = "1"; el.style.opacity = ".5";
  try {
    const r = await api(`/api/accounts/${uid}/action`, { method: "POST", body: { action: "refresh_token" } });
    toast(`已续期，有效期至 ${fmtTime(r.expires_at)}${r.revived ? "，账号已恢复启用" : ""}`);
  } catch (e) { toast("续期失败：" + e.message); }
  render();
}
function leftText(t) {
  if (!t || String(t).startsWith("0001")) return "";
  const ms = new Date(t).getTime() - Date.now();
  if (ms <= 0) return "已到期";
  const m = Math.floor(ms / 60000), d = Math.floor(m / 1440), h = Math.floor((m % 1440) / 60);
  if (d >= 1) return `${d} 天 ${h} 小时`;
  if (h >= 1) return `${h} 小时 ${m % 60} 分`;
  return `${m} 分钟`;
}
function expColor(t) {
  const ms = new Date(t).getTime() - Date.now();
  return ms < 24 * 3600e3 ? "var(--bad)" : ms < 7 * 864e5 ? "var(--warn)" : "var(--fg2)";
}
function bar(pct, color) {
  pct = Math.max(0, Math.min(100, pct));
  return `<div style="height:6px;background:var(--bg3);border-radius:3px;overflow:hidden"><div style="height:100%;width:${pct}%;background:${color}"></div></div>`;
}
function packTotals(a) {
  const ps = a.Packages || [];
  return { size: ps.reduce((s, p) => s + (p.size || 0), 0), used: ps.reduce((s, p) => s + (p.used || 0), 0) };
}
function balanceCell(a) {
  const { size, used } = packTotals(a);
  const remainPct = size > 0 ? a.Balance * 100 / size : 0;
  const color = remainPct < 10 ? "var(--bad)" : remainPct < 30 ? "var(--warn)" : "var(--ok)";
  return `<div style="font-size:16px;font-weight:600">${fmtN(a.Balance)}</div>
    ${size > 0 ? `${bar(remainPct, color)}<div style="font-size:11px;color:var(--fg3);margin-top:2px">已用 ${fmtN(used)} / 共 ${fmtN(size)}（剩 ${remainPct.toFixed(0)}%）</div>` : '<div style="font-size:11px;color:var(--fg3)">无积分包数据</div>'}
    ${a.Expiring7d > 0 ? `<div style="font-size:11px;color:var(--warn)">7 天内到期 ${fmtN(a.Expiring7d)}</div>` : ""}
    ${a.BalanceErr ? `<div style="font-size:11px;color:var(--bad)" title="${esc(a.BalanceErr)}">余额刷新失败</div>` : ""}`;
}
function shortName(n) { return (n || "积分包").replace(/^CodeBuddy/, "").replace(/^个人版[-_]?/, "") || "积分包"; }
function dayKey(t) { const d = new Date(t); return d.getFullYear() + "-" + String(d.getMonth() + 1).padStart(2, "0") + "-" + String(d.getDate()).padStart(2, "0"); }
// 同名且同一天到期的积分包合并（赠送包常常几十个 100 分的小包）
function packGroups(a) {
  const m = new Map();
  for (const p of (a.Packages || [])) {
    const k = (p.name || "") + "|" + (p.has_cycle ? dayKey(p.cycle_end) : "none");
    let g = m.get(k);
    if (!g) { g = { name: p.name, has_cycle: p.has_cycle, end: p.cycle_end, remain: 0, used: 0, size: 0, n: 0 }; m.set(k, g); }
    g.remain += p.remain || 0; g.used += p.used || 0; g.size += p.size || 0; g.n++;
    if (p.has_cycle && new Date(p.cycle_end) < new Date(g.end)) g.end = p.cycle_end;
  }
  return [...m.values()].sort((x, y) => (x.has_cycle ? new Date(x.end) : 9e15) - (y.has_cycle ? new Date(y.end) : 9e15));
}
const PACK_ROWS = 4;
function packsCell(a) {
  const gs = packGroups(a);
  if (!gs.length) return '<span style="color:var(--fg3);font-size:12px">-</span>';
  const row = g => {
    const pct = g.size > 0 ? g.remain * 100 / g.size : 0;
    const exp = g.has_cycle ? `<span style="color:${expColor(g.end)}">${fmtTime(g.end)} 到期 · 还剩 ${leftText(g.end)}</span>` : '<span style="color:var(--fg3)">无到期时间</span>';
    return `<div style="margin-bottom:6px;font-size:12px">
      <div style="display:flex;justify-content:space-between;gap:8px"><b>${esc(shortName(g.name))}${g.n > 1 ? ` <span style="color:var(--fg3);font-weight:normal">×${g.n}</span>` : ""}</b><span>剩 ${fmtN(g.remain)} <span style="color:var(--fg3)">/ ${fmtN(g.size)} · 已用 ${fmtN(g.used)}</span></span></div>
      ${bar(pct, "var(--info)")}<div style="margin-top:1px">${exp}</div></div>`;
  };
  const shown = gs.slice(0, PACK_ROWS).map(row).join("");
  const rest = gs.slice(PACK_ROWS);
  if (!rest.length) return shown;
  const restRemain = rest.reduce((s, g) => s + g.remain, 0);
  const last = rest.filter(g => g.has_cycle).pop();
  return shown + `<div style="font-size:12px;color:var(--fg3)">另有 ${rest.length} 批，合计剩 ${fmtN(restRemain)}${last ? `，最晚 ${fmtTime(last.end)} 到期` : ""} · 点「明细」查看</div>`;
}
const ADD_KINDS = { checkin: "签到", claim: "任务领奖", add: "领奖/充值", reset: "周期重置" };
// 今日增加的来源拆解：来源 + 金额，以及是否新增了积分包（计入总额度）
function addedReasons(a) {
  const today = dayKey(new Date());
  const list = (a.Changes || []).filter(c => c.delta > 0 && ADD_KINDS[c.kind] && dayKey(c.time) === today);
  if (!list.length) return a.TodayAdded > 0 ? '<div style="font-size:11px;color:var(--fg3)">来源未记录（本版本之前的增加）</div>' : "";
  return list.map(c => {
    const inTotal = c.size_delta > 0 ? `计入总额度 +${fmtN(c.size_delta)}` : "总额度不变";
    return `<div style="font-size:11px;color:var(--fg3)" title="${esc(c.note || "")}">↳ ${ADD_KINDS[c.kind]} +${fmtN(c.delta)} · ${inTotal}</div>`;
  }).join("");
}
function todayCell(a) {
  const used = (a.DailyCredit || 0) + (a.TodayExternal || 0);
  const rows = [];
  rows.push(`<div title="经 PoolMate 网关 ${fmtN(a.DailyCredit)}，其他渠道（如客户端直连）${fmtN(a.TodayExternal)}">消耗 <b>${fmtN(used)}</b></div>`);
  rows.push(`<div style="color:${a.TodayAdded > 0 ? "var(--ok)" : "var(--fg3)"}" title="签到、领奖、充值、周期重置等带来的增加">增加 <b>${a.TodayAdded > 0 ? "+" : ""}${fmtN(a.TodayAdded)}</b></div>`);
  rows.push(addedReasons(a));
  if (a.TodayExpired > 0) rows.push(`<div style="color:var(--bad)" title="积分包到期，剩余额度作废">到期作废 <b>-${fmtN(a.TodayExpired)}</b></div>`);
  if (a.TodayPending > 0) rows.push(`<div style="font-size:11px;color:var(--fg3)" title="网关已记账并在本地预扣、但上游余额还没扣下来的积分。上游扣费通常有几分钟延迟，补扣后这里会自动归零，不计入增加或其他渠道消耗。">待上游扣费 ${fmtN(a.TodayPending)}</div>`);
  return rows.join("");
}
const CHG_KIND = { checkin: ["签到", "var(--ok)"], claim: ["任务领奖", "var(--ok)"], add: ["增加", "var(--ok)"], reset: ["周期重置", "var(--ok)"], expire: ["到期作废", "var(--bad)"], spend: ["其他渠道消耗", "var(--warn)"] };
function balanceDlg(uid) {
  api("/api/accounts").then(d => {
    const a = (d.accounts || []).find(x => x.UID === uid);
    if (!a) return toast("账号不存在");
    const { size, used } = packTotals(a);
    const ps = packGroups(a);
    const chg = (a.Changes || []).slice().reverse();
    modal(`<h3>积分明细 · ${esc(a.Name || uid.slice(0, 8))}</h3>
    <div style="display:flex;gap:18px;flex-wrap:wrap;margin:6px 0 12px">
      <div><div style="color:var(--fg3);font-size:12px">当前余额</div><div style="font-size:20px;font-weight:600">${fmtN(a.Balance)}</div></div>
      <div><div style="color:var(--fg3);font-size:12px">总额度 / 已用</div><div style="font-size:16px">${fmtN(size)} / ${fmtN(used)}</div></div>
      <div><div style="color:var(--fg3);font-size:12px">今日消耗</div><div style="font-size:16px">${fmtN((a.DailyCredit || 0) + (a.TodayExternal || 0))}</div></div>
      <div><div style="color:var(--fg3);font-size:12px">今日增加</div><div style="font-size:16px;color:var(--ok)">+${fmtN(a.TodayAdded)}</div></div>
      <div><div style="color:var(--fg3);font-size:12px">今日到期作废</div><div style="font-size:16px;color:${a.TodayExpired ? "var(--bad)" : "inherit"}">${fmtN(a.TodayExpired)}</div></div>
      <div><div style="color:var(--fg3);font-size:12px">余额更新于</div><div style="font-size:14px">${fmtTime(a.BalanceAt)}</div></div>
    </div>
    <h4 style="margin:8px 0 4px">积分包（同名且同日到期已合并，按到期时间排序，共 ${(a.Packages || []).length} 个 / ${ps.length} 批）</h4>
    <div style="max-height:260px;overflow:auto">${ps.length ? `<table><tr><th>名称</th><th>个数</th><th>剩余</th><th>已用</th><th>总量</th><th>到期时间</th><th>距到期</th></tr>${ps.map(g => `<tr>
      <td>${esc(shortName(g.name))}</td><td>${g.n}</td><td>${fmtN(g.remain)}</td><td>${fmtN(g.used)}</td><td>${fmtN(g.size)}</td>
      <td>${g.has_cycle ? fmtTime(g.end) : "-"}</td><td style="color:${g.has_cycle ? expColor(g.end) : "inherit"}">${g.has_cycle ? leftText(g.end) : "-"}</td></tr>`).join("")}</table>` : '<p style="color:var(--fg3)">暂无积分包数据</p>'}</div>
    <h4 style="margin:14px 0 4px">余额变动记录（最近 ${chg.length} 条）</h4>
    <div style="max-height:240px;overflow:auto">${chg.length ? `<table><tr><th>时间</th><th>类型</th><th>变动</th><th>变动后余额</th><th>总额度</th><th>说明</th></tr>${chg.map(c => {
      const k = CHG_KIND[c.kind] || [c.kind, "inherit"];
      return `<tr><td>${fmtTime(c.time)}</td><td style="color:${k[1]}">${k[0]}</td><td style="color:${k[1]}">${c.delta > 0 ? "+" : ""}${fmtN(c.delta)}</td><td>${fmtN(c.balance)}</td><td>${c.delta > 0 ? (c.size_delta > 0 ? `<span style="color:var(--ok)">+${fmtN(c.size_delta)}</span>` : "不变") : "-"}</td><td style="font-size:12px;color:var(--fg3)">${esc(c.note || "")}</td></tr>`;
    }).join("")}</table>` : '<p style="color:var(--fg3)">暂无变动记录（从本版本起，每次刷新余额时对比差异自动记录）</p>'}</div>
    <p style="color:var(--fg3);font-size:11px;margin-top:8px">变动按相邻两次余额刷新的差值推算，已扣除经 PoolMate 网关的消耗；因此客户端直连等外部消耗会单独列出。刷新间隔内的多次变动会合并为一条。</p>
    <div class="mrow"><button onclick="acct(${js(uid)},'refresh');closeModal()">刷新余额</button><button class="primary" onclick="closeModal()">关闭</button></div>`, true);
  }).catch(e => toast(e.message));
}
function tagDlg(uid, tags) {
  modal(`<h3>编辑标签</h3>
  <p style="color:var(--fg2);font-size:12px">用逗号分隔，每个标签最多 20 字，最多 10 个。</p>
  <input id="tag-in" value="${esc(tags)}" placeholder="例如：主力, 备用, 企业">
  <div class="mrow"><button onclick="closeModal()">取消</button><button class="primary" onclick="tagSave(${js(uid)})">保存</button></div>`);
}
async function tagSave(uid) {
  const tags = $("#tag-in").value.split(/[,，]/).map(s => s.trim()).filter(Boolean);
  try { await api(`/api/accounts/${uid}/action`, { method: "POST", body: { action: "set_tags", tags } }); closeModal(); toast("标签已保存"); render(); }
  catch (e) { toast(e.message); }
}
function tagBatchDlg(action) {
  if (!acctSel.size) return toast("先勾选账号");
  modal(`<h3>${action === "add_tag" ? "批量加标签" : "批量去标签"}（${acctSel.size} 个账号）</h3>
  <input id="tag-in" placeholder="标签名">
  <div class="mrow"><button onclick="closeModal()">取消</button><button class="primary" onclick="tagBatchDo(${js(action)})">确定</button></div>`);
}
async function tagBatchDo(action) {
  const tag = $("#tag-in").value.trim();
  if (!tag) return toast("请输入标签");
  try { await api("/api/accounts/batch", { method: "POST", body: { action, uids: [...acctSel], tag } }); closeModal(); toast("已完成"); render(); }
  catch (e) { toast(e.message); }
}
// ---------- 一键切号：把池内账号写入本机客户端 ----------
async function switchDlg(uid, name, realm) {
  modal(`<h3>切换本机客户端账号</h3><p style="color:var(--fg2)">正在读取本机客户端状态…</p>`);
  try {
    const d = await api("/api/local/switch/targets");
    const all = d.targets || [];
    const list = all.filter(t => t.realm === realm);
    const rows = list.map((t, i) => `<tr>
      <td><input type="radio" name="sw-src" value="${esc(t.name)}" ${i === 0 ? "checked" : ""} ${t.error ? "disabled" : ""}></td>
      <td>${esc(t.name)}</td>
      <td>${t.error ? `<span style="color:var(--bad)">${esc(t.error)}</span>` : t.logged_in ? esc(t.nickname || t.uid) + (t.uid === uid ? "（已是该账号）" : t.in_pool ? "" : " ⚠ 不在账号池") : "未登录"}</td>
      <td>${t.running ? '<span class="tag cooling">运行中</span>' : '<span class="tag paused">未运行</span>'}</td>
      <td>${t.has_backup ? `<button class="mini" onclick="switchRestore(${js(t.name)},${t.running})">还原上次</button>` : ""}</td></tr>`).join("");
    modal(`<h3>切换本机客户端账号</h3>
    <p style="font-size:13px">把「<b>${esc(name || uid.slice(0, 8))}</b>」写入本机客户端的登录态。</p>
    ${list.length ? `<table><tr><th></th><th>客户端</th><th>当前账号</th><th>状态</th><th></th></tr>${rows}</table>`
      : `<p style="color:var(--fg3)">未发现与该账号区域（${realm === "global" ? "国际版" : "国内版"}）匹配的本机客户端，或客户端从未登录过。</p>`}
    <ul style="color:var(--warn);font-size:12px;margin:10px 0;padding-left:18px">
      <li>客户端运行中会被关闭（先正常退出，超时再强制结束），请先保存客户端里未完成的工作。</li>
      <li>写入前自动备份原登录态，可随时点「还原上次」。</li>
      <li>客户端与 PoolMate 之后共用同一份 refreshToken，任一方刷新会让另一方失效；若客户端提示登录过期，再从这里切一次或在客户端重新登录。</li>
    </ul>
    <div class="mrow"><button onclick="closeModal()">取消</button>${list.length ? `<button class="primary" onclick="switchDo(${js(uid)},this)">确认切换</button>` : ""}</div>`, true);
  } catch (e) { modal(`<h3>切换本机客户端账号</h3><p style="color:var(--bad)">${esc(e.message)}</p><div class="mrow"><button onclick="closeModal()">关闭</button></div>`); }
}
async function switchDo(uid, btn) {
  const sel = document.querySelector('input[name="sw-src"]:checked');
  if (!sel) return toast("请选择客户端");
  await withBusy(btn, async () => {
    try {
      const r = await api(`/api/accounts/${uid}/switch`, { method: "POST", body: { source: sel.value, close_client: true } });
      closeModal();
      toast(r.unchanged ? r.message : `已切换：${r.from || "未登录"} → ${r.to}。${r.message}`, 6000);
      render();
    } catch (e) { toast("切换失败：" + e.message, 6000); }
  });
}
async function switchRestore(source, running) {
  if (running && !confirm("客户端正在运行，还原会先关闭它。继续？")) return;
  try {
    const r = await api("/api/local/switch/restore", { method: "POST", body: { source, close_client: true } });
    closeModal();
    toast(`已还原为 ${r.nickname || r.uid || "原账号"}。${r.message}`, 6000);
  } catch (e) { toast("还原失败：" + e.message, 6000); }
}
async function localImportDlg() {
  modal(`<h3>从本机客户端导入</h3><p style="color:var(--fg2)">正在探测本机 WorkBuddy / CodeBuddy 客户端…</p>`);
  try {
    const d = await api("/api/local/sources");
    const items = d.accounts || [];
    modal(`<h3>从本机客户端导入</h3>
    <p style="color:var(--fg2);font-size:12px">读取本机已登录客户端的登录态（只读，不改动客户端）。检测到的客户端：${(d.sources || []).map(s => esc(s.name)).join("、") || "无"}</p>
    ${items.length ? `<table><tr><th>来源</th><th>账号</th><th>类型</th><th>凭证到期</th><th>状态</th></tr>
      ${items.map(i => `<tr><td>${esc(i.source)}</td><td>${esc(i.nickname || i.uid)}</td><td>${i.enterprise_id ? "企业" : "个人"}</td>
      <td>${i.expires_at ? fmtTime(new Date(i.expires_at * 1000).toISOString()) : "-"}</td><td>${i.in_pool ? "已在池中" : "未导入"}</td></tr>`).join("")}</table>`
      : '<p style="color:var(--fg3)">未发现可导入的登录态。</p>'}
    ${(d.errors || []).length ? `<p style="color:var(--warn);font-size:12px">${d.errors.map(esc).join("<br>")}</p>` : ""}
    <p style="color:var(--warn);font-size:12px;margin-top:8px">⚠ 导入后本机客户端与 PoolMate 共用同一份刷新令牌，任一方刷新都可能让另一方登录失效（需重新登录）。企业账号请确认符合公司使用规范。</p>
    <div class="mrow"><button onclick="closeModal()">取消</button>${items.length ? '<button class="primary" onclick="localImportDo(this)">导入全部</button>' : ""}</div>`, true);
  } catch (e) { modal(`<h3>从本机客户端导入</h3><p style="color:var(--bad)">${esc(e.message)}</p><div class="mrow"><button onclick="closeModal()">关闭</button></div>`); }
}
async function localImportDo(btn) {
  await withBusy(btn, async () => {
    try {
      const r = await api("/api/local/import", { method: "POST", body: {} });
      closeModal();
      toast(`导入 ${r.imported.length} 个，跳过 ${r.skipped.length} 个`);
      if (r.skipped.length) setTimeout(() => alert("跳过明细：\n" + r.skipped.join("\n")), 300);
      render();
    } catch (e) { toast("导入失败：" + e.message); }
  });
}
function statusText(s) { return { ready: "可用", cooling: "冷却", breaker: "熔断", disabled: "禁用", paused: "暂停", budget: "预算满" }[s] || s; }
function selAll(on) { $$(".sel").forEach(c => { c.checked = on; }); acctSel = on ? new Set($$(".sel").map(c => c.dataset.uid)) : new Set(); updateSelInfo(); }
function selOne(uid, on) { on ? acctSel.add(uid) : acctSel.delete(uid); updateSelInfo(); }
function updateSelInfo() { const el = $("#sel-info"); if (el) el.textContent = acctSel.size ? `已选 ${acctSel.size} 个` : ""; }
async function acct(uid, action) {
  try { await api(`/api/accounts/${uid}/action`, { method: "POST", body: { action } }); toast("已执行"); render(); }
  catch (e) { toast("失败：" + e.message); }
}
function delAcct(uid, name) {
  modal(`<h3>删除账号</h3><p>确认删除「${esc(name)}」？凭证文件将从磁盘移除，不可恢复。</p>
  <div class="mrow"><button onclick="closeModal()">取消</button><button class="danger" onclick="doDel(${js(uid)})">确认删除</button></div>`);
}
async function doDel(uid) { try { await api(`/api/accounts/${uid}/action`, { method: "POST", body: { action: "delete" } }); closeModal(); toast("已删除"); render(); } catch (e) { toast(e.message); } }

// 按钮忙碌态：执行期间禁用并显示“处理中…”，防止连点重复请求。
async function withBusy(btn, fn) {
  if (!btn || btn.disabled) return;
  const old = btn.textContent;
  btn.disabled = true; btn.textContent = "处理中…";
  try { return await fn(); } finally { btn.disabled = false; btn.textContent = old; }
}
async function batch(action, btn, uidList) {
  const uids = uidList || [...acctSel];
  if (!uids.length) return toast("先勾选账号");
  await withBusy(btn, async () => {
    try {
      const r = await api("/api/accounts/batch", { method: "POST", body: { action, uids } });
      if (action === "claim_growth" || action === "accept_growth" || (r.errors && r.errors.length)) batchResultDlg(action, r);
      else toast(`成功 ${r.ok} 个，失败 ${r.failed} 个`);
      if (TAB === "growth") pageGrowth(); else render();
    } catch (e) { toast(e.message); }
  });
}
// 批量结果弹窗：逐账号明细 + 失败原因（替代 alert）。
function batchResultDlg(action, r) {
  const title = { claim_growth: "批量领奖结果", accept_growth: "批量接受结果" }[action] || "批量操作结果";
  const det = r.details || [];
  const credit = det.reduce((s, x) => s + (x.credit || 0), 0);
  const rows = det.map(x => `<tr><td>${esc(x.name)}</td>
    <td>${action === "claim_growth" ? `领 ${x.done} 项${x.credit ? ` · +${x.credit} 积分` : ""}${x.failed ? ` <span style="color:var(--bad)">失败 ${x.failed}</span>` : ""}` : `接受 ${x.done} 项${x.skipped ? `，跳过 ${x.skipped}` : ""}`}</td></tr>`).join("");
  modal(`<h3>${title}</h3>
  <p>成功 ${r.ok} 个账号，<span style="color:${r.failed ? "var(--bad)" : "inherit"}">失败 ${r.failed} 个</span>${credit ? `；合计 +${credit} 积分` : ""}</p>
  ${rows ? `<table>${rows}</table>` : ""}
  ${r.errors && r.errors.length ? `<h4 style="margin:12px 0 6px;color:var(--bad)">失败明细</h4><div class="log-detail">${r.errors.map(esc).join("<br>")}</div>` : ""}
  <div class="mrow"><button class="primary" onclick="closeModal()">关闭</button></div>`, true);
}

// ---------- 成长任务（单账号弹窗） ----------
const G = { uid: "", name: "", data: null, busy: false, collapsed: { done: true, locked: true } };
const G_GROUPS = [
  ["claimable", "待领奖", "ok"], ["acceptable", "未接受", "info"], ["doing", "进行中", "warn"],
  ["locked", "未解锁", ""], ["done", "已完成", ""],
];
const G_STATUS_TEXT = { accepted: "已接受", already_accepted: "已接受过" };
function growthState(t) {
  if (t.accept_status === "claimed") return "done";
  if (t.locked) return "locked";
  if (t.target > 0 && t.current >= t.target) return "claimable";
  if (t.accept_status === "accepted") return "doing";
  return "acceptable"; // 未接受
}
// 限时任务：从描述文本解析“至 M 月 D 日 / 截止 M 月 D 日”，返回截止 Date 或 null。
function growthDeadline(t) {
  const m = /(?:至|截止(?:到)?)\s*(\d{1,2})\s*月\s*(\d{1,2})\s*日/.exec(t.task_desc || "");
  if (!m) return null;
  const now = new Date();
  let d = new Date(now.getFullYear(), +m[1] - 1, +m[2], 23, 59, 59);
  if (now - d > 180 * 86400000) d = new Date(now.getFullYear() + 1, +m[1] - 1, +m[2], 23, 59, 59);
  return d;
}
function growthDays(d) { return d ? Math.ceil((d - Date.now()) / 86400000) : null; }
// 仅放行 http(s)/workbuddy 协议，杜绝 javascript: 等注入。
function safeUrl(u) {
  try { const x = new URL(u, location.href); return ["http:", "https:", "workbuddy:"].includes(x.protocol) ? x.href : ""; }
  catch { return ""; }
}
function growthSort(list) {
  return list.slice().sort((a, b) => {
    const da = growthDays(growthDeadline(a)), db = growthDays(growthDeadline(b));
    const ha = da != null && da >= 0, hb = db != null && db >= 0;
    if (ha !== hb) return ha ? -1 : 1;
    if (ha && hb && da !== db) return da - db;
    return (b.reward_credit || 0) - (a.reward_credit || 0);
  });
}
function growthRow(t) {
  const st = growthState(t);
  const reward = `+${t.reward_credit} 积分${t.reward_energy ? ` · +${t.reward_energy} 能量` : ""}`;
  let prog = "";
  if (t.target > 0) {
    const pct = Math.min(100, Math.round(t.current / t.target * 100));
    prog = `<div class="pbar" title="${t.current}/${t.target}"><i style="width:${pct}%"></i></div>
      <span style="color:var(--fg3);font-size:11px">${t.current}/${t.target}</span>${st === "doing" ? ` <span style="color:var(--warn);font-size:11px">还差 ${t.target - t.current}</span>` : ""}`;
  } else if (st === "doing") {
    prog = `<div style="color:var(--fg3);font-size:11px">待客户端完成</div>`;
  }
  const dl = growthDeadline(t), left = growthDays(dl);
  const dlTag = dl && left >= 0 && st !== "done"
    ? ` <span class="tag ${left <= 3 ? "error" : "warn"}">${left === 0 ? "今日截止" : `剩 ${left} 天`}</span>` : "";
  const tagCls = /限定|限时/.test(t.tag || "") ? "warn" : "info";
  const url = t.jump_url ? safeUrl(t.jump_url) : "";
  const act = st === "claimable" ? `<button class="mini primary" data-g="claim" data-code="${esc(t.task_code)}">领奖</button>`
    : st === "acceptable" ? `<button class="mini" data-g="accept" data-code="${esc(t.task_code)}">接受</button>` : "";
  const jump = url && st !== "done" && st !== "locked"
    ? ` <a class="btn mini" href="${esc(url)}" ${url.startsWith("workbuddy:") ? 'title="用 WorkBuddy 桌面端打开"' : 'target="_blank" rel="noopener noreferrer"'}>去完成</a>` : "";
  return `<tr><td>${esc(t.title)}${t.tag ? ` <span class="tag ${tagCls}">${esc(t.tag)}</span>` : ""}${t.is_new ? ` <span class="tag ok">NEW</span>` : ""}${dlTag}
      <div style="color:var(--fg3);font-size:12px">${esc(t.task_desc || "")}</div></td>
    <td style="white-space:nowrap;min-width:130px">${reward}${prog}</td><td style="white-space:nowrap">${act}${jump}</td></tr>`;
}
function growthPaint() {
  const d = G.data, tasks = d.tasks || [];
  const prev = $("#g-scroll") ? $("#g-scroll").scrollTop : 0;
  const sections = G_GROUPS.map(([key, label, cls]) => {
    const list = growthSort(tasks.filter(t => growthState(t) === key));
    if (!list.length) return "";
    const fold = key === "done" || key === "locked";
    const closed = fold && G.collapsed[key];
    const sum = key === "claimable" ? ` <span class="tag ok">+${list.reduce((s, t) => s + t.reward_credit, 0)} 积分</span>` : "";
    return `<h4 class="gh" ${fold ? `data-g="toggle" data-key="${key}"` : ""} style="margin:14px 0 6px">${fold ? (closed ? "▸ " : "▾ ") : ""}${label}（${list.length}）${sum}</h4>
    ${closed ? "" : `<table>${list.map(growthRow).join("")}</table>`}`;
  }).join("");
  const st = d.stat || {};
  const statLine = st.today_credit ? `<span style="color:var(--ok);font-size:12px">今日已领 +${st.today_credit} 积分${st.auto_credit ? `（自动 +${st.auto_credit}）` : ""}</span>` : "";
  modal(`<h3>成长任务 · ${esc(G.name)} ${statLine}</h3>
  <div class="mrow" style="justify-content:flex-start;margin-top:0">
    <button class="mini primary" data-g="claim_all" ${d.claimable ? "" : "disabled"}>一键领奖${d.claimable ? `（${d.claimable} 项 +${d.claimable_credit}）` : ""}</button>
    <button class="mini" data-g="accept_all" ${d.acceptable ? "" : "disabled"}>全部接受${d.acceptable ? `（${d.acceptable}）` : ""}</button>
    <button class="mini" data-g="refresh">刷新</button>
    <div class="spacer"></div>
    <button class="mini" data-g="close">关闭</button>
  </div>
  <div id="g-scroll" style="max-height:56vh;overflow:auto">${sections || '<p style="color:var(--fg3)">暂无任务</p>'}</div>
  <p style="color:var(--fg3);font-size:12px;margin-top:10px">「进行中」的任务需在 WorkBuddy 客户端完成对应行为后回到此处领奖（后台每隔设定时间也会自动领取）。</p>`, true);
  if (prev) $("#g-scroll").scrollTop = prev; // 重绘后保持滚动位置
}
// 打开 / 刷新弹窗。silent=true 且弹窗已开时不闪“加载中”；refresh=true 绕过 45s 缓存。
async function growthDlg(uid, name, opt = {}) {
  const silent = opt.silent && G.data && G.uid === uid && $("#g-scroll");
  G.uid = uid; G.name = name;
  if (!silent) { G.data = null; modal(`<h3>成长任务 · ${esc(name)}</h3><p style="color:var(--fg3)">加载中…</p>`, true); }
  let d;
  try { d = await api(`/api/accounts/${uid}/growth${opt.refresh ? "?refresh=1" : ""}`); }
  catch (e) {
    if (G.uid !== uid) return;
    modal(`<h3>成长任务 · ${esc(name)}</h3><p style="color:var(--bad)">${esc(e.message)}</p><div class="mrow"><button data-g="refresh">重试</button><button data-g="close">关闭</button></div>`, true);
    return;
  }
  if (G.uid !== uid || !$("#modal-mask").classList.contains("show")) return; // 加载期间已关闭/切换
  G.data = d; growthPaint();
}
// 弹窗内事件委托（data-g 属性），避免把账号名/任务码拼进 onclick。
$("#modal").addEventListener("click", e => {
  const el = e.target.closest("[data-g]");
  if (el && !el.disabled) growthClick(el);
});
async function growthClick(el) {
  const act = el.dataset.g, uid = G.uid, name = G.name;
  if (act === "close") return closeModal();
  if (act === "toggle") { G.collapsed[el.dataset.key] = !G.collapsed[el.dataset.key]; return growthPaint(); }
  if (act === "refresh") return growthDlg(uid, name, { silent: true, refresh: true });
  if (act === "back") return growthDlg(uid, name, { refresh: true });
  if (G.busy) return;
  G.busy = true;
  const old = el.textContent;
  $$("#modal [data-g]").forEach(b => { if (b.tagName === "BUTTON") b.disabled = true; });
  el.textContent = "处理中…";
  let showResult = false;
  try {
    const r = await api(`/api/accounts/${uid}/growth/action`, { method: "POST", body: { action: act, task_code: el.dataset.code || "" } });
    if (act === "claim") toast(r.already_claimed ? "已领过" : `+${r.credit} 积分${r.energy ? ` · +${r.energy} 能量` : ""}`);
    else if (act === "claim_all") {
      if (r.failed) { showResult = true; growthResultDlg(r); }
      else toast(`领奖 ${r.done} 项，+${r.credit} 积分${r.energy ? ` · +${r.energy} 能量` : ""}`);
    } else if (act === "accept_all") {
      const reasons = [...new Set(Object.values(r.statuses || {}).filter(v => v !== "accepted").map(v => G_STATUS_TEXT[v] || v))];
      toast(`接受 ${r.done} 项${r.skipped ? `，跳过 ${r.skipped} 项（${reasons.join("、") || "未生效"}）` : ""}`);
    } else toast("已接受");
  } catch (e) { toast("失败：" + e.message); el.textContent = old; }
  finally { G.busy = false; }
  if (!showResult) growthDlg(uid, name, { silent: true, refresh: true });
}
function growthResultDlg(r) {
  const tagCls = { claimed: "ok", failed: "error" };
  const txt = { claimed: "已领", already: "已领过", failed: "失败" };
  modal(`<h3>一键领奖结果 · ${esc(G.name)}</h3>
  <p>成功 ${r.done} 项，+${r.credit} 积分${r.energy ? ` · +${r.energy} 能量` : ""}；<span style="color:var(--bad)">失败 ${r.failed} 项</span></p>
  <table><tr><th>任务</th><th>状态</th><th>说明</th></tr>${(r.results || []).map(x =>
    `<tr><td>${esc(x.title || x.task_code)}</td><td><span class="tag ${tagCls[x.status] || ""}">${txt[x.status] || x.status}</span></td>
     <td style="font-size:12px;color:var(--fg3)">${x.status === "failed" ? esc(x.error) : x.credit ? `+${x.credit} 积分` : "-"}</td></tr>`).join("")}</table>
  <div class="mrow"><button class="primary" data-g="back">返回任务列表</button></div>`, true);
}

// ---------- 成长任务（多账号总览） ----------
async function pageGrowth(force) {
  const d = await api("/api/growth/overview" + (force === true ? "?refresh=1" : ""));
  const accs = d.accounts || [];
  const okUids = accs.filter(a => !a.error).map(a => a.uid);
  const todayCredit = accs.reduce((s, a) => s + (a.stat.today_credit || 0), 0);
  const autoCredit = accs.reduce((s, a) => s + (a.stat.auto_credit || 0), 0);
  const cell = (n, color) => n ? `<b style="color:${color}">${n}</b>` : '<span style="color:var(--fg3)">0</span>';
  $("#page").innerHTML = `
  <div class="cards">
    <div class="card"><div class="l">待领奖</div><div class="v" style="color:var(--ok)">${d.total_claimable}</div><div class="sub">合计 +${d.total_claimable_credit} 积分</div></div>
    <div class="card"><div class="l">今日已领积分</div><div class="v">${fmtN(todayCredit)}</div><div class="sub">其中自动领取 ${fmtN(autoCredit)}（重启后清零）</div></div>
    <div class="card"><div class="l">账号数</div><div class="v">${accs.length}</div><div class="sub">${okUids.length} 个可查询</div></div>
  </div>
  <div class="toolbar">
    <button class="primary" onclick="batch('claim_growth',this,GROWTH_UIDS)">全部账号一键领奖</button>
    <button onclick="batch('accept_growth',this,GROWTH_UIDS)">全部账号全部接受</button>
    <div class="spacer"></div>
    <button onclick="withBusy(this,()=>pageGrowth(true))">刷新（强制）</button>
  </div>
  <div class="panel"><table>
    <tr><th>账号</th><th>待领奖</th><th>未接受</th><th>进行中</th><th>已完成</th><th>今日已领</th><th>操作</th></tr>
    ${accs.map(a => a.error ? `<tr><td><b>${esc(a.name)}</b></td><td colspan="5" style="color:var(--bad);font-size:12px">${esc(a.error)}</td><td></td></tr>` : `<tr>
      <td><b>${esc(a.name)}</b></td>
      <td>${cell(a.summary.claimable, "var(--ok)")}${a.summary.claimable ? ` <span style="color:var(--fg3);font-size:11px">+${a.summary.claimable_credit}</span>` : ""}</td>
      <td>${cell(a.summary.acceptable, "var(--info)")}</td>
      <td>${cell(a.summary.doing, "var(--warn)")}</td>
      <td>${a.summary.done}</td>
      <td>${a.stat.today_credit ? `+${a.stat.today_credit}` : "-"}</td>
      <td style="white-space:nowrap"><button class="mini" data-uid="${esc(a.uid)}" data-name="${esc(a.name)}" onclick="growthDlg(this.dataset.uid,this.dataset.name)">详情</button>
        ${a.summary.claimable ? `<button class="mini primary" onclick="batch('claim_growth',this,[${js(a.uid)}])">领奖</button>` : ""}
        ${a.summary.acceptable ? `<button class="mini" onclick="batch('accept_growth',this,[${js(a.uid)}])">接受</button>` : ""}</td></tr>`).join("")}
  </table>${!accs.length ? '<p style="color:var(--fg3);padding:20px;text-align:center">还没有账号</p>' : ""}</div>
  <details class="panel"><summary style="cursor:pointer;font-weight:600;color:var(--fg2)">按任务汇总（${(d.tasks || []).length} 项）</summary>
    <table style="margin-top:10px"><tr><th>任务</th><th>奖励</th><th>待领奖</th><th>未接受</th><th>进行中</th><th>已完成</th><th>还没接受的账号</th></tr>
    ${(d.tasks || []).map(t => `<tr><td>${esc(t.title)}<div style="color:var(--fg3);font-size:12px">${esc(t.desc || "")}</div></td>
      <td style="white-space:nowrap">+${t.reward_credit}${t.reward_energy ? ` · +${t.reward_energy}` : ""}</td>
      <td>${t.counts.claimable || 0}</td><td>${t.counts.acceptable || 0}</td><td>${t.counts.doing || 0}</td><td>${t.counts.done || 0}</td>
      <td style="font-size:12px;color:var(--fg3)">${esc((t.acceptable_names || []).join("、") || "-")}</td></tr>`).join("")}
    </table></details>`;
  window.GROWTH_UIDS = okUids;
}
async function addAccountDlg() {
  modal(`<h3>添加账号（设备授权登录）</h3>
  <label>版本</label><select id="lg-realm"><option value="cn">国内版（codebuddy.cn）</option><option value="global">国际版（workbuddy.ai）</option></select>
  <div id="lg-step1">
    <div class="mrow"><button onclick="closeModal()">取消</button><button class="primary" onclick="loginStart()">获取授权链接</button></div>
  </div><div id="lg-step2" style="display:none">
    <p>1. 在浏览器打开授权链接完成登录：</p>
    <p style="margin:8px 0"><a id="lg-url" href="#" target="_blank" class="kv"></a></p>
    <p>2. 登录完成后这里会自动检测到（每 3 秒轮询）…</p>
    <p id="lg-status" style="color:var(--warn);margin-top:10px">等待登录…</p>
  </div>`);
}
let loginTimer = null;
async function loginStart() {
  try {
    const r = await api("/api/login/start", { method: "POST", body: { realm: $("#lg-realm").value } });
    $("#lg-step1").style.display = "none"; $("#lg-step2").style.display = "block";
    const a = $("#lg-url"); a.href = r.auth_url; a.textContent = r.auth_url;
    window.open(r.auth_url, "_blank");
    clearInterval(loginTimer);
    // 服务端已在后台接管轮询并自动入池，这里仅查询状态；
    // 单次错误不停止（关掉页面也不影响入池，重开面板即可看到新账号）
    loginTimer = setInterval(async () => {
      try {
        const p = await api("/api/login/poll", { method: "POST", body: { state: r.state } });
        if (p.done) {
          clearInterval(loginTimer); closeModal();
          toast(`账号「${p.nickname}」已加入`); render();
        } else if (p.error) {
          clearInterval(loginTimer); $("#lg-status").textContent = p.error;
        }
      } catch (e) {
        $("#lg-status").textContent = e.message;
        if ((e.message || "").includes("过期")) clearInterval(loginTimer);
      }
    }, 3000);
  } catch (e) { toast(e.message); }
}
function importDlg() {
  modal(`<h3>导入凭证</h3>
  <p style="color:var(--fg2);font-size:12px">支持 PoolMate / PoolService / wb2api 的导出文件（明文或加密）。</p>
  <label>选择文件</label><input type="file" id="imp-file" accept=".json,.enc">
  <label>密码（加密文件时填写）</label><input type="password" id="imp-pass">
  <div class="mrow"><button onclick="closeModal()">取消</button><button class="primary" onclick="doImport()">导入</button></div>`);
}
async function doImport() {
  const f = $("#imp-file").files[0];
  if (!f) return toast("选择文件");
  const raw = await f.arrayBuffer();
  const pass = $("#imp-pass").value;
  try {
    const r = await fetch("/api/accounts/import", {
      method: "POST", headers: { "X-Admin-Key": ADMIN_KEY, "X-Import-Password": encodeURIComponent(pass), "Content-Type": "application/octet-stream" }, body: raw,
    }).then(x => x.json());
    if (r.error) throw new Error(r.error);
    closeModal();
    toast(`导入 ${r.imported.length} 个，跳过 ${r.skipped.length} 个`);
    if (r.skipped.length) setTimeout(() => alert("跳过明细：\n" + r.skipped.join("\n")), 300);
    render();
  } catch (e) { toast("导入失败：" + e.message); }
}
async function exportDlg(onlySel) {
  const d = await api("/api/accounts");
  const accs = d.accounts || [];
  if (onlySel && !acctSel.size) return toast("先勾选账号");
  const allTags = [...new Set(accs.flatMap(a => a.Tags || []))].sort();
  const pre = a => onlySel ? acctSel.has(a.UID) : (!acctTag || (a.Tags || []).includes(acctTag));
  modal(`<h3>导出账号配置</h3>
  <p style="color:var(--bad);font-size:12px">⚠ 导出文件含登录令牌（明文或你设置密码加密），等同于账号密码，请妥善保管。</p>
  ${allTags.length ? `<label>按标签筛选（仅勾选带该标签的账号）</label>
  <select id="exp-tag" onchange="expTagPick(this.value)"><option value="">全部</option>${allTags.map(t => `<option value="${esc(t)}" ${t === acctTag && !onlySel ? "selected" : ""}>${esc(t)}</option>`).join("")}</select>` : ""}
  <div style="max-height:200px;overflow-y:auto;margin:10px 0">
    <label style="margin:0"><input type="checkbox" id="exp-all" onchange="$$('.exp-one').forEach(c=>c.checked=this.checked)"> 全选</label>
    ${accs.map(a => `<label style="margin:0"><input type="checkbox" class="exp-one" data-uid="${esc(a.UID)}" data-tags="${esc((a.Tags || []).map(encodeURIComponent).join(","))}" ${pre(a) ? "checked" : ""}> ${esc(a.Name)}（${a.Realm}）${(a.Tags || []).map(t => `<span class="tag paused" style="margin-left:4px">${esc(t)}</span>`).join("")}</label>`).join("")}
  </div>
  <label>加密密码（留空 = 明文导出）</label><input type="password" id="exp-pass">
  <div class="mrow"><button onclick="closeModal()">取消</button><button class="primary" onclick="doExport()">导出</button></div>`);
}
function expTagPick(tag) {
  $$(".exp-one").forEach(c => { c.checked = !tag || c.dataset.tags.split(",").includes(encodeURIComponent(tag)); });
}
async function doExport() {
  const uids = $$(".exp-one").filter(c => c.checked).map(c => c.dataset.uid);
  if (!uids.length) return toast("勾选账号");
  const r = await fetch("/api/accounts/export", { method: "POST", headers: { "X-Admin-Key": ADMIN_KEY, "Content-Type": "application/json" }, body: JSON.stringify({ uids, password: $("#exp-pass").value }) });
  if (!r.ok) {
    let msg = "HTTP " + r.status;
    try { msg = (await r.json()).error || msg; } catch (_) { }
    return toast("导出失败：" + msg);
  }
  const blob = await r.blob();
  const a = document.createElement("a");
  a.href = URL.createObjectURL(blob);
  a.download = "poolmate-backup-" + new Date().toISOString().slice(0, 10) + ".json";
  a.click(); closeModal(); toast("已导出");
}

// ---------- 密钥 ----------
async function pageKeys() {
  const d = await api("/api/keys");
  const list = d.keys || [];
  $("#page").innerHTML = `
  <div class="toolbar"><button class="primary" onclick="keyDlg()">＋ 新建密钥</button>
  <span style="color:var(--fg3);font-size:12px">网关地址：<span class="kv">http://${location.host}/v1</span>（OpenAI）· <span class="kv">http://${location.host}</span>（Anthropic）</span></div>
  <div class="panel"><table>
    <tr><th>名称</th><th>密钥</th><th>模型白名单</th><th>RPM</th><th>今日/每日预算</th><th>累计/预算</th><th>状态</th><th>操作</th></tr>
    ${list.map(k => `<tr>
      <td><b>${esc(k.name)}</b></td><td class="kv">${esc(k.key)} <button class="mini" onclick="navigator.clipboard.writeText(${js(k.key)});toast('已复制')">⧉</button></td>
      <td>${(k.models || []).length ? esc(k.models.join(", ")) : "全部"}</td>
      <td>${k.rpm || "不限"}</td><td>${fmtN(k.used_daily)} / ${k.daily_budget || "不限"}</td>
      <td>${fmtN(k.used_total)} / ${k.total_budget || "不限"}</td>
      <td><span class="tag ${k.disabled ? "disabled" : "ready"}">${k.disabled ? "停用" : "启用"}</span></td>
      <td style="white-space:nowrap">
        <button class="mini" onclick="keyEdit(${js(k.id)})">编辑</button>
        <button class="mini" onclick="keyRotate(${js(k.id)})">轮换</button>
        <button class="mini" onclick="keyToggle(${js(k.id)},${!k.disabled})">${k.disabled ? "启用" : "停用"}</button>
        <button class="mini danger" onclick="keyDel(${js(k.id)})">删</button>
      </td></tr>`).join("")}
  </table></div>`;
}
async function keyDlg(edit) {
  const m = await api("/api/models");
  const opts = (m.models || []).map(x => x.id);
  modal(`<h3>${edit ? "编辑" : "新建"}密钥</h3>
  <label>名称</label><input id="k-name" value="${edit ? esc(edit.name) : ""}">
  <label>模型白名单（逗号分隔，支持 glm-* 通配；留空 = 全部）</label><input id="k-models" list="k-models-list" placeholder="glm-*, kimi-k3" value="${edit ? esc((edit.models || []).join(", ")) : ""}">
  <datalist id="k-models-list">${opts.map(o => `<option value="${o}">`).join("")}</datalist>
  <label>每分钟请求上限（0 = 不限）</label><input id="k-rpm" type="number" value="${edit ? edit.rpm : 0}">
  <label>每日积分预算（0 = 不限）</label><input id="k-daily" type="number" value="${edit ? edit.daily_budget : 0}">
  <label>累计积分预算（0 = 不限）</label><input id="k-total" type="number" value="${edit ? edit.total_budget : 0}">
  ${!edit ? '<label>有效期（天，0 = 永久）</label><input id="k-expire" type="number" value="0">' : ""}
  <div class="mrow"><button onclick="closeModal()">取消</button><button class="primary" onclick="keySave(${js(edit ? edit.id : "")})">保存</button></div>`);
}
async function keyEdit(id) {
  const d = await api("/api/keys");
  keyDlg((d.keys || []).find(k => k.id === id));
}
async function keySave(id) {
  const body = {
    name: $("#k-name").value, models: $("#k-models").value.split(/[,，]/).map(s => s.trim()).filter(Boolean),
    rpm: +$("#k-rpm").value, daily_budget: +$("#k-daily").value, total_budget: +$("#k-total").value,
  };
  try {
    if (id) {
      await api("/api/keys/" + id, { method: "PUT", body });
    } else {
      body.expire_days = +$("#k-expire").value;
      const k = await api("/api/keys", { method: "POST", body });
      modal(`<h3>密钥已创建</h3><p class="kv" style="padding:12px;background:var(--bg);border-radius:8px">${k.key}</p>
      <p style="color:var(--fg2);font-size:12px;margin-top:8px">请立即保存，它也会出现在密钥列表中。</p>
      <div class="mrow"><button class="primary" onclick="closeModal();render()">完成</button></div>`);
      return;
    }
    closeModal(); toast("已保存"); render();
  } catch (e) { toast(e.message); }
}
async function keyToggle(id, disabled) { await api("/api/keys/" + id, { method: "PUT", body: { disabled } }); render(); }
async function keyDel(id) { if (confirm("确认删除该密钥？")) { await api("/api/keys/" + id, { method: "DELETE" }); render(); } }
async function keyRotate(id) {
  if (!confirm("轮换后旧密钥立即失效，确认？")) return;
  const k = await api(`/api/keys/${id}/rotate`, { method: "POST" });
  modal(`<h3>新密钥</h3><p class="kv" style="padding:12px;background:var(--bg);border-radius:8px">${k.key}</p>
  <div class="mrow"><button class="primary" onclick="closeModal();render()">完成</button></div>`);
}

// ---------- 日志 ----------
async function pageLogs() {
  const d = await api("/api/logs?limit=200");
  const list = d.logs || [];
  $("#page").innerHTML = `
  <div class="toolbar">
    <select id="lf-status" onchange="renderLogs()"><option value="">全部状态</option><option value="ok">成功</option><option value="error">失败</option></select>
    <select id="lf-proto" onchange="renderLogs()"><option value="">全部协议</option><option value="openai">OpenAI</option><option value="anthropic">Anthropic</option></select>
    <input id="lf-model" placeholder="模型名过滤" style="width:180px" oninput="renderLogs()">
  </div>
  <div class="panel"><table id="log-table"></table></div>`;
  window._logs = list;
  renderLogs();
}
function renderLogs() {
  const st = $("#lf-status")?.value || "", pr = $("#lf-proto")?.value || "", mo = ($("#lf-model")?.value || "").toLowerCase();
  const list = (window._logs || []).filter(l =>
    (!st || l.status === st) && (!pr || l.protocol === pr) && (!mo || (l.model || "").toLowerCase().includes(mo)));
  $("#log-table").innerHTML = `<tr><th>时间</th><th>模型</th><th>账号</th><th>密钥</th><th>协议</th><th>状态</th><th>Tokens</th><th>积分</th><th>首字</th><th>总耗时</th></tr>` +
    list.map((l, i) => `<tr onclick="toggleLogDetail(${i})" style="cursor:pointer">
      <td>${fmtTime(l.time)}</td><td>${esc(l.model)}</td><td>${esc(l.account_name || "-")}</td><td>${esc(l.key_name || "-")}</td>
      <td>${l.protocol}</td><td><span class="tag ${l.status}">${l.status}</span></td>
      <td>${l.prompt_tokens}+${l.completion_tokens}</td><td>${fmtN(l.credits)}</td><td>${fmtMs(l.first_token_ms)}</td><td>${fmtMs(l.total_ms)}</td>
    </tr><tr class="log-d" id="log-d-${i}" style="display:none"><td colspan="10"><div class="log-detail">
      选号：${esc(l.select_reason || "-")}　重试：${l.retries}　缓存：${l.cached_tokens}
      ${l.err_msg ? `<br><span style="color:var(--bad)">${esc(l.err_kind)}：${esc(l.err_msg)}</span>` : ""}
    </div></td></tr>`).join("");
}
function toggleLogDetail(i) { const el = $("#log-d-" + i); el.style.display = el.style.display === "none" ? "" : "none"; }

// ---------- 用量 ----------
async function pageUsage() {
  const d = await api("/api/usage");
  const bd = d.breakdown || {}, t = d.today || {}, hours = d.last24h || [];
  const okRate = t.requests ? (t.requests - t.errors) * 100 / t.requests : 100;
  const cacheRate = t.tokens ? t.cache_hits * 100 / t.tokens : 0;
  const avgCredit = t.requests ? t.credits / t.requests : 0;
  const avgFirst = t.first_cnt ? Math.round(t.first_ms / t.first_cnt) : 0;
  $("#page").innerHTML = `
  <div class="cards">
    ${usageCard("今日请求", fmtN(t.requests || 0), `近 24 小时 ${fmtN(hours.reduce((n, x) => n + (x.requests || 0), 0))} 次`)}
    ${usageCard("今日积分消耗", fmtN(t.credits || 0), `平均 ${avgCredit.toFixed(2)} 积分 / 次`)}
    ${usageCard("今日 Tokens", fmtK(t.tokens || 0), `缓存命中 ${cacheRate.toFixed(1)}%（${fmtK(t.cache_hits || 0)}）`)}
    ${usageCard("请求成功率", okRate.toFixed(1) + "%", `${t.errors || 0} 次失败`, okRate < 90 ? "var(--bad)" : okRate < 98 ? "var(--warn)" : "var(--ok)")}
    ${usageCard("平均首字耗时", avgFirst ? fmtMs(avgFirst) : "-", "仅统计有首字数据的请求")}
  </div>
  <div class="panel"><div class="panel-head"><h3>近 24 小时用量</h3><span class="muted">柱高：请求数 · 折线：积分消耗</span></div>${usageChart(hours)}</div>
  <div class="panel"><div class="panel-head"><h3>按模型与账号</h3><span class="muted">点击模型展开账号明细 · 按积分消耗排序</span></div>${modelAccountTable(bd.model, bd.model_account)}</div>
  <div class="grid2">
    <div class="panel"><h3>按密钥</h3>${aggTable(bd.key)}</div>
    <div class="panel"><h3>按协议</h3>${aggTable(bd.protocol)}</div>
  </div>
  <div class="panel"><div class="panel-head"><h3>使用热力图（近 30 天）</h3><span class="muted">颜色越深表示请求越多 · 悬停查看积分</span></div>${heatmap(d.heatmap)}</div>`;
}
function usageCard(label, value, sub, color) {
  return `<div class="card"><div class="l">${label}</div><div class="v" style="color:${color || "inherit"}">${value}</div><div class="sub">${sub}</div></div>`;
}
function usageChart(hours) {
  if (!hours.length) return '<p class="muted">暂无数据</p>';
  const maxReq = Math.max(1, ...hours.map(x => x.requests || 0));
  const maxCredit = Math.max(0.01, ...hours.map(x => x.credits || 0));
  const now = new Date(); now.setMinutes(0, 0, 0);
  const points = hours.map((x, i) => `${(i * 100 / 23).toFixed(2)},${(35 - (x.credits || 0) / maxCredit * 30).toFixed(2)}`).join(" ");
  return `<div class="usage-chart">${hours.map((x, i) => {
    const tm = new Date(now.getTime() - (23 - i) * 3600000);
    const h = (x.requests || 0) / maxReq * 100;
    return `<div class="uchart-col" title="${tm.getMonth() + 1}月${tm.getDate()}日 ${tm.getHours()}:00 · ${x.requests || 0} 次 · ${fmtN(x.credits || 0)} 积分 · ${fmtK(x.tokens || 0)} tokens">
      <div class="uchart-bar-wrap"><div class="uchart-bar ${x.errors ? "has-error" : ""}" style="height:${h}%"></div></div>
      <div class="uchart-label">${i % 4 === 0 || i === 23 ? String(tm.getHours()).padStart(2, "0") : ""}</div></div>`;
  }).join("")}<svg class="uchart-line" viewBox="0 0 100 40" preserveAspectRatio="none"><polyline points="${points}" /></svg></div>
  <div class="chart-legend"><span><i class="legend-bar"></i>请求数</span><span><i class="legend-line"></i>积分消耗</span><span class="muted">时间：${String(new Date(now.getTime() - 23 * 3600000).getHours()).padStart(2, "0")}:00 至现在</span></div>`;
}
function aggTable(m) {
  const arr = Object.entries(m || {}).sort((a, b) => (b[1].credits || 0) - (a[1].credits || 0) || b[1].requests - a[1].requests);
  if (!arr.length) return '<p class="muted">暂无数据</p>';
  const totalCredits = arr.reduce((n, [, v]) => n + (v.credits || 0), 0);
  return `<table class="agg-table"><tr><th>名称</th><th>请求</th><th>成功率</th><th>Tokens</th><th>积分 / 占比</th><th>平均首字</th></tr>${arr.map(([k, v]) =>
    `<tr><td title="${esc(k)}"><b>${esc(k === "-" ? "未记录账号" : k)}</b></td>${aggCells(v, totalCredits)}</tr>`).join("")}</table>`;
}
function aggCells(v, totalCredits) {
  const rate = v.requests ? (v.requests - v.errors) * 100 / v.requests : 100;
  const share = totalCredits > 0 ? (v.credits || 0) * 100 / totalCredits : 0;
  const cacheRate = v.tokens ? (v.cache_hits || 0) * 100 / v.tokens : 0;
  return `<td>${fmtN(v.requests)}</td>
    <td><span style="color:${rate < 90 ? "var(--bad)" : rate < 98 ? "var(--warn)" : "var(--ok)"}">${rate.toFixed(rate < 100 && rate > 99 ? 1 : 0)}%</span>${v.errors ? `<div class="cell-sub">${v.errors} 次失败</div>` : ""}</td>
    <td>${fmtK(v.tokens)}<div class="cell-sub">缓存 ${cacheRate.toFixed(0)}%</div></td>
    <td><b>${fmtN(v.credits)}</b><div class="share-track" title="积分占比 ${share.toFixed(1)}%"><i style="width:${share}%"></i></div><div class="cell-sub">${share.toFixed(1)}%</div></td>
    <td>${v.first_cnt ? fmtMs(Math.round(v.first_ms / v.first_cnt)) : "-"}</td>`;
}
function modelAccountTable(models, pairs) {
  const entries = Object.entries(models || {}).sort((a, b) => (b[1].credits || 0) - (a[1].credits || 0));
  if (!entries.length) return '<p class="muted">暂无数据</p>';
  const totalCredits = entries.reduce((n, [, v]) => n + (v.credits || 0), 0);
  const accounts = {};
  Object.entries(pairs || {}).forEach(([key, v]) => {
    const i = key.indexOf("\0");
    if (i < 0) return;
    const model = key.slice(0, i), account = key.slice(i + 1);
    (accounts[model] ||= []).push([account, v]);
  });
  return `<table class="agg-table model-account-table"><tr><th>模型 / 账号</th><th>请求</th><th>成功率</th><th>Tokens</th><th>积分 / 占比</th><th>平均首字</th></tr>${entries.map(([model, total], i) => {
    const rows = (accounts[model] || []).sort((a, b) => (b[1].credits || 0) - (a[1].credits || 0));
    return `<tbody class="model-group"><tr class="model-total" onclick="toggleModelAccounts(${i})" title="点击展开或收起账号明细">
      <td><span class="model-arrow" id="model-arrow-${i}">▾</span><b>${esc(model)}</b><span class="tag paused">${rows.length} 个账号</span></td>${aggCells(total, totalCredits)}</tr>
      ${rows.map(([account, v]) => `<tr class="account-detail" data-model-detail="${i}"><td><span class="account-branch">└</span>${esc(account === "-" ? "未记录账号" : account)}</td>${aggCells(v, totalCredits)}</tr>`).join("")}</tbody>`;
  }).join("")}</table>`;
}
function toggleModelAccounts(i) {
  const hidden = $(`#model-arrow-${i}`).textContent === "▸";
  $(`#model-arrow-${i}`).textContent = hidden ? "▾" : "▸";
  document.querySelectorAll(`[data-model-detail="${i}"]`).forEach(el => el.style.display = hidden ? "" : "none");
}
function heatmap(h) {
  const days = ["日", "一", "二", "三", "四", "五", "六"];
  let max = 1;
  (h || []).forEach(row => row.forEach(c => max = Math.max(max, c.requests)));
  let html = '<div class="heat"><div></div>';
  for (let i = 0; i < 24; i++) html += `<div style="text-align:center">${i % 3 === 0 ? i : ""}</div>`;
  (h || []).forEach((row, d) => {
    html += `<div style="text-align:right;padding-right:6px">周${days[d]}</div>`;
    row.forEach((c, hour) => {
      const a = c.requests ? 0.18 + Math.sqrt(c.requests / max) * 0.82 : 0;
      html += `<div class="hcell ${c.requests ? "active" : ""}" title="周${days[d]} ${hour}:00–${hour + 1}:00\n${c.requests} 次请求 · ${fmtN(c.credits)} 积分" style="${c.requests ? `background:rgba(240,160,60,${a})` : ""}"></div>`;
    });
  });
  return html + `</div><div class="heat-legend"><span>少</span>${[0.18, 0.38, 0.58, 0.78, 1].map(a => `<i style="background:rgba(240,160,60,${a})"></i>`).join("")}<span>多（峰值 ${max} 次 / 小时）</span></div>`;
}

// ---------- 调试台 ----------
async function pagePlayground() {
  const [m, k] = await Promise.all([api("/api/models"), api("/api/keys")]);
  $("#page").innerHTML = `
  <div class="panel"><h3>API 调试台（请求真实经过网关与账号池）</h3>
    <div class="grid2">
      <div><label>模型</label><select id="pg-model">${(m.models || []).map(x => `<option value="${x.id}">${x.id}（${x.context_window || "?"} ctx）</option>`).join("")}</select></div>
      <div><label>密钥</label><select id="pg-key">${(k.keys || []).filter(x => !x.disabled).map(x => `<option value="${x.key}">${esc(x.name)}</option>`).join("")}</select></div>
    </div>
    <label>消息</label><textarea id="pg-msg" rows="4">你好，用一句话介绍你自己。</textarea>
    <div class="mrow" style="justify-content:flex-start"><button class="primary" id="pg-send" onclick="pgSend()">发送（流式）</button>
    <span id="pg-stat" style="color:var(--fg3);font-size:12px;align-self:center"></span></div>
  </div>
  <div class="panel"><h3>输出</h3><div class="pg-out" id="pg-out"></div><div class="pg-meta" id="pg-meta"></div></div>`;
}
async function pgSend() {
  const btn = $("#pg-send"); btn.disabled = true;
  const out = $("#pg-out"); out.textContent = "";
  $("#pg-meta").textContent = "";
  const t0 = performance.now();
  try {
    const resp = await fetch("/v1/chat/completions", {
      method: "POST", headers: { "Content-Type": "application/json", "Authorization": "Bearer " + $("#pg-key").value },
      body: JSON.stringify({ model: $("#pg-model").value, stream: true, messages: [{ role: "user", content: $("#pg-msg").value }] }),
    });
    if (!resp.ok) {
      const e = await resp.json().catch(() => ({}));
      out.textContent = "错误 " + resp.status + "：" + ((e.error && e.error.message) || resp.statusText);
      btn.disabled = false; return;
    }
    const reader = resp.body.getReader();
    const dec = new TextDecoder();
    let buf = "", first = 0;
    while (true) {
      const { done, value } = await reader.read();
      if (done) break;
      buf += dec.decode(value, { stream: true });
      const lines = buf.split("\n"); buf = lines.pop();
      for (const line of lines) {
        if (!line.startsWith("data:")) continue;
        const p = line.slice(5).trim();
        if (p === "[DONE]") continue;
        try {
          const c = JSON.parse(p);
          const d = c.choices && c.choices[0] && c.choices[0].delta;
          if (d) {
            if (!first && (d.content || d.reasoning_content)) first = performance.now() - t0;
            if (d.reasoning_content) { out.textContent += ""; }
            if (d.content) out.textContent += d.content;
          }
        } catch { }
      }
      $("#pg-stat").textContent = first ? `首字 ${Math.round(first)}ms · 已用时 ${((performance.now() - t0) / 1000).toFixed(1)}s` : "连接中…";
    }
    $("#pg-meta").textContent = `总耗时 ${((performance.now() - t0) / 1000).toFixed(1)}s · 详情见「请求日志」最新一条（含选号理由与积分消耗）`;
  } catch (e) { out.textContent = "请求失败：" + e.message; }
  btn.disabled = false;
}

// ---------- 任务 ----------
async function pageTasks() {
  const [t, h] = await Promise.all([api("/api/tasks"), api("/api/tasks/history")]);
  $("#page").innerHTML = `
  <div class="panel"><h3>定时任务</h3><table>
    <tr><th>任务</th><th>说明</th><th>状态</th><th>下次执行</th><th>操作</th></tr>
    ${(t.tasks || []).map(x => `<tr><td>${x.name}</td><td>${esc(x.desc)}</td>
      <td>${x.running ? '<span class="tag cooling">执行中</span>' : x.enabled ? '<span class="tag ready">已启用</span>' : '<span class="tag disabled">已停用</span>'}</td>
      <td>${x.next_run ? fmtTime(x.next_run) : "-"}</td>
      <td><button class="mini" onclick="runTask(${js(x.name)})">立即执行</button></td></tr>`).join("")}
  </table></div>
  <div class="panel"><h3>执行历史</h3><table>
    <tr><th>任务</th><th>触发</th><th>开始</th><th>成功</th><th>已做</th><th>失败</th><th>跳过</th><th>详情</th></tr>
    ${(h.history || []).map(x => `<tr><td>${x.task}</td><td>${x.source === "manual" ? "手动" : "定时"}</td>
      <td>${fmtTime(x.start)}</td><td>${x.ok}</td><td>${x.already}</td><td style="color:${x.failed ? "var(--bad)" : "inherit"}">${x.failed}</td><td>${x.skipped}</td>
      <td style="font-size:12px;color:var(--fg3)">${esc((x.detail || "").slice(0, 80))}</td></tr>`).join("")}
  </table></div>`;
}
async function runTask(name) { try { await api(`/api/tasks/${name}/run`, { method: "POST" }); toast("已受理，稍后看历史"); setTimeout(render, 2000); } catch (e) { toast(e.message); } }

// ---------- 设置 ----------
async function pageSettings() {
  const c = await api("/api/settings");
  $("#page").innerHTML = `
  <div class="grid2">
    <div class="panel"><h3>网关</h3>
      <label>模型命名</label><select id="s-naming"><option value="short">短名</option><option value="full">全名</option><option value="both">短名 + 全名</option></select>
      <label><input type="checkbox" id="s-creditname" style="width:auto" ${c.gateway.credit_in_model_name ? "checked" : ""}> 模型名附加积分倍率</label>
      <label>出站代理（留空 = 直连；支持 http / https / socks5，如 http://127.0.0.1:7890）</label>
      <input id="s-proxy" placeholder="http://127.0.0.1:7890" value="${esc(c.gateway.proxy || "")}">
    </div>
    <div class="panel"><h3>模型映射与隐藏</h3>
      <label>别名映射（每行一条：别名=上游模型）</label>
      <textarea id="s-aliases" rows="4" placeholder="claude-sonnet=glm-5.3&#10;gpt-5=kimi-k3">${esc(Object.entries(c.gateway.model_aliases || {}).map(([k, v]) => k + "=" + v).join("\n"))}</textarea>
      <label>隐藏模型（逗号分隔，支持尾部 * 通配；不出现在 /v1/models）</label>
      <input id="s-mhide" placeholder="hy3, minimax-*" value="${esc((c.gateway.model_hide || []).join(", "))}">
      <p style="color:var(--fg3);font-size:12px;margin-top:8px">别名：客户端请求别名时自动映射到上游真实模型，并出现在 /v1/models 列表中；密钥白名单按客户端请求的名字匹配。</p>
    </div>
    <div class="panel"><h3>选号与流量治理</h3>
      <label>单账号最大在途</label><input id="s-mif" type="number" value="${c.pool.max_in_flight}">
      <label>到期优先权重倍数</label><input id="s-waste" type="number" step="0.5" value="${c.pool.waste_boost}">
      <label>余额下限（低于不选）</label><input id="s-floor" type="number" value="${c.pool.credit_floor}">
      <label>单账号每日积分预算（0 = 不限）</label><input id="s-dab" type="number" value="${c.pool.daily_account_budget}">
    </div>
    <div class="panel"><h3>定时任务</h3>
      <label><input type="checkbox" id="s-checkin" style="width:auto" ${c.schedule.checkin_enabled ? "checked" : ""}> 每日签到（${(c.schedule.checkin_hours || []).join("、")} 点）</label>
      <label><input type="checkbox" id="s-keepalive" style="width:auto" ${c.schedule.keepalive_enabled ? "checked" : ""}> token 保活（${c.schedule.keepalive_hour} 点）</label>
      <label>余额刷新间隔（分钟，0 = 关）</label><input id="s-brm" type="number" value="${c.schedule.balance_refresh_minutes}">
      <label><input type="checkbox" id="s-health" style="width:auto" ${c.schedule.token_health_enabled ? "checked" : ""}> 凭证健康检查（${c.schedule.token_health_hour} 点）</label>
      <label><input type="checkbox" id="s-autobak" style="width:auto" ${c.schedule.auto_backup_enabled ? "checked" : ""}> 自动备份（保留 ${c.schedule.auto_backup_keep} 份）</label>
      <label><input type="checkbox" id="s-gclaim" style="width:auto" ${c.schedule.growth_claim_enabled ? "checked" : ""}> 成长任务自动领奖（自动接受未接受任务，达标即领）</label>
      <label>领奖轮询间隔（分钟，0 = 关）</label><input id="s-gclaim-min" type="number" value="${c.schedule.growth_claim_minutes}">
      <label><input type="checkbox" id="s-wake" style="width:auto" ${c.schedule.wake_enabled ? "checked" : ""}> 定时唤醒（发最小请求，提前触发额度重置周期；会消耗极少量积分）</label>
      <label>唤醒整点（逗号分隔，0-23）</label><input id="s-wake-hours" value="${esc((c.schedule.wake_hours || []).join(", "))}" placeholder="8">
      <label>唤醒模型（留空 = 自动选倍率最低）</label><input id="s-wake-model" value="${esc(c.schedule.wake_model || "")}">
    </div>
    <div class="panel"><h3>告警通知 <button class="mini" onclick="notifyTest()">发送测试</button></h3>
      <label><input type="checkbox" id="s-nen" style="width:auto" ${c.notify.enabled ? "checked" : ""}> 启用告警</label>
      <label>企业微信机器人 Webhook</label><input id="s-wecom" value="${esc(c.notify.wecom_webhook || "")}">
      <label>Bark 地址（含 Key）</label><input id="s-bark" value="${esc(c.notify.bark_url || "")}">
      <label>Server酱 SendKey</label><input id="s-sct" value="${esc(c.notify.serverchan_key || "")}">
      <label>Telegram Bot Token / ChatID</label><div style="display:flex;gap:8px"><input id="s-tgt" placeholder="token" value="${esc(c.notify.telegram_token || "")}"><input id="s-tgc" placeholder="chat id" value="${esc(c.notify.telegram_chat_id || "")}"></div>
      <label>自定义 Webhook（JSON POST）</label><input id="s-custom" value="${esc(c.notify.custom_url || "")}">
      <label>低余额阈值（0 = 关）</label><input id="s-minb" type="number" value="${c.notify.min_balance}">
      <label><input type="checkbox" id="s-dosage" style="width:auto" ${c.notify.dosage_alert ? "checked" : ""}> 上游用量提醒告警（get-dosage-notify 非 0 时推送）</label>
    </div>
    <div class="panel"><h3>提示词模式</h3>
      <select id="s-pmode"><option value="passthrough">透传（原样，拦截时自动降级）</option><option value="custom">自定义（替换客户端 system）</option><option value="append">追加（客户端 + 网关提示词并用）</option></select>
      <label>自定义提示词</label><textarea id="s-ptext" rows="3">${esc(c.prompt.text || "")}</textarea>
    </div>
    <div class="panel"><h3>凭证保险箱</h3>
      <label><input type="checkbox" id="s-vault" style="width:auto" ${c.security.vault_enabled ? "checked" : ""}> 凭证加密落盘（机器绑定密钥）</label>
      <label>主密码（可选，输入后新写入凭证将用「密码+机器」双重加密；仅内存保存）</label>
      <div style="display:flex;gap:8px"><input type="password" id="s-vpass"><button class="mini" onclick="setVPass()">设置</button></div>
      <p style="color:var(--fg3);font-size:12px;margin-top:8px">注意：设主密码后重启服务需在面板重新输入，否则加密凭证无法读取。</p>
    </div>
  </div>
  <div class="toolbar"><button class="primary" onclick="saveSettings()">保存设置</button>
  <button onclick="runDiagnose()">一键诊断</button>
  <button onclick="backupNow()">立即备份</button>
  <div class="spacer"></div><span id="set-msg" style="color:var(--fg3);font-size:12px"></span></div>
  <div id="diag-out"></div>`;
  // innerHTML 插入的 <script> 不会执行，必须在这里回填下拉框，否则保存时会把这两项静默重置为首项。
  $("#s-naming").value = c.gateway.model_naming || "short";
  $("#s-pmode").value = c.prompt.mode || "passthrough";
}
async function saveSettings() {
  const cur = await api("/api/settings");
  const body = cur;
  body.gateway.model_naming = $("#s-naming").value;
  body.gateway.credit_in_model_name = $("#s-creditname").checked;
  const aliases = {};
  $("#s-aliases").value.split("\n").forEach(line => {
    const i = line.indexOf("=");
    if (i > 0) { const k = line.slice(0, i).trim(), v = line.slice(i + 1).trim(); if (k && v) aliases[k] = v; }
  });
  body.gateway.model_aliases = aliases;
  body.gateway.model_hide = $("#s-mhide").value.split(/[,，]/).map(s => s.trim()).filter(Boolean);
  body.pool.max_in_flight = +$("#s-mif").value;
  body.pool.waste_boost = +$("#s-waste").value;
  body.pool.credit_floor = +$("#s-floor").value;
  body.pool.daily_account_budget = +$("#s-dab").value;
  body.schedule.checkin_enabled = $("#s-checkin").checked;
  body.schedule.keepalive_enabled = $("#s-keepalive").checked;
  body.schedule.balance_refresh_minutes = +$("#s-brm").value;
  body.schedule.token_health_enabled = $("#s-health").checked;
  body.schedule.auto_backup_enabled = $("#s-autobak").checked;
  body.schedule.growth_claim_enabled = $("#s-gclaim").checked;
  body.schedule.growth_claim_minutes = +$("#s-gclaim-min").value;
  body.gateway.proxy = $("#s-proxy").value.trim();
  body.schedule.wake_enabled = $("#s-wake").checked;
  body.schedule.wake_hours = $("#s-wake-hours").value.split(/[,，\s]+/).map(s => parseInt(s, 10)).filter(n => n >= 0 && n <= 23);
  body.schedule.wake_model = $("#s-wake-model").value.trim();
  body.notify.dosage_alert = $("#s-dosage").checked;
  body.notify.enabled = $("#s-nen").checked;
  body.notify.wecom_webhook = $("#s-wecom").value;
  body.notify.bark_url = $("#s-bark").value;
  body.notify.serverchan_key = $("#s-sct").value;
  body.notify.telegram_token = $("#s-tgt").value;
  body.notify.telegram_chat_id = $("#s-tgc").value;
  body.notify.custom_url = $("#s-custom").value;
  body.notify.min_balance = +$("#s-minb").value;
  body.prompt.mode = $("#s-pmode").value;
  body.prompt.text = $("#s-ptext").value;
  body.security.vault_enabled = $("#s-vault").checked;
  try { await api("/api/settings", { method: "PUT", body }); $("#set-msg").textContent = "已保存 " + new Date().toLocaleTimeString(); toast("设置已保存"); }
  catch (e) { toast(e.message); }
}
async function notifyTest() {
  const r = await api("/api/notify/test", { method: "POST" });
  alert("各通道结果：\n" + Object.entries(r).map(([k, v]) => `${k}: ${v}`).join("\n"));
}
async function setVPass() { await api("/api/vault/password", { method: "POST", body: { password: $("#s-vpass").value } }); toast("主密码已设置"); }
async function runDiagnose() {
  $("#diag-out").innerHTML = '<div class="panel"><h3>诊断中…</h3></div>';
  const d = await api("/api/diagnose", { method: "POST" });
  $("#diag-out").innerHTML = `<div class="panel"><h3>诊断报告</h3><table>${(d.checks || []).map(c =>
    `<tr><td class="check-${c.status}">${c.status === "ok" ? "✓" : c.status === "warn" ? "⚠" : "✗"}</td><td>${esc(c.name)}</td><td>${esc(c.detail)}</td></tr>`).join("")}</table></div>`;
}
async function backupNow() { const r = await api("/api/backups/now", { method: "POST" }); toast("已备份到 " + r.target); }

// ---------- 模型与档位 ----------
let MF = { q: "", cap: "all" };
function fmtTok(n) { return !n ? "—" : n >= 1e6 ? (n / 1e6).toFixed(n % 1e6 ? 1 : 0) + "M" : n >= 1000 ? Math.round(n / 1000) + "K" : String(n); }
async function pageModels() {
  const d = await api("/api/models");
  window._models = d.models || [];
  $("#page").innerHTML = `
  <div class="toolbar">
    <input id="mf-q" placeholder="搜索模型 ID / 名称" style="width:230px" value="${esc(MF.q)}" oninput="MF.q=this.value;renderModelRows()">
    <select style="width:120px" onchange="MF.cap=this.value;renderModelRows()">
      <option value="all">全部能力</option><option value="tools">工具</option><option value="vision">视觉</option><option value="think">思考</option>
    </select>
    <span id="mf-count" style="color:var(--fg3);font-size:12px"></span>
    <div class="spacer"></div>
    <button onclick="refreshModels()">重新获取</button>
  </div>
  <div class="panel" style="overflow-x:auto"><table>
    <tr><th>模型</th><th>积分倍率</th><th>默认档（网关注入）</th><th>支持的思考档位</th><th>上下文长度</th><th>最大输出</th></tr>
    <tbody id="model-rows"></tbody>
  </table>
  <p style="color:var(--fg3);font-size:12px;margin-top:10px">默认档 / 上下文长度：客户端未显式指定时由网关注入（reasoning_effort / context_window），客户端自带参数优先；最大输出：请求 max_tokens 缺失或超过该值时自动钳制。三项留空均跟随上游默认，修改即保存。</p>
  </div>`;
  renderModelRows();
}
function renderModelRows() {
  const q = MF.q.toLowerCase();
  const list = (window._models || []).filter(m =>
    (!q || m.id.toLowerCase().includes(q) || (m.name || "").toLowerCase().includes(q)) &&
    (MF.cap === "all" || (MF.cap === "tools" && m.tools) || (MF.cap === "vision" && m.vision) || (MF.cap === "think" && (m.reasoning || []).length)));
  $("#mf-count").textContent = `${list.length} / ${(window._models || []).length} 个模型`;
  $("#model-rows").innerHTML = list.map(m => {
    const ov = m.override || {};
    if (m.alias_of) {
      return `<tr><td><b>${esc(m.id)}</b> <span class="tag info">别名 → ${esc(m.alias_of)}</span></td>
        <td colspan="5" style="color:var(--fg3);font-size:12px">别名映射在「设置 → 模型映射与隐藏」中管理</td></tr>`;
    }
    const effCtx = ov.context_window > 0 ? ov.context_window : m.context_window;
    const effOut = ov.max_output > 0 ? ov.max_output : m.max_output;
    const thinkTag = (m.reasoning || []).length
      ? (m.can_disable_thinking ? '<span class="tag warn">思考可关</span>' : '<span class="tag warn">思考常开</span>') : "";
    const tags = [m.tools ? '<span class="tag ok">工具</span>' : "", m.vision ? '<span class="tag info">视觉</span>' : "", thinkTag].filter(Boolean).join(" ");
    // 默认档：上游默认值展示在占位项里；注入值选中高亮
    const effSel = (m.reasoning || []).length
      ? `<select id="ov-eff-${esc(m.id)}" style="width:130px;${ov.effort ? "border-color:var(--accent)" : ""}" onchange="saveOv(${js(m.id)})">
           <option value="">上游默认${m.default_effort ? "（" + m.default_effort + "）" : ""}</option>${m.reasoning.map(r => `<option value="${r}" ${ov.effort === r ? "selected" : ""}>${r}</option>`).join("")}</select>`
      : '<span style="color:var(--fg3)">—</span>';
    // 上下文长度：有上游长度档用下拉，否则数字输入
    const lens = m.supported_lengths || [];
    const ctxCtl = lens.length
      ? `<select id="ov-ctx-${esc(m.id)}" style="width:130px;${ov.context_window > 0 ? "border-color:var(--accent)" : ""}" onchange="saveOv(${js(m.id)})">
           <option value="0">上游默认（${fmtTok(m.context_window)}）</option>${lens.map(L => `<option value="${L}" ${ov.context_window === L ? "selected" : ""}>${fmtTok(L)}</option>`).join("")}</select>`
      : `<input id="ov-ctx-${esc(m.id)}" type="number" min="0" style="width:110px;${ov.context_window > 0 ? "border-color:var(--accent)" : ""}" value="${ov.context_window || ""}" placeholder="${m.context_window || ""}" onchange="saveOv(${js(m.id)})">`;
    const tiers = (m.reasoning || []).length
      ? m.reasoning.map(r => `<span class="tag info">${esc(r)}</span>`).join(" ") + (m.can_disable_thinking ? ' <span class="tag">off（可关）</span>' : "")
      : '<span style="color:var(--fg3)">—</span>';
    return `<tr>
      <td><b>${esc(m.id)}</b><br><span style="color:var(--fg3);font-size:12px">${esc(m.name || "")}</span><br>${tags}${m.description ? `<div style="font-size:11px;color:var(--fg3);margin-top:3px;max-width:280px">${esc(m.description)}</div>` : ""}</td>
      <td>${m.rate ? "x" + m.rate : "—"}</td>
      <td>${effSel}</td>
      <td>${tiers}</td>
      <td>${ctxCtl}<div style="font-size:11px;color:${ov.context_window > 0 ? "var(--accent)" : "var(--fg3)"}">${fmtTok(effCtx)}</div></td>
      <td><input id="ov-out-${esc(m.id)}" type="number" min="0" style="width:100px;${ov.max_output > 0 ? "border-color:var(--accent)" : ""}" value="${ov.max_output || ""}" placeholder="${m.max_output || ""}" onchange="saveOv(${js(m.id)})">
        <div style="font-size:11px;color:${ov.max_output > 0 ? "var(--accent)" : "var(--fg3)"}">${fmtTok(effOut)}</div></td>
    </tr>`;
  }).join("");
}
async function saveOv(id) {
  const g = k => document.getElementById(`ov-${k}-${id}`);
  const body = {
    context_window: g("ctx") ? +g("ctx").value || 0 : 0,
    max_output: g("out") ? +g("out").value || 0 : 0,
    effort: g("eff") ? g("eff").value : "",
  };
  try {
    await api(`/api/models/${encodeURIComponent(id)}/override`, { method: "PUT", body });
    const m = (window._models || []).find(x => x.id === id);
    if (m) m.override = body.context_window || body.max_output || body.effort ? body : {};
    renderModelRows();
    toast(`${id} 已保存`);
  } catch (e) { toast(e.message); }
}
async function refreshModels() {
  try { const r = await api("/api/models/refresh", { method: "POST" }); toast(`已重新拉取，共 ${r.count} 个模型`); pageModels(); }
  catch (e) { toast(e.message); }
}

const PAGES = {
  overview: pageOverview, accounts: pageAccounts, growth: () => pageGrowth(), keys: pageKeys, models: pageModels, logs: pageLogs,
  usage: pageUsage, playground: pagePlayground, tasks: pageTasks, settings: pageSettings,
};

// ---------- 启动 ----------
function boot() {
  document.documentElement.dataset.theme = localStorage.getItem("pm_theme") || "";
  if (!ADMIN_KEY) { $("#key-gate").style.display = "flex"; $("#app").style.display = "none"; return; }
  $("#key-gate").style.display = "none"; $("#app").style.display = "block";
  render();
}
boot();
