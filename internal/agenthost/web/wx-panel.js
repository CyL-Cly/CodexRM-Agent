(() => {
  let wxProbe = null;
  let wxTaskTimer = 0;
  let wxPanelBound = false;
  let wxContactPage = 1;
  let wxContactRevision = 0;
  let wxContactSelection = null;
  let wxGroup = null;
  let groupNoticeEditor;
  let defaultNoticeEditor;
  let wxContactAccount = "";
  let scan = null;
  let scanTimer = 0;
  let robots = [];
  let workspaces = [];
  let workspacesLoad;

  const prevInit = window.initializeSettings;
  window.initializeSettings = async function (data) {
    await prevInit(data);
    if (data.wxSupported !== true) return;
    bindWxPanel();
    refreshWxPanel().catch((error) => {
      const status = document.getElementById("wx-probe-status");
      if (status) status.textContent = error.message;
    });
  };

  function bindWxPanel() {
    if (wxPanelBound) return;
    wxPanelBound = true;
    for (const kind of ["group", "friend"]) buildProfile(kind);
    document.addEventListener("settings:view", event => {
      if (wxProbe?.ready) refreshWxView(event.detail);
    });
    $("wx-maintenance-action").addEventListener("change", () => { $("wx-backup-picker").hidden = $("wx-maintenance-action").value !== "restore"; });
    $("wx-maintenance-form").addEventListener("submit", event => {
      event.preventDefault();
      const operation = $("wx-maintenance-action");
      const confirmed = ["restore", "uninstall", "upgrade", "upgrade-adopted-clients"].includes(operation.value);
      if (confirmed && !window.confirm("确认" + operation.selectedOptions[0].textContent + "？")) return;
      let confirmText = "";
      if (operation.value === "purge") {
        confirmText = window.prompt("将删除当前安装的账号、登录状态和数据库。请输入「清除微信机器人数据」进行确认。");
        if (confirmText !== "清除微信机器人数据") return;
      }
      action($("wx-maintenance-run"), async () => {
        await api("/api/wx/maintenance", {method:"POST",body:JSON.stringify({action:operation.value,backup:$("wx-backup-file").value,confirm:confirmed,confirmText})});
        await refreshWxPanel();
      });
    });
    $("wx-cache-form").addEventListener("submit", event => {
      event.preventDefault();
      action($("wx-cache-save"), async () => renderCache(await api("/api/wx/cache", { method:"POST", body:JSON.stringify({clean:true,days:Number($("wx-cache-days").value)}) })));
    });
    $("wx-robot").addEventListener("change", () => { resetScan(); renderRobot(); });
    $("wx-robot-create-form").addEventListener("submit", event => {
      event.preventDefault();
      action($("wx-robot-create"), async () => {
        const proxy = {
          ProxyIp: $("wx-robot-proxy-address").value.trim(),
          ProxyUser: $("wx-robot-proxy-user").value.trim(),
          ProxyPassword: $("wx-robot-proxy-password").value
        };
        const result = await robotRequest("create", {
          name: $("wx-robot-name").value.trim(),
          version: $("wx-robot-version").value,
          proxy
        });
        $("wx-robot-create-form").reset();
        await loadRobots(String(result.id));
        if (!result.created) notice("机器人已登记，容器初始化未完成。", true);
      });
    });
    $("wx-robot-login").addEventListener("click", () => action($("wx-robot-login"), beginScan));
    $("wx-login-type").addEventListener("change", () => {
      const ipad = $("wx-login-type").value === "ipad";
      $("wx-login-pretender-row").hidden = ipad;
      if (ipad) $("wx-login-pretender").checked = false;
    });
    $("wx-robot-activate").addEventListener("click", () => action($("wx-robot-activate"), () => activateRobot(Number($("wx-robot").value))));
    $("wx-robot-logout").addEventListener("click", () => action($("wx-robot-logout"), async () => {
      if (!window.confirm("退出此机器人使用的微信账号？")) return;
      await robotRequest("logout", { id: Number($("wx-robot").value) });
      resetScan();
      await refreshWxPanel();
    }));
    $("wx-robot-export").addEventListener("click", () => action($("wx-robot-export"), async () => {
      if (!window.confirm("设备登录数据包含敏感信息。确认下载？")) return;
      const id = Number($("wx-robot").value);
      const headers = new Headers({"X-CodexRM-Confirm": "export-login"});
      if (token) headers.set("Authorization", "Bearer " + token);
      const response = await fetch("/api/wx/robot/export-login?id=" + id, {method:"POST", headers, cache:"no-store"});
      if (!response.ok) throw new Error((await response.json()).error || "导出失败");
      const download = document.createElement("a");
      download.href = URL.createObjectURL(await response.blob());
      download.download = "wxbot-login-" + id + ".txt";
      download.click();
      setTimeout(() => URL.revokeObjectURL(download.href), 30000);
    }));
    $("wx-test-message-form").addEventListener("submit", event => {
      event.preventDefault();
      const target = readTestTarget();
      const body = $("wx-test-text").value.trim();
      if (!target || !body || !window.confirm("将向「" + target.contact + "」发送测试消息，确认发送？")) return;
      action($("wx-test-send"), async () => {
        await contactOperation("post-message--send--text", target.robot, {}, {id:Number(target.robot), to_wxid:target.contact, content:body});
        if ($("wx-robot").value === target.robot) $("wx-test-result").textContent = "微信协议已接受测试消息。";
      });
    });
    $("wx-test-receive").addEventListener("click", () => {
      const target = readTestTarget();
      if (!target) return;
      action($("wx-test-receive"), async () => {
        const history = await contactOperation("get-chat--history", target.robot, {contact_id:target.contact,page_index:"1",page_size:"10"});
        if ($("wx-robot").value !== target.robot || $("wx-test-target").value.trim() !== target.contact) return;
        const entries = history?.items || history?.list || (Array.isArray(history) ? history : []);
        const received = $("wx-test-received");
        received.textContent = entries.map(entry => (entry.is_send ? "发出" : "收到") + " · " + (entry.content || "[非文本消息]")).join("\n");
        received.hidden = entries.length === 0;
        $("wx-test-result").textContent = entries.length ? "已读取最近的聊天记录。" : "暂时没有聊天记录。";
      });
    });
    $("wx-open-slider").addEventListener("click", () => action($("wx-open-slider"), openSlider));
    $("wx-auto-slider").addEventListener("click", () => action($("wx-auto-slider"), async () => {
      const current = scan;
      if (!current?.needsCode) return;
      await robotRequest("login-slider-auto", { id: current.id, scanId: current.scanId });
      if (scan !== current) return;
      current.needsCode = false;
      resetSlider();
      $("wx-scan-code-form").hidden = true;
      $("wx-scan-status").textContent = "验证已提交，正在确认登录结果。";
      scheduleScan(current);
    }));
    $("wx-slider-continue").addEventListener("click", () => {
      if (!scan || $("wx-slider-continue").disabled) return;
      scan.needsCode = false;
      $("wx-scan-status").textContent = "正在确认微信登录…";
      $("wx-scan-code-form").hidden = true;
      resetSlider();
      scheduleScan(scan);
    });
    window.addEventListener("message", event => {
      const frame = $("wx-slider-frame");
      if (!scan?.needsCode || $("wx-scan-slider").hidden || event.source !== frame.contentWindow || event.origin !== "null" || event.data?.type !== "sliderResult") return;
      const verified = event.data.data?.success === true;
      $("wx-slider-continue").disabled = !verified;
      $("wx-scan-status").textContent = verified ? "滑块验证通过。请在微信确认登录，再点击下方按钮。" : "滑块验证未通过，请重试或提交微信显示的验证码。";
    });
    $("wx-scan-code-form").addEventListener("submit", event => {
      event.preventDefault();
      const current = scan;
      if (!current || !current.needsCode) return;
      action($("wx-submit-scan-code"), async () => {
        await robotRequest("login-code", { id: current.id, scanId: current.scanId, code: $("wx-scan-code").value });
        if (scan !== current) return;
        current.needsCode = false;
        $("wx-scan-code-form").reset();
        $("wx-scan-code-form").hidden = true;
        resetSlider();
        scheduleScan(current);
      });
    });
    $("wx-install").addEventListener("click", () => action($("wx-install"), async () => {
      const optionalServices = Array.from($("wx-optional-services").querySelectorAll("input:checked"), input => input.value);
      await api("/api/wx/install", { method: "POST", body: JSON.stringify({ optionalServices }) });
      await refreshWxPanel();
    }));
    $("wx-admin-oauth-qr").addEventListener("click", () => action($("wx-admin-oauth-qr"), async () => {
      const data = await api("/api/wx/manage/get-wx-admin-oauth-qr", { method: "POST" });
      $("wx-admin-oauth-image").src = data.image;
      $("wx-admin-oauth-image").hidden = false;
    }));
    $("wx-takeover").addEventListener("click", () => action($("wx-takeover"), async () => {
      const token = $("wx-admin-token").value.trim();
      const oauthCode = $("wx-admin-oauth").open ? $("wx-admin-oauth-code").value.trim() : "";
      if (!token && !oauthCode) throw new Error("请输入后台管理凭据或公众号授权码");
      await api("/api/wx/takeover", { method: "POST", body: JSON.stringify({ token, oauthCode, composeDir: wxProbe?.existing?.composeDir || "" }) });
      $("wx-admin-token").value = "";
      $("wx-admin-oauth-code").value = "";
      await refreshWxPanel();
    }));
    $("wx-task-retry").addEventListener("click", () => action($("wx-task-retry"), async () => {
      await api("/api/wx/task", { method: "POST", body: JSON.stringify({ retry: true }) });
      await refreshWxPanel();
    }));
    for (const [id, offset] of [["wx-contacts-prev", -1], ["wx-contacts-next", 1]]) {
      $(id).addEventListener("click", () => loadWxContacts(wxContactPage + offset));
    }
    $("wx-group-members-sync").addEventListener("click", () => action($("wx-group-members-sync"), () => loadGroupMembers(wxGroup, true)));
    for (const [button, field] of [["wx-group-member-add", "wx-group-members"],["wx-group-admin-add","wx-group-admins"]]) {
      $(button).addEventListener("click", () => {
        const id = $("wx-group-member-picker").value;
        if (!id) return;
        $(field).value = Array.from(new Set([...$(field).value.split(/[\s,;，；]+/).filter(Boolean), id])).join("\n");
      });
    }
    $("wx-group-form").addEventListener("submit", (event) => {
      event.preventDefault();
      const group = wxGroup;
      const account = selectedWxAccount();
      if (!group || account?.id !== group.accountId || account?.wxid !== group.wxid) return;
      const list = id => $(id).value.split(/[\s,;，；]+/).filter(Boolean);
      action($("wx-group-save"), async () => {
        await wxAPI("group", account, { groupId: group.id, enabled: $("wx-group-enabled").checked,
          trigger: $("wx-group-trigger").value, session: $("wx-group-session").value,
          mode: $("wx-group-mode").value, ...window.readWXProfile("group"),
          commands: Array.from($("wx-group-commands").selectedOptions, option => option.value),
          members: list("wx-group-members"), admins: list("wx-group-admins") });
        notice("群设置已保存。");
        await loadWxContacts(wxContactPage);
      });
    });
    groupNoticeEditor = createNoticeEditor($("wx-group-notice-editor"), () => {
      if (!wxGroup || !isCurrentContact(wxGroup)) return null;
      return {id: wxGroup.accountId, wxid: wxGroup.wxid, group: wxGroup.id};
    });
    defaultNoticeEditor = createNoticeEditor($("wx-default-notice-editor"), () => {
      const account = selectedWxAccount();
      return account ? {id: account.id, wxid: account.wxid, scope: "global"} : null;
    });
    $("wx-default-notices").addEventListener("toggle", () => {
      if ($("wx-default-notices").open) defaultNoticeEditor.load();
    });
    $("wx-contact-search").addEventListener("click", () => loadWxContacts(1));
    $("wx-contact-kind").addEventListener("change", () => loadWxContacts(1));
    $("wx-friend").addEventListener("input", () => {
      if (wxGroup) return;
      wxContactSelection = null;
      updateContactSelection();
      contactDetailStatus();
    });
    $("wx-contact-keyword").addEventListener("keydown", (event) => {
      if (event.key === "Enter") {
        event.preventDefault();
        $("wx-contact-search").click();
      }
    });
    $("wx-contact-sync").addEventListener("click", () => action($("wx-contact-sync"), async () => {
      const account = selectedWxAccount();
      await wxAPI("contacts/sync", account, {});
      await loadWxContacts(1);
      notice("已同步通讯录。");
    }));
    $("wx-account").addEventListener("change", () => { loadWxContacts(1).catch((error) => notice(error.message, true)); });
  }

  window.refreshWxPanel = async function refreshWxPanel() {
    wxProbe = await api("/api/wx/probe");
    renderWxProbe(wxProbe);
    const task = wxProbe.task;
    const active = task && ["pending", "running", "needs_privilege"].includes(task.status);
    if (active && !wxTaskTimer) wxTaskTimer = setInterval(() => {
      refreshWxPanel().catch(() => {});
    }, 2000);
    if (!active && wxTaskTimer) {
      clearInterval(wxTaskTimer);
      wxTaskTimer = 0;
    }
    if (wxProbe.ready) {
      await Promise.all([refreshWxAccounts(), loadRobots()]);
      if (selectedWxAccount()) await loadWxContacts(wxContactPage);
      refreshWxView(window.settingsNavigation.view);
      document.dispatchEvent(new Event("wx:ready"));
    }
  }

  function refreshWxView(view) {
    if (view === "wx-maintenance") {
      loadMaintenance().catch(error => { $("wx-maintenance-state").textContent = error.message; });
    }
    if (view === "wx-cache") {
      api("/api/wx/cache").then(renderCache).catch(error => { $("wx-cache-usage").textContent = error.message; });
    }
  }

  async function loadMaintenance() {
    const state = await api("/api/wx/maintenance");
    const operation = $("wx-maintenance-action");
    const lifecycle = ["start", "stop", "restart"];
    for (const option of operation.options) {
      const available = option.value === "upgrade-adopted-clients" ? state.adopted : state.managed || (state.adopted && lifecycle.includes(option.value));
      option.hidden = !available;
      option.disabled = !available;
    }
    if (operation.selectedOptions[0]?.disabled) operation.value = "start";
    $("wx-maintenance-form").hidden = !(state.managed || state.adopted);
    $("wx-backup-picker").hidden = operation.value !== "restore";
    $("wx-backup-file").replaceChildren(...state.backups.map(name => new Option(name, name)));
    $("wx-maintenance-state").textContent = state.managed
      ? "备份、恢复与升级期间会暂停机器人。"
      : state.adopted ? "可启停、重启或升级机器人客户端。公共服务升级、备份、恢复和卸载使用原部署工具。" : "请先安装或接管 wxbot。";
  }

  function renderCache(data) {
    $("wx-cache-days").value = data.days;
    $("wx-cache-usage").textContent = data.count + " 份记录 · " + (data.bytes / 1048576).toFixed(1) + " MiB" + (data.removed ? " · 已清理 " + data.removed + " 份" : "");
    const states = {saved: "已保存原件", thumbnail: "仅有缩略图", unavailable: "协议未提供或原件已过期", download_failed: "下载失败", limit: "超过本次附件限额"};
    const records = (data.recent || []).map(record => {
      const box = document.createElement("details");
      const summary = document.createElement("summary");
      const media = record.media || [];
      summary.textContent = new Date(record.updatedAt).toLocaleString() + " · " + media.length + " 个媒体条目";
      const list = document.createElement("ul");
      for (const entry of media) {
        const row = document.createElement("li");
        const label = [entry.entry, entry.name, states[entry.status] || entry.status, entry.size ? entry.size + " 字节" : "", entry.path].filter(Boolean);
        row.textContent = label.join(" · ");
        if (entry.sha256) row.title = "SHA-256 " + entry.sha256;
        list.append(row);
      }
      box.append(summary, list);
      return box;
    });
    $("wx-cache-records").replaceChildren(...records);
  }

  function buildProfile(kind) {
    const container = $("wx-" + kind + "-profile");
    for (const [key,label,tag] of [["workspace","工作区","select"],["model","模型","input"],["prompt","提示词","textarea"]]) {
      const title = document.createElement("label");
      const field = document.createElement(tag);
      field.id = "wx-" + kind + "-" + key;
      title.htmlFor = field.id;
      title.textContent = label;
      if (key === "model") { field.placeholder = "留空使用 Agent 默认模型"; field.maxLength = 128; }
      if (key === "prompt") { field.rows = 3; field.maxLength = 1000; }
      container.append(title,field);
    }
  }
  window.showWXProfile = async function(kind, profile, stillSelected) {
    if (!workspacesLoad) workspacesLoad = api("/api/wx/workspaces").then(data => { workspaces = data.workspaces || []; }).catch(() => { workspacesLoad = undefined; });
    await workspacesLoad;
    if (!stillSelected()) return;
    const workspace = $("wx-" + kind + "-workspace");
    workspace.replaceChildren(new Option("Agent 默认工作区", ""), ...workspaces.map(row => new Option(row.name, row.path)));
    if (profile.workspace && !workspaces.some(row => row.path === profile.workspace)) workspace.add(new Option(profile.workspace, profile.workspace));
    workspace.value = profile.workspace || "";
    $("wx-" + kind + "-model").value = profile.model || "";
    $("wx-" + kind + "-prompt").value = profile.prompt || "";
  };
  window.readWXProfile = kind => Object.fromEntries(["workspace","model","prompt"].map(key => [key,$("wx-" + kind + "-" + key).value.trim()]));

  function readTestTarget() {
    const robot = $("wx-robot").value;
    const contact = $("wx-test-target").value.trim();
    if (!robot || !contact || /[@\s\x00-\x1f]/.test(contact)) {
      notice("请选择机器人并填写测试好友的 WxID。", true);
      return null;
    }
    return { robot, contact };
  }

  function contactOperation(operation, robot, query, body = {}) {
    const search = new URLSearchParams({robot, ...query});
    return api("/api/wx/manage/" + operation + "?" + search, {method:"POST",body:JSON.stringify(body)});
  }

  function robotRequest(action, payload) {
    return api(`/api/wx/robot/${action}`, payload === undefined ? {} : { method: "POST", body: JSON.stringify(payload) });
  }

  async function loadRobots(preferred) {
    const select = $("wx-robot");
    const previous = preferred || select.value;
    try {
      robots = await robotRequest("list");
      select.replaceChildren(...robots.map(row => new Option(row.robot_name || row.robot_code, String(row.id))));
      if (robots.some(row => String(row.id) === previous)) select.value = previous;
      renderRobot();
      document.dispatchEvent(new Event("wx:robots"));
    } catch (error) {
      $("wx-robot-state").textContent = error.message;
    }
  }

  function renderRobot() {
    const current = robots.find(row => String(row.id) === $("wx-robot").value);
    const existing = wxProbe?.existing;
    const account = existing?.accounts?.find(row => row.id === "client_" + current?.robot_code);
    const protocol = existing?.services?.find(row => row.name === "server_" + current?.robot_code);
    const admin = existing?.services?.find(row => row.name === "wechat-robot-admin-backend");
    const loggedIn = account?.loggedIn ?? (current?.status === "online");
    const stages = current ? [
      current.nickname || current.wechat_id || current.robot_name,
      admin?.running ? "管理服务运行中" : "管理服务状态未知",
      protocol?.running ? "协议端运行中" : "协议端未就绪",
      loggedIn ? "微信在线" : "等待扫码",
      account?.capabilities?.codexOnly ? "仅 Codex 回复" : "需升级客户端才能启用仅 Codex 回复",
      account?.capabilities?.groupChat && account?.capabilities?.fileContext ? "支持群聊与文件" : "群聊与文件能力未就绪"
    ] : ["请先创建机器人。"];
    $("wx-robot-state").textContent = stages.join(" · ");
    $("wx-robot-login").disabled = !current;
    $("wx-robot-activate").disabled = !loggedIn;
    $("wx-robot-logout").disabled = !loggedIn;
    $("wx-robot-export").disabled = !current;
    $("wx-test-message").hidden = !loggedIn;
    if ($("wx-test-message").dataset.robot !== String(current?.id || "")) {
      $("wx-test-message").dataset.robot = String(current?.id || "");
      $("wx-test-message-form").reset();
      $("wx-test-received").hidden = true;
      $("wx-test-received").textContent = "";
      $("wx-test-result").textContent = "";
    }
  }

  function resetSlider() {
    $("wx-open-slider").hidden = true;
    $("wx-auto-slider").hidden = true;
    $("wx-scan-slider").hidden = true;
    $("wx-slider-frame").removeAttribute("src");
    $("wx-slider-continue").disabled = true;
  }

  async function openSlider() {
    const current = scan;
    if (!current?.needsCode) return;
    const result = await robotRequest("login-slider", { id: current.id, scanId: current.scanId });
    if (scan !== current || !current.needsCode) return;
    if (!/^\/wx-slider\/[A-Za-z0-9_-]{43}$/.test(result.path)) throw new Error("滑块页面地址无效");
    $("wx-slider-continue").disabled = true;
    $("wx-slider-frame").src = result.path;
    $("wx-scan-slider").hidden = false;
    $("wx-scan-status").textContent = "请完成微信滑块验证。";
  }

  function resetScan() {
    clearTimeout(scanTimer);
    scan = null;
    $("wx-scan").hidden = true;
    $("wx-scan-image").removeAttribute("src");
    $("wx-scan-code-form").hidden = true;
    $("wx-scan-code-form").reset();
    resetSlider();
  }

  async function beginScan() {
    resetScan();
    const id = Number($("wx-robot").value);
    const result = await robotRequest("login", {
      id, loginType: $("wx-login-type").value, isPretender: $("wx-login-pretender").checked
    });
    if (Number($("wx-robot").value) !== id) return;
    if (result.loggedIn) { await activateRobot(id); return; }
    scan = { id, scanId: result.scanId, expires: Date.now() + 300000 };
    $("wx-scan").hidden = false;
    $("wx-scan-image").src = result.qr;
    $("wx-scan-status").textContent = "请使用微信扫描并确认登录。";
    scheduleScan(scan);
  }

  function scheduleScan(current) {
    scanTimer = setTimeout(() => checkScan(current), 3000);
  }

  async function checkScan(current) {
    try {
      const result = await robotRequest("login-check", { id: current.id, scanId: current.scanId });
      if (scan !== current) return;
      if (result.loggedIn) {
        resetScan();
        await activateRobot(current.id);
      } else if (result.needsCode) {
        current.needsCode = true;
        $("wx-scan-code-form").hidden = false;
        $("wx-open-slider").hidden = false;
        $("wx-auto-slider").hidden = false;
        $("wx-scan-status").textContent = "请选择微信提示的验证方式。";
      } else if (result.expired || Date.now() >= current.expires) {
        $("wx-scan-status").textContent = "二维码已过期，请重新获取。";
      } else scheduleScan(current);
    } catch (error) {
      if (scan === current && Date.now() < current.expires) scheduleScan(current);
      else notice(error.message, true);
    }
  }

  async function activateRobot(id) {
    $("wx-robot-state").textContent = "正在同步通讯录并配对 CodexRM…";
    const result = await robotRequest("activate", { id });
    await refreshWxPanel();
    if (wxAccounts.some(row => row.id === result.id && row.wxid === result.wxid)) {
      $("wx-account").value = result.id;
      await Promise.all([loadWxSettings(), loadWxContacts(1)]);
    }
    const failures = [result.syncError, result.pairError].filter(Boolean);
    notice(failures.length ? failures.join("。") : "微信已登录，已同步通讯录并接入 CodexRM。", failures.length > 0);
  }

  function renderWxProbe(probe) {
    const setup = $("wx-setup");
    const manage = $("wx-manage");
    const existing = $("wx-existing");
    const helper = $("wx-helper-cmd");
    const taskBox = $("wx-task");
    const token = $("wx-admin-token");
    const tokenRow = $("wx-token-row");
    const oauth = $("wx-admin-oauth");
    const install = $("wx-install");
    const takeover = $("wx-takeover");
    const status = $("wx-probe-status");
    if (!probe.osSupported) {
      status.textContent = "首批安装仅支持 Debian、Ubuntu 的 amd64 主机。";
      setup.hidden = false;
      manage.hidden = true;
      install.hidden = true;
      takeover.hidden = true;
      return;
    }
    const task = probe.task;
    const busy = task && ["pending", "running", "needs_privilege", "failed"].includes(task.status);
    const failed = task && (task.status === "failed" || task.status === "needs_privilege");
    setup.hidden = !!probe.ready && !busy;
    manage.hidden = !probe.ready;
    window.settingsNavigation.wxReady(!!probe.ready);
    const accounts = probe.existing?.accounts || [];
    const loggedIn = accounts.filter(row => row.loggedIn).length;
    const services = (probe.existing?.services || []).filter(row => !/^(client|server)_/.test(row.name));
    $("overview-wx-state").textContent = probe.ready ? "已接入" : probe.existing?.found ? "待接管" : "未安装";
    $("overview-wx-accounts").textContent = `${loggedIn} / ${accounts.length} 个账号已登录`;
    $("wx-service-summary").hidden = $("wx-overview-links").hidden = !probe.ready;
    $("wx-services-count").textContent = services.length ? `${services.filter(row => row.running).length} / ${services.length} 运行中` : "未知";
    $("wx-logged-in-count").textContent = `${loggedIn} / ${accounts.length}`;
    $("wx-install-mode").textContent = probe.instance?.mode === "taken-over" ? "已接管" : "已安装";
    if (probe.ready && !failed) {
      const caps = (probe.existing?.accounts || []).filter((row) => row.loggedIn);
      const extra = caps.find((row) => row.capabilities?.groupChat || row.capabilities?.fileContext);
      status.textContent = extra ? "微信已登录，当前客户端支持群聊和文件上下文。" : "微信机器人已就绪。";
    } else {
      const docker = probe.docker?.usable ? probe.docker.version : probe.docker?.error || "未安装";
      const compose = probe.compose?.usable ? probe.compose.version : "未就绪";
      const disk = probe.diskFreeBytes ? `${Math.floor(probe.diskFreeBytes / (1024 ** 3))} GB 可用` : "磁盘空间未知";
      const ports = (probe.ports || []).filter(port => port.inUse).map(port => port.port);
      const occupied = ports.length > 0 && !probe.existing?.found ? ` · 端口 ${ports.join("、")} 已占用` : "";
      status.textContent = `${probe.osId || probe.os} ${probe.osVersion || ""} ${probe.arch} · Docker ${docker} · Compose ${compose} · ${disk}${occupied}`.replaceAll("  ", " ");
    }
    if (probe.existing?.found && !probe.ready) {
      existing.hidden = false;
      const accounts = (probe.existing.accounts || []).map((row) => {
        const name = [row.name, row.wxid].filter(Boolean).join(" · ") || row.id;
        const caps = row.capabilities?.codexrm ? "支持 CodexRM" : "客户端待升级";
        return `${name}（${row.loggedIn ? "已登录" : "未登录"}，${caps}）`;
      });
      existing.replaceChildren(
        p(`识别到已有安装${probe.existing.version ? " · 版本 " + probe.existing.version : ""}`),
        p(`数据目录：${(probe.existing.dataDirs || [probe.existing.composeDir]).filter(Boolean).join("、") || "未知"}`),
        p(`服务：${(probe.existing.services || []).map((row) => `${row.name} ${row.running ? "运行中" : row.status}`).join("、") || "无"}`),
        p(accounts.length ? `账号：${accounts.join("、")}` : "尚未发现已登录账号")
      );
    } else {
      existing.hidden = true;
      existing.replaceChildren();
    }
    const needToken = !!(probe.existing?.found && !probe.ready);
    token.hidden = !needToken;
    oauth.hidden = tokenRow.hidden = !needToken;
    const needHelper = !!(probe.needPrivilege || task?.privilegeCommand);
    helper.hidden = !needHelper;
    helper.textContent = needHelper ? (task?.privilegeCommand || probe.helper?.command || "") : "";
    install.hidden = !!(probe.ready || probe.existing?.found || busy && task?.kind !== "install");
    takeover.hidden = !!(!probe.existing?.found || probe.ready || busy && task?.kind !== "takeover");
    $("wx-optional-services").hidden = install.hidden;
    if (task && (busy || failed)) {
      taskBox.hidden = false;
      const current = (task.steps || []).find((step) => step.id === task.step) || {};
      $("wx-task-step").textContent = `${kindLabel(task.kind)}：${current.name || task.step || task.status}`;
      $("wx-task-error").textContent = task.error || "";
      const images = $("wx-task-images");
      images.replaceChildren();
      for (const image of task.images || []) {
        const item = document.createElement("li");
        item.textContent = `${image.name} · ${imageStatus(image.status)}${image.error ? " · " + image.error : ""}`;
        images.append(item);
      }
      $("wx-task-retry").hidden = !failed;
    } else {
      taskBox.hidden = true;
    }
  }

  function kindLabel(kind) {
    return kind === "takeover" ? "接管" : kind === "maintenance" ? "服务维护" : "安装";
  }
  function imageStatus(status) {
    return { pending: "等待", pulling: "拉取中", completed: "完成", failed: "失败" }[status] || status;
  }
  function p(text) {
    const node = document.createElement("p");
    node.className = "hint";
    node.textContent = text;
    return node;
  }

  async function loadWxContacts(page) {
    const account = selectedWxAccount();
    const box = $("wx-contacts");
    const meta = $("wx-contact-meta");
    const revision = ++wxContactRevision;
    const accountKey = contactAccountKey(account);
    if (wxContactAccount !== accountKey) {
      wxContactAccount = accountKey;
      $("wx-default-notices").open = false;
      wxContactSelection = null;
      wxGroup = null;
      $("wx-group-form").hidden = true;
      $("wx-group-notices").hidden = true;
      window.wxContactActions.select(null);
      contactDetailStatus();
      box.replaceChildren();
      meta.textContent = "";
    }
    window.wxContactActions.directory(account);
    $("wx-contacts-prev").disabled = true;
    $("wx-contacts-next").disabled = true;
    if (!account) {
      box.replaceChildren();
      meta.textContent = "";
      return;
    }
    wxContactPage = page;
    const params = new URLSearchParams({
      id: account.id,
      wxid: account.wxid,
      keyword: $("wx-contact-keyword").value.trim(),
      page_index: String(page),
      page_size: "20"
    });
    const type = $("wx-contact-kind").value;
    if (type) params.set("type", type);
    let data, enabledGroups;
    try {
      [data, enabledGroups] = await Promise.all([api(`/api/wx/contacts?${params}`),
        account.capabilities?.groupChat ? wxAPI("groups", account).catch(() => []) : []]);
    } catch (error) {
      if (revision !== wxContactRevision) return;
      meta.textContent = "通讯录暂不可用，仍显示上次结果。" + (error.message ? ` ${error.message}` : "");
      return;
    }
    if (revision !== wxContactRevision) return;
    const items = data.items || data.Items || [];
    const total = data.total ?? data.Total ?? 0;
    box.replaceChildren();
    if (!items.length) {
      const empty = document.createElement("p");
      empty.className = "empty";
      empty.textContent = "没有匹配的好友或群聊";
      box.append(empty);
    }
    for (const item of items) {
      const row = document.createElement("button");
      row.type = "button";
      row.className = "wx-contact";
      const kind = item.type === "chat_room" ? "群聊" : item.type === "official_account" ? "公众号" : "好友";
      const title = item.remark || item.nickname || item.wechat_id;
      const enabled = Array.isArray(enabledGroups) && enabledGroups.includes(item.wechat_id);
      const name = document.createElement("strong");
      name.textContent = title;
      const identity = document.createElement("span");
      identity.className = "hint";
      identity.textContent = item.wechat_id;
      const badge = document.createElement("span");
      badge.className = enabled ? "contact-badge enabled" : "contact-badge";
      badge.textContent = enabled ? "群聊 · 已启用" : kind;
      row.append(name, badge, identity);
      row.disabled = item.type === "official_account";
      row.title = row.disabled ? "公众号不支持配置接入权限" : title;
      row.dataset.contact = item.wechat_id;
      row.addEventListener("click", () => {
        const current = selectedWxAccount();
        if (contactAccountKey(current) === accountKey) selectWxContact(item, current);
      });
      box.append(row);
    }
    updateContactSelection();
    const synced = data.syncedAt ? `，上次同步 ${formatTime(data.syncedAt)}` : "";
    $("wx-contacts-prev").disabled = page <= 1;
    $("wx-contacts-next").disabled = page * 20 >= total;
    meta.textContent = `共 ${total} 条 · 第 ${page} 页${synced}`;
  }

  function contactAccountKey(account) {
    return account ? JSON.stringify([account.id, account.wxid]) : "";
  }

  function updateContactSelection() {
    for (const row of $("wx-contacts").querySelectorAll(".wx-contact")) {
      if (!row.disabled) row.setAttribute("aria-pressed", String(row.dataset.contact === wxContactSelection?.id));
    }
  }

  function contactDetailStatus(message = "", error = false) {
    const status = $("wx-contact-detail-status");
    status.textContent = message;
    status.hidden = !message;
    status.classList.toggle("wx-error", error);
  }

  function isCurrentContact(selection) {
    const account = selectedWxAccount();
    return wxContactSelection === selection && account?.id === selection.accountId && account.wxid === selection.wxid;
  }

  async function loadGroupMembers(group, sync = false) {
    if (!group) return;
    const query = new URLSearchParams({id:group.accountId,wxid:group.wxid,group:group.id});
    const data = await api("/api/wx/group-members?" + query, sync ? {method:"POST",body:"{}"} : {});
    if (!isCurrentContact(group)) return;
    $("wx-group-member-picker").replaceChildren(...(Array.isArray(data) ? data : []).map(row => new Option(row.remark || row.nickname || row.wechat_id, row.wechat_id)));
  }

  function groupNoticesRequest(query, payload) {
    return api("/api/wx/group-notices?" + new URLSearchParams(query),
      payload === undefined ? {} : {method: "POST", body: JSON.stringify(payload)});
  }

  function createNoticeEditor(container, selectedScope) {
    container.append($("wx-notice-editor-template").content.cloneNode(true));
    const form = container.querySelector("form");
    const status = container.querySelector("[role=status]");
    const retry = container.querySelector("[data-retry]");
    const copy = form.querySelector("[data-copy-defaults]");
    const save = form.querySelector("button[type=submit]");
    let revision = 0;

    const sections = [
      {prefix: "welcome_", label: "进群欢迎", textLabel: "欢迎语", defaultText: "欢迎加入群聊！"},
      {prefix: "leave_chat_room_alert_", label: "退群提醒", textLabel: "提醒文本", defaultText: "{placeholder}退出了群聊", departure: true}
    ].map(definition => {
      const section = $("wx-notice-fields-template").content.firstElementChild.cloneNode(true);
      form.querySelector("[data-notice-fields]").append(section);
      section.querySelector("[data-notice-label]").textContent = definition.label;
      for (const field of section.querySelectorAll("[data-departure]")) field.hidden = !definition.departure;
      if (definition.departure) section.querySelector("[data-text-format]").dataset.format = "text";
      const fields = Object.fromEntries(Array.from(section.querySelectorAll("[data-field]"), field => {
        const name = field.dataset.field;
        field.id = container.id + "-" + definition.prefix + name;
        const label = section.querySelector('[data-label="' + name + '"]');
        if (label) label.htmlFor = field.id;
        return [name, field];
      }));
      const content = section.querySelector("[data-notice-content]");
      const textLabel = section.querySelector('[data-label="text"]');
      let extended = false;
      const placeholder = section.querySelector("[data-placeholder]");

      function update() {
        const enabled = fields.enabled.checked;
        const format = fields.type.value;
        content.hidden = !enabled;
        for (const group of section.querySelectorAll("[data-format]")) {
          group.hidden = !group.dataset.format.split(" ").includes(format);
        }
        for (const group of section.querySelectorAll("[data-extended]")) group.hidden = !extended;
        placeholder.hidden = !definition.departure || (format !== "text" && format !== "url");
        const formatLabel = section.querySelector('[data-label="type"]');
        formatLabel.hidden = fields.type.hidden = !!definition.departure && !extended;
        for (const field of Object.values(fields)) {
          const hiddenField = field.closest("[hidden]");
          field.disabled = field !== fields.enabled && (!enabled || (hiddenField !== null && content.contains(hiddenField)));
        }
        textLabel.textContent = !definition.departure && format === "url" ? "卡片说明" : definition.textLabel;
      }

      fields.enabled.addEventListener("change", update);
      fields.type.addEventListener("change", update);

      return {
        set(settings) {
          extended = settings.extended_formats === true;
          fields.enabled.checked = !!settings[definition.prefix + "enabled"];
          for (const [name, field] of Object.entries(fields)) {
            if (name !== "enabled") field.value = settings[definition.prefix + name] ?? "";
          }
          if (!fields.type.value || (definition.departure && !extended)) fields.type.value = "text";
          if (!fields.text.value) fields.text.value = definition.defaultText;
          update();
        },
        read() {
          const values = {[definition.prefix + "enabled"]: fields.enabled.checked};
          for (const [name, field] of Object.entries(fields)) {
            if (name !== "enabled" && !field.disabled) {
              values[definition.prefix + name] = field.type === "number" ? field.valueAsNumber : field.value.trim();
            }
          }
          return values;
        }
      };
    });

    function show(settings, forGroup) {
      form.hidden = false;
      for (const section of sections) section.set(settings);
      copy.hidden = !forGroup;
      const messages = [];
      if (settings.inherited) messages.push("当前沿用默认通告。保存后应用于本群。");
      if (!settings.extended_formats) messages.push("升级微信机器人客户端后可使用退群卡片、图片和表情包，以及自定义欢迎卡片标题和封面。");
      status.textContent = messages.join(" ");
    }

    function isCurrent(query, loadedRevision) {
      return loadedRevision === revision &&
        new URLSearchParams(selectedScope()).toString() === new URLSearchParams(query).toString();
    }

    async function load() {
      const query = selectedScope();
      const loadedRevision = ++revision;
      form.hidden = true;
      retry.hidden = true;
      if (!query) return;
      status.textContent = "正在读取通告设置…";
      try {
        const settings = await groupNoticesRequest(query);
        if (isCurrent(query, loadedRevision)) show(settings, !!query.group);
      } catch (error) {
        if (!isCurrent(query, loadedRevision)) return;
        status.textContent = error.message;
        retry.hidden = false;
      }
    }

    retry.addEventListener("click", load);
    copy.addEventListener("click", () => {
      const query = selectedScope();
      const loadedRevision = revision;
      if (!query?.group) return;
      action(copy, async () => {
        const settings = await groupNoticesRequest({id: query.id, wxid: query.wxid, scope: "global"});
        if (!isCurrent(query, loadedRevision)) return;
        show(settings, true);
        status.textContent = "已填入默认通告，保存后应用于本群。";
      });
    });
    form.addEventListener("submit", event => {
      event.preventDefault();
      const query = selectedScope();
      const loadedRevision = revision;
      if (!query) return;
      const payload = Object.assign({}, ...sections.map(section => section.read()));
      action(save, async () => {
        const result = await groupNoticesRequest(query, payload);
        if (!isCurrent(query, loadedRevision)) return;
        show(result, !!query.group);
        notice("进退群通告已保存。");
      });
    });
    return {load};
  }

  async function selectWxContact(item, account) {
    const selection = { accountId: account.id, wxid: account.wxid, id: item.wechat_id, item };
    wxContactSelection = selection;
    updateContactSelection();
    wxGroup = null;
    wxFriend = "";
    $("wx-group-form").hidden = true;
    $("wx-group-notices").hidden = true;
    $("wx-friend-form").hidden = true;
    $("wx-friend").value = "";
    window.wxContactActions.select(selection);
    contactDetailStatus();
    revealContactEditor();
    try {
      if (selection.id.endsWith("@chatroom")) {
        wxGroup = selection;
        $("wx-group-notices").hidden = false;
        groupNoticeEditor.load();
        if (!account.capabilities?.groupChat) return;
        contactDetailStatus("正在读取 Codex 接入权限…");
        const group = await wxAPI("group", account, undefined, selection.id);
        if (!isCurrentContact(selection)) return;
        $("wx-group-enabled").checked = group.enabled;
        $("wx-group-trigger").value = group.trigger;
        $("wx-group-session").value = group.session;
        $("wx-group-mode").value = group.mode;
        $("wx-group-members").value = (group.members || []).join("\n");
        $("wx-group-admins").value = (group.admins || []).join("\n");
        $("wx-group-commands").replaceChildren(...wxCommands.filter(command => command.handler_type === "web").map(command => new Option(command.name, command.name, false, (group.commands || []).includes(command.name))));
        await window.showWXProfile("group", group, () => isCurrentContact(selection));
        if (isCurrentContact(selection)) {
          $("wx-group-member-picker").replaceChildren();
          $("wx-group-form").hidden = false;
          loadGroupMembers(selection).catch(() => {});
        }
      } else {
        contactDetailStatus("正在读取好友权限…");
        $("wx-friend").value = selection.id;
        const access = await wxAPI("access", account, undefined, selection.id);
        if (!isCurrentContact(selection)) return;
        $("wx-friend-enabled").checked = access.enabled;
        $("wx-friend-mode").value = ["tools", "admin"].includes(access.mode) ? access.mode : "text";
        $("wx-friend-commands").replaceChildren(...wxCommands.map(command => new Option(command.name, command.name, false, (access.commands || []).includes(command.name))));
        await window.showWXProfile("friend", access, () => isCurrentContact(selection));
        if (isCurrentContact(selection)) {
          wxFriend = selection.id;
          $("wx-friend-form").hidden = false;
        }
      }
      if (isCurrentContact(selection)) contactDetailStatus();
    } catch (error) {
      if (isCurrentContact(selection)) contactDetailStatus(error.message, true);
    }
  }

  function revealContactEditor() {
    const detail = $("wx-contact-detail");
    if (window.settingsNavigation.view !== "wx-contacts") return;
    const top = detail.getBoundingClientRect().top;
    const headerBottom = document.querySelector(".topbar").getBoundingClientRect().bottom;
    if (!matchMedia("(max-width: 1050px)").matches && top >= headerBottom && top < innerHeight) return;
    detail.focus({ preventScroll: true });
    detail.scrollIntoView({ block: "start" });
  }

  window.wxContactRobot = account => robots.find(row => "client_" + row.robot_code === account?.id);
  window.wxRefreshContacts = () => loadWxContacts(wxContactPage);
  window.wxClearContact = selection => {
    if (!isCurrentContact(selection)) return;
    wxContactSelection = wxGroup = null;
    wxFriend = "";
    $("wx-friend").value = "";
    $("wx-friend-form").hidden = true;
    $("wx-group-form").hidden = true;
    $("wx-group-notices").hidden = true;
    contactDetailStatus();
    updateContactSelection();
    window.wxContactActions.select(null);
  };

  function formatTime(value) {
    const date = new Date(value);
    if (Number.isNaN(date.getTime())) return value;
    return date.toLocaleString("zh-CN", { hour12: false });
  }
})();
