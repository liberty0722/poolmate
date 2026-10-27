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
function modal(html) { $("#modal").innerHTML = html; $("#modal-mask").classList.add("show"); }
function closeModal() { $("#modal-mask").classList.remove("show"); }
$("#modal-mask").addEventListener("click", e => { if (e.target.id === "modal-mask") closeModal(); });
function fmtN(n) { return n == null ? "-" : Number(n).toLocaleString("zh-CN", { maximumFractionDigits: 2 }); }
function fmtTime(t) { if (!t || t.startsWith("0001")) return "-"; const d = new Date(t); return d.toLocaleString("zh-CN", { hour12: false, month: "2-digit", day: "2-digit", hour: "2-digit", minute: "2-digit" }); }
function fmtMs(ms) { return ms > 1000 ? (ms / 1000).toFixed(1) + "s" : ms + "ms"; }
function esc(s) { return String(s ?? "").replace(/[&<>"]/g, c => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" }[c])); }
function gateSubmit() { ADMIN_KEY = $("#key-input").value.trim(); sessionStorage.setItem("pm_key", ADMIN_KEY); boot(); }
function logout() { sessionStorage.removeItem("pm_key"); ADMIN_KEY = ""; location.reload(); }
function toggleTheme() {
  const h = document.documentElement;
  const v = h.dataset.theme === "light" ? "" : "light";
  h.dataset.theme = v; localStorage.setItem("pm_theme", v);
}

// ---------- 路由 ----------
const TABS = [
  ["overview", "概览"], ["accounts", "账号"], ["keys", "密钥"], ["models", "模型"],
  ["logs", "请求日志"], ["usage", "用量分析"], ["playground", "调试台"],
  ["tasks", "任务"], ["settings", "设置"],
];
function renderTabs() {
  $("#tabs").innerHTML = TABS.map(([id, name]) =>
    `<button class="${id === TAB ? "active" : ""}" onclick="go('${id}')">${name}</button>`).join("");
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
async function pageAccounts() {
  const d = await api("/api/accounts");
  const list = d.accounts || [];
  $("#page").innerHTML = `
  <div class="toolbar">
    <button class="primary" onclick="addAccountDlg()">＋ 添加账号</button>
    <button onclick="importDlg()">导入凭证</button>
    <button onclick="exportDlg()">导出配置</button>
    <div class="spacer"></div>
    <span style="color:var(--fg3);font-size:12px" id="sel-info"></span>
    <button class="mini" onclick="batch('refresh')">刷新</button>
    <button class="mini" onclick="batch('checkin')">签到</button>
    <button class="mini" onclick="batch('pause')">暂停</button>
    <button class="mini" onclick="batch('resume')">恢复</button>
    <button class="mini" onclick="batch('revive')">解冻</button>
    <button class="mini danger" onclick="batch('delete')">删除</button>
  </div>
  <div class="panel"><table>
    <tr><th><input type="checkbox" onchange="selAll(this.checked)"></th><th>状态</th><th>账号</th><th>域</th>
    <th>余额</th><th>7天到期</th><th>最近积分包到期</th><th>今日消耗</th><th>凭证剩余</th><th>操作</th></tr>
    ${list.map(a => `<tr>
      <td><input type="checkbox" class="sel" data-uid="${a.UID}" onchange="selOne('${a.UID}',this.checked)" ${acctSel.has(a.UID) ? "checked" : ""}></td>
      <td><span class="tag ${a.Status}" title="${esc(a.StatusWhy || "")}">${statusText(a.Status)}</span></td>
      <td><b>${esc(a.Name || a.UID.slice(0, 8))}</b></td>
      <td><span class="tag ${a.Realm}">${a.Realm === "global" ? "国际版" : "国内版"}</span></td>
      <td>${fmtN(a.Balance)}</td>
      <td style="color:${a.Expiring7d > 0 ? "var(--warn)" : "inherit"}">${fmtN(a.Expiring7d)}</td>
      <td>${a.NextExpiry ? fmtTime(a.NextExpiry) : "-"}</td>
      <td>${fmtN(a.DailyCredit)}</td>
      <td style="color:${a.ExpiryDays < 7 ? "var(--bad)" : "inherit"}">${a.ExpiryDays.toFixed(0)} 天</td>
      <td style="white-space:nowrap">
        <button class="mini" onclick="acct('${a.UID}','refresh')">刷新</button>
        <button class="mini" onclick="acct('${a.UID}','checkin')">签到</button>
        ${a.Status === "paused" ? `<button class="mini" onclick="acct('${a.UID}','resume')">恢复</button>` : `<button class="mini" onclick="acct('${a.UID}','pause')">暂停</button>`}
        <button class="mini" onclick="acct('${a.UID}','revive')">解冻</button>
        <button class="mini danger" onclick="delAcct('${a.UID}','${esc(a.Name)}')">删</button>
      </td></tr>`).join("")}
  </table>${!list.length ? '<p style="color:var(--fg3);padding:20px;text-align:center">还没有账号，点击「添加账号」开始</p>' : ""}</div>`;
  updateSelInfo();
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
  <div class="mrow"><button onclick="closeModal()">取消</button><button class="danger" onclick="doDel('${uid}')">确认删除</button></div>`);
}
async function doDel(uid) { try { await api(`/api/accounts/${uid}/action`, { method: "POST", body: { action: "delete" } }); closeModal(); toast("已删除"); render(); } catch (e) { toast(e.message); } }
async function batch(action) {
  if (!acctSel.size) return toast("先勾选账号");
  try {
    const r = await api("/api/accounts/batch", { method: "POST", body: { action, uids: [...acctSel] } });
    toast(`成功 ${r.ok} 个，失败 ${r.failed} 个`); render();
  } catch (e) { toast(e.message); }
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
    loginTimer = setInterval(async () => {
      try {
        const p = await api("/api/login/poll", { method: "POST", body: { state: r.state } });
        if (p.done) {
          clearInterval(loginTimer); closeModal();
          toast(`账号「${p.nickname}」已加入`); render();
        }
      } catch (e) { clearInterval(loginTimer); $("#lg-status").textContent = e.message; }
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
    const r = await fetch("/api/accounts/import?password=" + encodeURIComponent(pass), {
      method: "POST", headers: { "X-Admin-Key": ADMIN_KEY, "Content-Type": "application/octet-stream" }, body: raw,
    }).then(x => x.json());
    if (r.error) throw new Error(r.error);
    closeModal();
    toast(`导入 ${r.imported.length} 个，跳过 ${r.skipped.length} 个`);
    if (r.skipped.length) setTimeout(() => alert("跳过明细：\n" + r.skipped.join("\n")), 300);
    render();
  } catch (e) { toast("导入失败：" + e.message); }
}
async function exportDlg() {
  const d = await api("/api/accounts");
  modal(`<h3>导出账号配置</h3>
  <p style="color:var(--bad);font-size:12px">⚠ 导出文件含登录令牌（明文或你设置密码加密），等同于账号密码，请妥善保管。</p>
  <div style="max-height:200px;overflow-y:auto;margin:10px 0">
    <label style="margin:0"><input type="checkbox" id="exp-all" checked onchange="$$('.exp-one').forEach(c=>c.checked=this.checked)"> 全选</label>
    ${(d.accounts || []).map(a => `<label style="margin:0"><input type="checkbox" class="exp-one" data-uid="${a.UID}" checked> ${esc(a.Name)}（${a.Realm}）</label>`).join("")}
  </div>
  <label>加密密码（留空 = 明文导出）</label><input type="password" id="exp-pass">
  <div class="mrow"><button onclick="closeModal()">取消</button><button class="primary" onclick="doExport()">导出</button></div>`);
}
async function doExport() {
  const uids = $$(".exp-one").filter(c => c.checked).map(c => c.dataset.uid);
  if (!uids.length) return toast("勾选账号");
  const r = await fetch("/api/accounts/export", { method: "POST", headers: { "X-Admin-Key": ADMIN_KEY, "Content-Type": "application/json" }, body: JSON.stringify({ uids, password: $("#exp-pass").value }) });
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
      <td><b>${esc(k.name)}</b></td><td class="kv">${esc(k.key)} <button class="mini" onclick="navigator.clipboard.writeText('${k.key}');toast('已复制')">⧉</button></td>
      <td>${(k.models || []).length ? esc(k.models.join(", ")) : "全部"}</td>
      <td>${k.rpm || "不限"}</td><td>${fmtN(k.used_daily)} / ${k.daily_budget || "不限"}</td>
      <td>${fmtN(k.used_total)} / ${k.total_budget || "不限"}</td>
      <td><span class="tag ${k.disabled ? "disabled" : "ready"}">${k.disabled ? "停用" : "启用"}</span></td>
      <td style="white-space:nowrap">
        <button class="mini" onclick="keyEdit('${k.id}')">编辑</button>
        <button class="mini" onclick="keyRotate('${k.id}')">轮换</button>
        <button class="mini" onclick="keyToggle('${k.id}',${!k.disabled})">${k.disabled ? "启用" : "停用"}</button>
        <button class="mini danger" onclick="keyDel('${k.id}')">删</button>
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
  <div class="mrow"><button onclick="closeModal()">取消</button><button class="primary" onclick="keySave('${edit ? edit.id : ""}')">保存</button></div>`);
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
  const bd = d.breakdown || {};
  $("#page").innerHTML = `
  <div class="grid2">
    <div class="panel"><h3>按模型</h3>${aggTable(bd.model)}</div>
    <div class="panel"><h3>按账号</h3>${aggTable(bd.account)}</div>
    <div class="panel"><h3>按密钥</h3>${aggTable(bd.key)}</div>
    <div class="panel"><h3>按协议</h3>${aggTable(bd.protocol)}</div>
  </div>
  <div class="panel"><h3>使用热力图（星期 × 小时，近 30 天）</h3>${heatmap(d.heatmap)}</div>`;
}
function aggTable(m) {
  const arr = Object.entries(m || {}).sort((a, b) => b[1].requests - a[1].requests);
  if (!arr.length) return '<p style="color:var(--fg3)">暂无数据</p>';
  return `<table><tr><th>名称</th><th>请求</th><th>成功率</th><th>Tokens</th><th>积分</th><th>平均首字</th></tr>${arr.map(([k, v]) =>
    `<tr><td>${esc(k)}</td><td>${v.requests}</td><td>${v.requests ? Math.round((v.requests - v.errors) * 100 / v.requests) : 100}%</td>
     <td>${fmtK(v.tokens)}</td><td>${fmtN(v.credits)}</td><td>${v.first_cnt ? fmtMs(Math.round(v.first_ms / v.first_cnt)) : "-"}</td></tr>`).join("")}</table>`;
}
function heatmap(h) {
  const days = ["日", "一", "二", "三", "四", "五", "六"];
  let max = 1;
  (h || []).forEach(row => row.forEach(c => max = Math.max(max, c.requests)));
  let html = '<div class="heat"><div></div>';
  for (let i = 0; i < 24; i++) html += `<div style="text-align:center">${i}</div>`;
  (h || []).forEach((row, d) => {
    html += `<div style="text-align:right;padding-right:6px">周${days[d]}</div>`;
    row.forEach(c => {
      const a = c.requests / max;
      html += `<div class="hcell" title="周${days[d]} ${row.indexOf(c)}时：${c.requests} 次 / ${fmtN(c.credits)} 分" style="background:rgba(240,160,60,${0.06 + a * 0.9})"></div>`;
    });
  });
  return html + "</div>";
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
      <td><button class="mini" onclick="runTask('${x.name}')">立即执行</button></td></tr>`).join("")}
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
      <label>模型命名</label><select id="s-naming"><option value="short">短名</option><option value="full">全名</option></select>
      <label><input type="checkbox" id="s-creditname" style="width:auto" ${c.gateway.credit_in_model_name ? "checked" : ""}> 模型名附加积分倍率</label>
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
    </div>
    <div class="panel"><h3>告警通知 <button class="mini" onclick="notifyTest()">发送测试</button></h3>
      <label><input type="checkbox" id="s-nen" style="width:auto" ${c.notify.enabled ? "checked" : ""}> 启用告警</label>
      <label>企业微信机器人 Webhook</label><input id="s-wecom" value="${esc(c.notify.wecom_webhook || "")}">
      <label>Bark 地址（含 Key）</label><input id="s-bark" value="${esc(c.notify.bark_url || "")}">
      <label>Server酱 SendKey</label><input id="s-sct" value="${esc(c.notify.serverchan_key || "")}">
      <label>Telegram Bot Token / ChatID</label><div style="display:flex;gap:8px"><input id="s-tgt" placeholder="token" value="${esc(c.notify.telegram_token || "")}"><input id="s-tgc" placeholder="chat id" value="${esc(c.notify.telegram_chat_id || "")}"></div>
      <label>自定义 Webhook（JSON POST）</label><input id="s-custom" value="${esc(c.notify.custom_url || "")}">
      <label>低余额阈值（0 = 关）</label><input id="s-minb" type="number" value="${c.notify.min_balance}">
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
  <div id="diag-out"></div>
  <script>document.getElementById('s-naming').value='${c.gateway.model_naming || "short"}';document.getElementById('s-pmode').value='${c.prompt.mode || "passthrough"}';<\/script>`;
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
      ? `<select id="ov-eff-${m.id}" style="width:130px;${ov.effort ? "border-color:var(--accent)" : ""}" onchange="saveOv('${m.id}')">
           <option value="">上游默认${m.default_effort ? "（" + m.default_effort + "）" : ""}</option>${m.reasoning.map(r => `<option value="${r}" ${ov.effort === r ? "selected" : ""}>${r}</option>`).join("")}</select>`
      : '<span style="color:var(--fg3)">—</span>';
    // 上下文长度：有上游长度档用下拉，否则数字输入
    const lens = m.supported_lengths || [];
    const ctxCtl = lens.length
      ? `<select id="ov-ctx-${m.id}" style="width:130px;${ov.context_window > 0 ? "border-color:var(--accent)" : ""}" onchange="saveOv('${m.id}')">
           <option value="0">上游默认（${fmtTok(m.context_window)}）</option>${lens.map(L => `<option value="${L}" ${ov.context_window === L ? "selected" : ""}>${fmtTok(L)}</option>`).join("")}</select>`
      : `<input id="ov-ctx-${m.id}" type="number" min="0" style="width:110px;${ov.context_window > 0 ? "border-color:var(--accent)" : ""}" value="${ov.context_window || ""}" placeholder="${m.context_window || ""}" onchange="saveOv('${m.id}')">`;
    const tiers = (m.reasoning || []).length
      ? m.reasoning.map(r => `<span class="tag info">${esc(r)}</span>`).join(" ") + (m.can_disable_thinking ? ' <span class="tag">off（可关）</span>' : "")
      : '<span style="color:var(--fg3)">—</span>';
    return `<tr>
      <td><b>${esc(m.id)}</b><br><span style="color:var(--fg3);font-size:12px">${esc(m.name || "")}</span><br>${tags}${m.description ? `<div style="font-size:11px;color:var(--fg3);margin-top:3px;max-width:280px">${esc(m.description)}</div>` : ""}</td>
      <td>${m.rate ? "x" + m.rate : "—"}</td>
      <td>${effSel}</td>
      <td>${tiers}</td>
      <td>${ctxCtl}<div style="font-size:11px;color:${ov.context_window > 0 ? "var(--accent)" : "var(--fg3)"}">${fmtTok(effCtx)}</div></td>
      <td><input id="ov-out-${m.id}" type="number" min="0" style="width:100px;${ov.max_output > 0 ? "border-color:var(--accent)" : ""}" value="${ov.max_output || ""}" placeholder="${m.max_output || ""}" onchange="saveOv('${m.id}')">
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
  overview: pageOverview, accounts: pageAccounts, keys: pageKeys, models: pageModels, logs: pageLogs,
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
