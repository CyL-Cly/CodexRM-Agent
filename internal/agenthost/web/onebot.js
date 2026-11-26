"use strict";
(() => {
  let supported = false;
  let bound = false;
  let dirty = false;
  let loading = false;
  const byId = id => document.getElementById(id);
  const input = (field, value = "", options = {}) => {
    const element = document.createElement(options.textarea ? "textarea" : "input");
    element.dataset.field = field;
    if (!options.textarea) element.type = options.type || "text";
    if (options.textarea) element.rows = options.rows || 2;
    if (options.min !== undefined) element.min = options.min;
    if (options.step !== undefined) element.step = options.step;
    if (options.placeholder) element.placeholder = options.placeholder;
    if (element.type === "checkbox") element.checked = value === true;
    else element.value = value ?? "";
    return element;
  };
  const select = (field, value, choices) => {
    const element = document.createElement("select");
    element.dataset.field = field;
    for (const [key, label] of choices) element.add(new Option(label, key, false, value === key));
    return element;
  };
  const label = (text, control) => {
    const wrapper = document.createElement("label");
    wrapper.textContent = text;
    if (control.type === "checkbox") {
      wrapper.className = "inline-field";
      wrapper.textContent = "";
      wrapper.append(control, document.createTextNode(text));
    } else wrapper.append(control);
    return wrapper;
  };
  const removeButton = fieldset => {
    const button = document.createElement("button");
    button.type = "button";
    button.className = "text-button";
    button.textContent = "删除此配置";
    button.addEventListener("click", () => { fieldset.remove(); dirty = true; });
    return button;
  };
  function userRow(id = "", access = {}) {
    const fieldset = document.createElement("fieldset");
    fieldset.dataset.onebotUser = "";
    const legend = document.createElement("legend");
    legend.textContent = id ? `私聊 QQ ${id}` : "新私聊 QQ";
    const qq = input("id", id, { type: "number", min: 1, step: 1, placeholder: "QQ 号" });
    qq.required = true;
    fieldset.append(legend,
      label("QQ 号", qq),
      label("允许接入", input("enabled", access.enabled !== false, { type: "checkbox" })),
      label("权限模式", select("mode", access.mode || "text", [["text", "仅 AI 聊天"], ["tools", "工作区工具"], ["admin", "管理员（完整主机访问）"]])) ,
      label("已登记工作区绝对路径（留空使用默认）", input("workspace", access.workspace || "", { placeholder: "/srv/project" })),
      label("模型（留空使用 Codex 默认）", input("model", access.model || "")),
      label("补充提示词", input("prompt", access.prompt || "", { textarea: true, rows: 2 })),
      removeButton(fieldset));
    return fieldset;
  }
  function groupRow(id = "", group = {}) {
    const fieldset = document.createElement("fieldset");
    fieldset.dataset.onebotGroup = "";
    const legend = document.createElement("legend");
    legend.textContent = id ? `群 ${id}` : "新群聊";
    const qq = input("id", id, { type: "number", min: 1, step: 1, placeholder: "群号" });
    qq.required = true;
    fieldset.append(legend,
      label("群号", qq),
      label("启用本群", input("enabled", group.enabled !== false, { type: "checkbox" })),
      label("触发方式", select("trigger", group.trigger || "mention", [["mention", "仅 @机器人"], ["all", "全部消息"]])),
      label("会话隔离", select("session", group.session || "shared", [["shared", "全群共享"], ["member", "成员独立"]])),
      label("权限模式", select("mode", group.mode || "text", [["text", "仅 AI 聊天"], ["tools", "工作区工具"]])),
      label("已登记工作区绝对路径（留空使用默认）", input("workspace", group.workspace || "", { placeholder: "/srv/project" })),
      label("模型（留空使用 Codex 默认）", input("model", group.model || "")),
      label("补充提示词", input("prompt", group.prompt || "", { textarea: true, rows: 2 })),
      label("允许成员 QQ（留空允许全群；空格或逗号分隔）", input("members", (group.members || []).join(" "), { placeholder: "10001 10002" })),
      label("共享会话管理员 QQ（空格或逗号分隔）", input("admins", (group.admins || []).join(" "), { placeholder: "10001 10002" })),
      removeButton(fieldset));
    return fieldset;
  }
  function readRows(selector, group = false) {
    const result = {};
    for (const row of document.querySelectorAll(selector)) {
      const value = field => row.querySelector(`[data-field="${field}"]`);
      const id = value("id").value.trim();
      if (!/^[1-9][0-9]*$/.test(id)) throw new Error(`${group ? "群号" : "QQ 号"}无效：${id || "未填写"}`);
      if (result[id]) throw new Error(`配置重复：${id}`);
      const item = {
        enabled: value("enabled").checked,
        mode: value("mode").value,
        workspace: value("workspace").value.trim(),
        model: value("model").value.trim(),
        prompt: value("prompt").value,
      };
      if (group) {
        item.trigger = value("trigger").value;
        item.session = value("session").value;
        item.members = value("members").value.split(/[\s,;]+/).filter(Boolean);
        item.admins = value("admins").value.split(/[\s,;]+/).filter(Boolean);
      }
      result[id] = item;
    }
    return result;
  }
  function applySettings(settings) {
    byId("onebot-enabled").checked = settings.enabled === true;
    byId("onebot-mode").value = settings.mode || "ws-reverse";
    byId("onebot-url").value = settings.url || "";
    byId("onebot-listen").value = settings.listen || "";
    byId("onebot-token").value = "";
    byId("onebot-clear-token").checked = false;
    byId("onebot-self-id").value = settings.selfId || 0;
    byId("onebot-message-limit").value = settings.messageLimit || 1800;
    byId("onebot-remote-media").checked = settings.allowRemoteMedia === true;
    byId("onebot-private-media").checked = settings.allowPrivateMedia === true;
    byId("onebot-local-media").checked = settings.allowLocalMedia === true;
    byId("onebot-forward-lookup").checked = settings.allowForwardLookup === true;
    byId("onebot-accept-friend").checked = settings.autoAcceptFriend === true;
    byId("onebot-accept-group").checked = settings.autoAcceptGroupInvite === true;
    const users = byId("onebot-users");
    users.replaceChildren(...Object.entries(settings.users || {}).map(([id, access]) => userRow(id, access)));
    const groups = byId("onebot-groups");
    groups.replaceChildren(...Object.entries(settings.groups || {}).map(([id, group]) => groupRow(id, group)));
    byId("onebot-token").disabled = false;
    syncMode();
    dirty = false;
  }
  function syncStatus(data) {
    const status = data.status || {};
    const transport = status.transport || status;
    const names = { disabled: "未启用", connecting: "正在连接", connected: "已连接", retrying: "等待重连" };
    const state = transport.state || status.state || "disabled";
    const details = [];
    if (transport.selfId) details.push(`机器人 QQ ${transport.selfId}`);
    if (status.sessions !== undefined) details.push(`${status.sessions} 个持久会话`);
    if (status.activeTurns) details.push(`${status.activeTurns} 个运行中任务`);
    byId("onebot-status").textContent = `${names[state] || state}${details.length ? " · " + details.join(" · ") : ""}`;
    byId("onebot-error").textContent = data.reloadError || data.loadError || transport.error || status.error || "";
    byId("overview-onebot-state").textContent = names[state] || state;
    byId("overview-onebot-detail").textContent = details.join(" · ") || "QQ 私聊与群聊";
    byId("onebot-token-hint").textContent = data.accessTokenSet ? "已保存 Token；留空保留，输入新值替换。" : "未保存 Token；非回环监听，以及启用工作区工具或管理员的回环监听均必须设置。";
  }
  async function load(fill = false) {
    if (!supported || loading || (dirty && !fill)) return;
    loading = true;
    try {
      const data = await api("/api/onebot");
      syncStatus(data);
      if (fill) applySettings(data.settings || {});
    } catch (error) {
      byId("onebot-error").textContent = error.message;
      if (fill) notice(error.message, true);
    } finally { loading = false; }
  }
  function syncMode() {
    const mode = byId("onebot-mode").value;
    byId("onebot-url").required = mode !== "ws-reverse";
    byId("onebot-listen").required = mode !== "ws";
    byId("onebot-listen").disabled = mode === "ws";
  }
  function bind() {
    if (bound) return;
    bound = true;
    byId("onebot-form").addEventListener("input", event => {
      if (event.target.id !== "onebot-token" || event.target.value) dirty = true;
    });
    byId("onebot-form").addEventListener("change", () => { dirty = true; });
    byId("onebot-mode").addEventListener("change", syncMode);
    byId("onebot-add-user").addEventListener("click", () => { byId("onebot-users").append(userRow()); dirty = true; });
    byId("onebot-add-group").addEventListener("click", () => { byId("onebot-groups").append(groupRow()); dirty = true; });
    byId("onebot-toggle-token").addEventListener("click", () => {
      const show = byId("onebot-token").type === "password";
      byId("onebot-token").type = show ? "text" : "password";
      byId("onebot-toggle-token").textContent = show ? "隐藏" : "显示";
    });
    byId("onebot-clear-token").addEventListener("change", event => {
      byId("onebot-token").disabled = event.target.checked;
      if (event.target.checked) byId("onebot-token").value = "";
    });
    byId("onebot-form").addEventListener("submit", event => {
      event.preventDefault();
      action(byId("onebot-save"), async () => {
        const settings = {
          enabled: byId("onebot-enabled").checked,
          mode: byId("onebot-mode").value,
          url: byId("onebot-url").value.trim(),
          listen: byId("onebot-listen").value.trim(),
          accessToken: byId("onebot-token").value,
          clearAccessToken: byId("onebot-clear-token").checked,
          selfId: Number(byId("onebot-self-id").value || 0),
          messageLimit: Number(byId("onebot-message-limit").value),
          allowRemoteMedia: byId("onebot-remote-media").checked,
          allowPrivateMedia: byId("onebot-private-media").checked,
          allowLocalMedia: byId("onebot-local-media").checked,
          allowForwardLookup: byId("onebot-forward-lookup").checked,
          autoAcceptFriend: byId("onebot-accept-friend").checked,
          autoAcceptGroupInvite: byId("onebot-accept-group").checked,
          users: readRows("[data-onebot-user]"),
          groups: readRows("[data-onebot-group]", true),
        };
        const admins = Object.entries(settings.users).filter(([, access]) => access.enabled && access.mode === "admin").map(([id]) => id);
        if (settings.enabled && admins.length && !confirm(`以下 QQ 将获得 Agent 主机的完整访问权限：${admins.join(", ")}。确认继续？`)) return;
        const data = await api("/api/onebot", { method: "PUT", body: JSON.stringify(settings) });
        applySettings(data.settings || settings);
        syncStatus(data);
        notice(data.reloadError ? `配置已保存，但热重载未完成：${data.reloadError}` : "OneBot 配置已保存并应用。", !!data.reloadError);
      });
    });
    byId("onebot-refresh").addEventListener("click", () => action(byId("onebot-refresh"), async () => {
      if (dirty && !confirm("放弃尚未保存的 OneBot 表单修改并重新读取？")) return;
      await load(true);
    }));
    byId("onebot-test-form").addEventListener("submit", event => {
      event.preventDefault();
      action(byId("onebot-test-send"), async () => {
        const group = byId("onebot-test-kind").value === "group";
        const id = Number(byId("onebot-test-target").value);
        await api("/api/onebot/test", { method: "POST", body: JSON.stringify({ userId: group ? 0 : id, groupId: group ? id : 0, text: byId("onebot-test-text").value }) });
        notice("测试消息已发送。");
      });
    });
    document.addEventListener("settings:view", event => {
      if (event.detail === "onebot") load(!dirty);
    });
    setInterval(() => {
      if (supported && !document.getElementById("settings-content").hidden) load(false);
    }, 5000);
  }
  window.initOneBotPanel = data => {
    supported = data.oneBotSupported === true;
    byId("nav-onebot").hidden = !supported;
    byId("overview-onebot").hidden = !supported;
    if (!supported) return;
    bind();
    load(true);
  };
})();
