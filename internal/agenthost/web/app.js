"use strict";
const $ = (id) => document.getElementById(id);
const settingsSessionKey = "codexrm.settingsSession";
function readSettingsSession() {
  try { return localStorage.getItem(settingsSessionKey) || ""; } catch { return ""; }
}
// URL fragments are never sent in HTTP requests or referrers. Keeping the
// capability here also lets the user refresh this page during this agent run.
let token = location.hash.slice(1) || readSettingsSession();
let sessionLogin = false;
let saved = null;
let busy = false;
let pairingExpiresAt = 0;
let pairingTimer = null;
let noticeTimer = null;
const notice = (message, error = false) => {
  clearTimeout(noticeTimer);
  const element = $("notice");
  element.textContent = message;
  element.className = error ? "toast error" : "toast";
  element.hidden = !message;
  if (message) noticeTimer = setTimeout(() => { element.hidden = true; }, error ? 6000 : 3000);
};
async function api(path, options = {}) {
  const headers = new Headers(options.headers);
  if (!(typeof FormData !== "undefined" && options.body instanceof FormData) && !headers.has("Content-Type")) {
    headers.set("Content-Type", "application/json");
  }
  if (token && !headers.has("Authorization")) headers.set("Authorization", `Bearer ${token}`);
  const response = await fetch(path, { ...options, headers, cache: "no-store" });
  const text = await response.text();
  let data;
  try { data = JSON.parse(text); } catch { if (!response.ok) throw new Error(text.trim()); throw new Error("服务器返回了无效的数据"); }
  if (!response.ok) {
    const error = new Error(data.error || "操作失败，请稍后重试");
    error.passwordRequired = response.status === 401 && data.passwordRequired === true;
    error.sessionLogin = data.sessionLogin === true;
    throw error;
  }
  return data;
}
function optionalStatus(id, label, value, time = false) {
  const element = $(id);
  element.hidden = !value;
  element.textContent = value ? `${label}${time ? new Date(value).toLocaleString() : value}` : "";
}
function status(data) {
  let update = data.updateStatus || (typeof data.update === "string" ? data.update : data.update?.status || data.update?.message) || "";
  if (data.updatePending) update = "更新已就绪，等待所有进行中的对话和排队消息结束后自动安装。";
  $("update-status").textContent = update || "启动时检查更新，之后每 20 小时自动检查一次。";
  $("status").textContent = data.status || "状态未知";
  $("overview-connection").textContent = data.status || "状态未知";
  $("overview-connection").classList.toggle("metric-online", data.status === "已连接");
  $("status").className = `status ${data.status === "已连接" ? "connected" : data.status === "连接失败" ? "failed" : ""}`;
  $("connection-error").textContent = data.status === "已连接" ? "" : data.error || "";
  const phases = { connecting: "正在连接中继", connected: "中继已连接", retrying: "正在等待重试", degraded: "连接受限，部分功能可能不可用" };
  optionalStatus("connection-phase", "连接阶段：", phases[data.state] || (data.state ? String(data.state) : ""));
  optionalStatus("last-connected", "最近连接：", data.lastConnectedAt, true);
  optionalStatus("next-retry", "下次重试：", data.nextRetryAt, true);
}
function apply(data) {
  $("server").value = data.server || "";
  $("allow-file-management").checked = data.allowFileManagement === true;
  $("overview-server").textContent = data.server || "尚未配置服务器";
  $("update-card").hidden = !["windows-agent", "linux-agent"].includes(data.updateMode);
  $("linux-update-card").hidden = data.updateMode !== "linux-manual";
  $("package-settings").hidden = !data.packaged;
  $("key").value = "";
  $("key").disabled = false;
  $("toggle-key").disabled = false;
  const clearKey = $("clear-key");
  const clearKeyRow = $("clear-key-row");
  if (clearKey) clearKey.checked = false;
  if (clearKeyRow) clearKeyRow.hidden = !data.enrollmentKeySet;
  $("key").placeholder = data.enrollmentKeySet ? "已保存密钥；留空保留，输入新密钥可替换" : "输入服务器的连接授权密钥";
  $("key-hint").textContent = data.enrollmentKeySet ? "留空保留当前密钥，输入新密钥可替换；勾选清除将删除已保存密钥。" : "公共中继无需密钥。自托管服务器按管理员提供的信息填写。";
  $("version").textContent = data.version;
  status(data);
  saved = data;
}
async function action(button, work) {
  if (busy) return;
  busy = true;
  button.disabled = true;
  try { await work(); } catch (error) { notice(error.message, true); } finally { button.disabled = button === $("copy-code") && !pairingExpiresAt; busy = false; }
}
function dirty() {
  return !saved || $("server").value !== saved.server || $("key").value !== "" || $("clear-key")?.checked === true ||
    $("allow-file-management").checked !== (saved.allowFileManagement === true);
}
function requireSaved() {
  if (dirty()) throw new Error("请先保存连接配置，再进行配对或设备管理。");
  if (!saved.server) throw new Error("请先填写远程服务器地址。");
}
async function devices() {
  requireSaved();
  const rows = await api("/api/devices");
  $("overview-devices").textContent = rows.filter(row => row.deviceType !== "robot").length;
  const deviceList = $("devices");
  deviceList.replaceChildren();
  for (const row of rows) {
    if (row.deviceType === "robot") continue;
    const item = document.createElement("div");
    item.className = "device";
    const icon = document.createElement("span");
    icon.className = "device-icon";
    icon.textContent = row.deviceType === "mobile" ? "▯" : "▣";
    const info = document.createElement("div");
    info.className = "device-info";
    const deviceName = row.clientName || "未命名设备";
    const clientId = row.clientId || "未知 ID";
    const shortId = clientId.slice(0, 8);
    const pairedAt = row.createdAt ? new Date(row.createdAt).toLocaleString() : "未知时间";
    const lastConnectedAt = row.lastConnectedAt ? new Date(row.lastConnectedAt).toLocaleString() : "尚无记录";
    const lastIP = row.lastIP || "尚无记录";
    const name = document.createElement("strong");
    name.textContent = deviceName;
    name.title = deviceName;
    const identity = document.createElement("p");
    identity.className = "hint";
    identity.textContent = `${{ desktop: "桌面设备", mobile: "移动设备", browser: "浏览器" }[row.deviceType] || "其他设备"} · ID ${shortId}`;
    const date = document.createElement("p");
    date.className = "hint";
    date.textContent = `配对于 ${pairedAt}`;
    const connected = document.createElement("p");
    connected.className = "hint";
    connected.textContent = `最近连接：${lastConnectedAt}`;
    const ip = document.createElement("p");
    ip.className = "hint";
    ip.textContent = `最近 IP：${lastIP}`;
    info.append(name, identity, date, connected, ip);
    const remove = document.createElement("button");
    remove.type = "button";
    remove.className = "text-button";
    remove.textContent = "移除授权";
    remove.addEventListener("click", () => {
      if (!confirm(`移除“${deviceName}”（ID ${shortId}）的访问授权？\n最近 IP：${lastIP}\n最近连接：${lastConnectedAt}\n配对于：${pairedAt}`)) return;
      action(remove, async () => {
        requireSaved();
        await api(`/api/devices/${encodeURIComponent(row.clientId)}`, { method: "DELETE" });
        try { await devices(); notice("设备授权已移除。"); }
        catch (error) { notice(`设备授权已移除，但刷新设备列表失败：${error.message}`, true); }
      });
    });
    item.append(icon, info, remove);
    deviceList.append(item);
  }
  if (!deviceList.childElementCount) {
    const empty = document.createElement("p");
    empty.className = "empty";
    empty.textContent = "暂无已配对设备。";
    deviceList.append(empty);
  }
}
$("settings-form").addEventListener("submit", (event) => {
  event.preventDefault();
  action($("save"), async () => {
    const input = {
      server: $("server").value.trim(),
      allowFileManagement: $("allow-file-management").checked,
    };
    if ($("clear-key")?.checked) input.enrollmentKey = "";
    else if ($("key").value) input.enrollmentKey = $("key").value;
    const data = await api("/api/settings", { method: "PUT", body: JSON.stringify(input) });
    apply(data);
    clearPairing();
    $("overview-devices").textContent = "—";
    $("devices").replaceChildren();
    notice("设置已保存，Agent 正在连接服务器。");
  });
});
$("check-update").addEventListener("click", () => action($("check-update"), async () => {
  if (dirty()) throw new Error("请先保存设置，再检查更新。");
  notice("正在检查更新；发现新版本后会在所有对话和排队消息结束后自动安装。");
  const data = await api("/api/update", { method: "POST" });
  notice(data.updatePending ? "更新已就绪，等待所有进行中的对话和排队消息结束后自动安装。" : data.updateAvailable ? "更新已校验，Agent 正在重启完成安装。" : "当前已是最新版本。");
}));
$("pair").addEventListener("click", () => action($("pair"), async () => {
  requireSaved();
  clearPairing();
  const data = await api("/api/pair", { method: "POST" });
  pairingExpiresAt = Number(data.expiresAt) * 1000;
  $("pair-code").textContent = data.code;
  if (data.url) {
    $("pair-link").href = data.url;
    $("pair-link").textContent = "打开远程网页 ↗";
    $("pair-link").removeAttribute("aria-disabled");
  } else {
    $("pair-link").removeAttribute("href");
    $("pair-link").textContent = "如需使用网页控制需要使用https";
    $("pair-link").setAttribute("aria-disabled", "true");
  }
  $("qr").src = data.qr;
  $("qr").alt = "扫描二维码配对";
  $("qr").hidden = false;
  $("copy-code").disabled = false;
  $("pair-result").className = "pair-result";
  $("pair-result").hidden = false;
  updatePairingExpiry();
  if (pairingExpiresAt) pairingTimer = setInterval(updatePairingExpiry, 1000);
  notice("配对码已生成，可在另一台设备上完成配对。");
}));
function updatePairingExpiry() {
  if (!pairingExpiresAt) return;
  const remaining = Math.max(0, pairingExpiresAt - Date.now());
  if (remaining > 0) {
    const seconds = Math.ceil(remaining / 1000);
    $("expires").textContent = `剩余 ${Math.floor(seconds / 60)}:${String(seconds % 60).padStart(2, "0")} · 有效至 ${new Date(pairingExpiresAt).toLocaleTimeString()}`;
    return;
  }
  if (pairingTimer !== null) clearInterval(pairingTimer);
  pairingTimer = null;
  pairingExpiresAt = 0;
  $("expires").textContent = "配对码已过期，请重新生成。";
  $("copy-code").disabled = true;
  $("pair-link").href = "";
  $("pair-link").textContent = "配对链接已失效";
  $("pair-link").setAttribute("aria-disabled", "true");
  $("qr").src = "";
  $("qr").alt = "配对码已过期，二维码已失效";
  $("qr").hidden = true;
  $("pair-result").className = "pair-result expired";
}
function clearPairing() {
  if (pairingTimer !== null) clearInterval(pairingTimer);
  pairingTimer = null;
  pairingExpiresAt = 0;
  $("pair-result").hidden = true;
  $("pair-result").className = "pair-result";
  $("pair-code").textContent = "";
  $("expires").textContent = "";
  $("pair-link").href = "";
  $("qr").src = "";
  $("qr").hidden = true;
  $("copy-code").disabled = true;
}
async function copyPairCode(element) {
  if (!pairingExpiresAt || pairingExpiresAt <= Date.now()) {
    updatePairingExpiry();
    throw new Error("配对码已过期，请重新生成。");
  }
  if (navigator.clipboard) {
    await navigator.clipboard.writeText(element.textContent);
    notice("配对码已复制。");
    return;
  }
  const selection = window.getSelection();
  const range = document.createRange();
  range.selectNodeContents(element);
  selection.removeAllRanges();
  selection.addRange(range);
  notice("已选中配对码，请手动复制。");
}
$("copy-code").addEventListener("click", () => action($("copy-code"), () => copyPairCode($("pair-code"))));
$("pair-link").addEventListener("click", event => { if (!pairingExpiresAt) event.preventDefault(); });
$("refresh").addEventListener("click", () => action($("refresh"), async () => { await devices(); notice(""); }));
$("clear-key")?.addEventListener("change", event => {
  const clear = event.currentTarget.checked;
  if (clear) $("key").value = "";
  $("key").disabled = clear;
  $("toggle-key").disabled = clear;
});
$("toggle-key").addEventListener("click", () => {
  const show = $("key").type === "password";
  $("key").type = show ? "text" : "password";
  $("toggle-key").textContent = show ? "隐藏" : "显示";
  $("toggle-key").setAttribute("aria-label", show ? "隐藏连接授权密钥" : "显示连接授权密钥");
});
async function initializeSettings(data) {
  apply(data);
  $("login-form").hidden = true;
  $("settings-content").hidden = false;
  window.settingsNavigation.init(data);
  if (window.initOneBotPanel) window.initOneBotPanel(data);
  notice("");
  const wxCard = $("wx-card");
  const showWx = data.wxSupported === true;
  if (wxCard && showWx) {
    bindWxSettings();
    if (!window.refreshWxPanel) refreshWxAccounts().catch(error => { $("wx-status").textContent = error.message; });
  }
  if (data.server) {
    try { await devices(); } catch (error) { notice(error.message, true); }
  }
}
function showLogin() {
  if (token && token === readSettingsSession()) {
    try { localStorage.removeItem(settingsSessionKey); } catch { /* Storage is optional. */ }
  }
  token = "";
  saved = null;
  $("settings-content").hidden = true;
  window.settingsNavigation.lock();
  clearPairing();
  $("devices").replaceChildren();
  const wxConfig = $("wx-config");
  if (wxConfig) wxConfig.hidden = true;
  $("login-form").hidden = false;
  $("status").textContent = "等待登录";
  $("status").className = "status";
}
$("login-form").addEventListener("submit", (event) => {
  event.preventDefault();
  action($("login"), async () => {
    const password = $("settings-password").value.trim();
    $("settings-password").value = "";
    try {
      if (sessionLogin) {
        const login = await api("/api/settings/login", { method: "POST", headers: { Authorization: `Bearer ${password}` } });
        token = login.sessionToken;
        try { localStorage.setItem(settingsSessionKey, token); } catch { /* Storage is optional. */ }
      } else {
        token = password;
      }
      await initializeSettings(await api("/api/settings"));
    } catch (error) {
      showLogin();
      $("settings-password").focus();
      throw error;
    }
  });
});
async function refreshSettings() {
  if (!$("login-form").hidden || busy) return;
  try {
    const data = await api("/api/settings");
    if (!saved) await initializeSettings(data);
    else status(data);
  } catch (error) {
    const hadToken = !!token;
    if (error.passwordRequired) {
      sessionLogin = error.sessionLogin;
      showLogin();
    }
    else $("status").textContent = "Agent 已退出或页面已失效";
    if (!error.passwordRequired || hadToken) notice(error.message, true);
  }
}
let wxAccounts = [];
let wxFriend = "";
let wxCommands = [];
let wxRevision = 0;
function selectedWxAccount() {
  return wxAccounts.find(row => row.id === $("wx-account").value && row.loggedIn);
}
function wxAPI(route, account, input, friend) {
  if (!account) throw new Error("请选择已登录的微信账号");
  const params = new URLSearchParams({ id: account.id, wxid: account.wxid });
  if (friend) params.set("friend", friend);
  return api(`/api/wx/${route}?${params}`, input === undefined ? {} : { method: "POST", body: JSON.stringify(input) });
}
async function refreshWxAccounts() {
  const old = selectedWxAccount();
  const rows = await api("/api/wx/accounts");
  wxAccounts = rows;
  const select = $("wx-account");
  select.replaceChildren(new Option(rows.length ? "请选择账号" : "未发现本机微信机器人", ""));
  for (const row of rows) {
    const identity = [row.name, row.wxid].filter(Boolean).join(" · ") || row.id;
    const option = new Option(`${identity}（${row.loggedIn ? "已登录" : "未登录"}）`, row.id);
    option.disabled = !row.loggedIn;
    select.add(option);
  }
  select.disabled = !rows.some(row => row.loggedIn);
  if (old && rows.some(row => row.id === old.id && row.wxid === old.wxid && row.loggedIn)) select.value = old.id;
  else select.value = rows.find(row => row.loggedIn)?.id || "";
  window.settingsNavigation.syncWxScope();
  await loadWxSettings();
}
async function loadWxSettings() {
  const revision = ++wxRevision;
  const account = selectedWxAccount();
  $("wx-config").hidden = true;
  $("wx-friend-form").hidden = true;
  $("wx-friend").value = "";
  $("wx-command-form").reset();
  wxFriend = "";
  $("wx-status").textContent = account ? "正在读取配置…" : "";
  if (!account) return;
  try {
    const [settings, commands] = await Promise.all([wxAPI("settings", account), wxAPI("commands", account)]);
    if (revision !== wxRevision) return;
    $("wx-enabled").checked = settings.enabled;
    $("wx-workspace").value = settings.workspace_alias || "";
    $("wx-disconnect").hidden = !settings.paired;
    $("wx-status").textContent = settings.agent_online && settings.enabled ? "Agent 在线" : settings.enabled ? "已启用，等待 Agent 连接" : "接入已停用";
    renderWxCommands(commands || [], account);
    $("wx-config").hidden = false;
  } catch (error) {
    if (revision === wxRevision) $("wx-status").textContent = error.message;
  }
}
function renderWxCommands(commands, account) {
  wxCommands = commands;
  const container = $("wx-commands");
  container.replaceChildren();
  for (const command of commands) {
    const row = document.createElement("div");
    row.className = "wx-command";
    const label = document.createElement("label");
    label.className = "inline-field";
    const enabled = document.createElement("input");
    enabled.type = "checkbox";
    enabled.checked = command.enabled;
    label.append(enabled, `/${command.name} · ${command.status}`);
    const note = document.createElement("p");
    note.className = "hint";
    note.textContent = command.help || "";
    const users = document.createElement("input");
    users.value = (command.allowed_wechat_ids || []).join(", ");
    users.placeholder = "可调用的好友 WxID，以逗号分隔";
    users.setAttribute("aria-label", `${command.name} 可调用的好友`);
    const save = document.createElement("button");
    save.type = "button";
    save.className = "secondary";
    save.textContent = "保存可调用好友";
    enabled.addEventListener("change", async () => {
      const desired = enabled.checked;
      enabled.disabled = true;
      try { await wxAPI("command-enabled", account, { name: command.name, enabled: desired }); }
      catch (error) { enabled.checked = !desired; notice(error.message, true); }
      finally { enabled.disabled = false; }
    });
    save.addEventListener("click", () => action(save, async () => {
      await wxAPI("command-users", account, { name: command.name, wechat_ids: users.value.split(/[\s,;]+/).filter(Boolean) });
      notice("已保存命令权限。");
    }));
    row.append(label, note, users, save);
    container.append(row);
  }
}
let wxSettingsBound = false;
function bindWxSettings() {
  if (wxSettingsBound) return;
  wxSettingsBound = true;
  $("wx-account").addEventListener("change", loadWxSettings);
  $("wx-refresh").addEventListener("click", () => action($("wx-refresh"), () => window.refreshWxPanel ? window.refreshWxPanel() : refreshWxAccounts()));
  $("wx-form").addEventListener("submit", event => {
    event.preventDefault();
    action($("wx-save"), async () => {
      await wxAPI("settings", selectedWxAccount(), { enabled: $("wx-enabled").checked, workspace_alias: $("wx-workspace").value.trim() });
      await loadWxSettings();
      notice("已保存微信机器人设置。");
    });
  });
  $("wx-disconnect").addEventListener("click", () => {
    if (!confirm("解除这个微信机器人的 Agent 授权？")) return;
    action($("wx-disconnect"), async () => {
      await wxAPI("disconnect", selectedWxAccount(), {});
      await loadWxSettings();
    });
  });
  $("wx-friend").addEventListener("input", () => { $("wx-friend-form").hidden = true; wxFriend = ""; });
  $("wx-read-friend").addEventListener("click", () => action($("wx-read-friend"), async () => {
    const account = selectedWxAccount();
    const friend = $("wx-friend").value.trim();
    if (!friend) throw new Error("请输入好友 WxID");
    const access = await wxAPI("access", account, undefined, friend);
    if (account !== selectedWxAccount() || friend !== $("wx-friend").value.trim()) return;
    wxFriend = friend;
    $("wx-friend-enabled").checked = access.enabled;
    $("wx-friend-mode").value = ["tools", "admin"].includes(access.mode) ? access.mode : "text";
    $("wx-friend-commands").replaceChildren(...wxCommands.map(command => new Option(command.name, command.name, false, (access.commands || []).includes(command.name))));
    if (window.showWXProfile) await window.showWXProfile("friend", access, () => account === selectedWxAccount() && wxFriend === friend);
    if (account === selectedWxAccount() && wxFriend === friend) $("wx-friend-form").hidden = false;
  }));
  $("wx-friend-form").addEventListener("submit", event => {
    event.preventDefault();
    action($("wx-save-friend"), async () => {
      if (!wxFriend) throw new Error("请先读取好友权限");
      await wxAPI("access", selectedWxAccount(), { wechat_id: wxFriend, enabled: $("wx-friend-enabled").checked, mode: $("wx-friend-mode").value, ...(window.readWXProfile?.("friend") || {}), commands: Array.from($("wx-friend-commands").selectedOptions, option => option.value) });
      notice("已保存好友权限。");
    });
  });
  $("wx-command-form").addEventListener("submit", event => {
    event.preventDefault();
    action($("wx-register-command"), async () => {
      const account = selectedWxAccount();
      const fields = ["name", "url", "method", "help", "params", "response-field", "header-name", "header-value"];
      const command = {};
      for (const field of fields) command[field.replaceAll("-", "_")] = $(`wx-command-${field}`).value.trim();
      command.web_url = command.url;
      command.web_method = command.method;
      delete command.url;
      delete command.method;
      command.timeout = Number($("wx-command-timeout").value);
      await wxAPI("commands", account, command);
      if (account === selectedWxAccount()) await loadWxSettings();
      notice("已注册 Web 查询命令。");
    });
  });
}
refreshSettings();
setInterval(refreshSettings, 5000);
