(() => {
  const navToggle = $("wx-nav-toggle");
  const navModules = $("wx-nav-modules");
  const moduleGrid = $("wx-op-modules");
  const modulePage = $("wx-op-module-page");
  const actionMenu = $("wx-op-actions");
  const status = $("wx-op-status");
  const detail = $("wx-op-detail");
  const robot = $("wx-robot");
  const settingsPairs = new Map([
    ["post-chat-room-settings", "get-chat-room-settings"],
    ["post-friend-settings", "get-friend-settings"],
    ["post-global-settings", "get-global-settings"],
    ["post-system-settings", "get-system-settings"],
    ["post-moments--settings", "get-moments--settings"],
    ["post-oss-settings", "get-oss-settings"]
  ]);
  const readSettings = new Map(Array.from(settingsPairs, ([save, read]) => [read, save]));
  const codexFields = new Set(["chat_ai_enabled", "chat_model", "chat_prompt", "chat_room_summary_model",
    "image_ai_enabled", "image_ai_settings", "image_recognition_model", "comment_model", "workflow_model",
    "image_understanding_model", "video_understanding_model", "ai_api_key", "ai_base_url"]);
  const settingLabels = {
    chat_ai_trigger: "词云忽略的消息前缀", chat_api_key: "MiMo 语音备用 API 密钥",
    chat_base_url: "MiMo 语音备用 API 地址",
    max_completion_tokens: "最大回复 Token 数", image_ai_enabled: "图片识别",
    image_recognition_model: "图片识别模型", image_ai_settings: "图片识别配置",
    tts_enabled: "语音回复", tts_model: "语音模型", tts_settings: "语音配置",
    knowledge_categories: "知识库分类", memory_extraction_blacklist: "记忆提取黑名单",
    chat_room_ranking_enabled: "群活跃排行", chat_room_summary_enabled: "群聊总结",
    chat_room_summary_mode: "总结模式", chat_room_summary_model: "总结模型",
    morning_enabled: "早安提醒", news_enabled: "新闻推送", news_type: "新闻类型",
    pat_enabled: "拍一拍回复", pat_text: "拍一拍回复内容", pat_type: "拍一拍回复类型",
    pat_voice_timbre: "拍一拍语音音色", podcast_enabled: "播客", podcast_config: "播客配置",
    short_video_parsing_enabled: "短视频解析", wxhb_notify_enabled: "红包提醒",
    wxhb_notify_member_list: "红包提醒成员"
  };
  const modules = new Map();
  let catalog;
  let catalogLoad;
  let activeOperation = "";

  function directorySection(id) {
    if (id.startsWith("post-chat-room--") && /--(create|join)$/.test(id)) return "directory";
    if (id === "get-contact--list" || id === "post-contact--sync" ||
        /^post-contact--friend--(search|add|pass-verify)$/.test(id)) return "directory";
    if (id === "get-chat--history" || id.startsWith("get-chat--") && id.endsWith("--download") ||
        id === "post-message--revoke") return "history";
    if (id.startsWith("post-message--send--")) return "send";
    if (id.endsWith("chat-room-settings") || id.endsWith("friend-settings")) return "plugins";
    if (/^post-chat-room--(announcement|name|remark)$/.test(id) || id === "delete-chat-room--quit" ||
        id === "post-contact--friend--remark" || id === "delete-contact--friend") return "profile";
    if (id.includes("-chat-room--") || id === "post-contact--friend--add-from-chat-room") return "members";
    return "";
  }

  function loadCatalog() {
    if (catalogLoad) return catalogLoad;
    catalogLoad = api("/wx-operations.json").then(items => {
      catalog = new Map(items.map(operation => [operation.id, operation]));
      return catalog;
    }).catch(error => {
      catalogLoad = null;
      throw error;
    });
    return catalogLoad;
  }

  function operationLink(label, module, operation) {
    const params = new URLSearchParams({ view: "wx-operations", "wx-module": module });
    if (operation) params.set("wx-op", operation);
    const link = document.createElement("a");
    link.href = "?" + params;
    link.dataset.route = "wx-operations";
    link.textContent = label;
    return link;
  }

  function expandMenu(expand) {
    navModules.hidden = !expand;
    navToggle.setAttribute("aria-expanded", String(expand));
    navToggle.setAttribute("aria-label", (expand ? "收起" : "展开") + "功能管理菜单");
  }

  async function loadOperations() {
    if (modules.size) return;
    try {
      const items = await loadCatalog();
      if (modules.size) return;
      for (const operation of items.values()) {
        if (directorySection(operation.id) || readSettings.has(operation.id)) continue;
        if (!modules.has(operation.module)) modules.set(operation.module, []);
        modules.get(operation.module).push(operation);
      }
      const names = [...modules.keys()];
      navModules.replaceChildren(...names.map(name => operationLink(name, name)));
      moduleGrid.replaceChildren(...names.map(name => operationLink(name, name)));
      renderOperations();
    } catch (error) {
      status.hidden = false;
      status.textContent = error.message;
      navModules.textContent = "读取失败，请重新展开。";
    }
  }

  document.addEventListener("settings:view", event => {
    if (event.detail === "wx-operations") {
      expandMenu(true);
      loadOperations();
    }
    renderOperations();
  });
  navToggle.addEventListener("click", () => {
    expandMenu(navModules.hidden);
    if (!navModules.hidden) loadOperations();
  });
  robot.addEventListener("change", () => {
    activeOperation = "";
    renderOperations();
  });
  document.addEventListener("wx:robots", renderOperations);

  function renderOperations() {
    if (window.settingsNavigation.view !== "wx-operations") {
      activeOperation = "";
      return;
    }
    const params = new URLSearchParams(location.search);
    const requested = params.get("wx-op");
    const section = requested ? directorySection(requested) : "";
    const name = params.get("wx-module");
    if (section || !requested && ["群管理", "群插件设置", "好友", "好友设置", "聊天记录"].includes(name)) {
      window.settingsNavigation.openContacts(requested);
      return;
    }
    if (!modules.size) return;
    const actions = modules.get(name);
    const operationID = readSettings.get(requested) || requested;
    for (const link of navModules.children) {
      if (link.textContent === name) link.setAttribute("aria-current", "page");
      else link.removeAttribute("aria-current");
    }
    if (actions) document.querySelector('.nav-link[data-route="wx-operations"]').removeAttribute("aria-current");
    status.hidden = true;
    moduleGrid.hidden = !!actions;
    modulePage.hidden = !actions;
    $("wx-op-module-title").textContent = actions ? name : "";
    actionMenu.replaceChildren(...(actions || []).map(operation => {
      const link = operationLink(settingsPairs.has(operation.id) ? operation.label.replace(/^保存/, "") : operation.label, name, operation.id);
      if (operation.id === operationID) link.setAttribute("aria-current", "page");
      return link;
    }));
    const operation = actions?.find(row => row.id === operationID);
    const key = operation ? operation.id + ":" + robot.value : "";
    detail.hidden = !operation;
    if (activeOperation === key && detail.hasChildNodes()) return;
    activeOperation = key;
    detail.replaceChildren();
    if (operation) {
      const scope = { robot: robot.value, current: () => activeOperation === key };
      const read = settingsPairs.get(operation.id);
      detail.append(read ? settingsEditor(operation, catalog.get(read), scope) : operationForm(operation, scope));
    }
  }

  function element(tag, content, className) {
    const node = document.createElement(tag);
    if (content) node.textContent = content;
    if (className) node.className = className;
    return node;
  }

  function secretKey(key) {
    return /(token|password|secret|api_key|cookie)/i.test(key);
  }

  let fieldID = 0;
  function operationControl(field, options = {}) {
    const choices = field.key.endsWith("score_action");
    const list = options.list;
    const tag = choices || field.type === "boolean" ? "select" :
      ["object", "array"].includes(field.type) || options.multiline ? "textarea" : "input";
    const input = element(tag);
    input.id = "wx-operation-field-" + ++fieldID;
    input.name = field.key;
    input.required = !!field.required;
    const label = element("label", options.label || settingLabels[field.key] || field.label);
    label.htmlFor = input.id;
    if (choices) input.replaceChildren(new Option("不调整", ""), new Option("增加", "increase"), new Option("减少", "reduce"));
    else if (field.type === "boolean") input.replaceChildren(new Option(options.optionalLabel || "沿用默认", ""), new Option("启用", "true"), new Option("停用", "false"));
    else if (field.type === "file") input.type = "file";
    else if (secretKey(field.key) || field.label === "设备登录备份") {
      input.type = "password";
      input.autocomplete = "off";
    } else if (options.datetime) input.type = "datetime-local";
    else if (["number", "integer"].includes(field.type)) input.inputMode = "numeric";
    if (tag === "textarea") input.rows = options.multiline ? 3 : 2;
    if (list) input.placeholder = "可输入多项，每行一项";
    return { field, input, label, list };
  }

  function parseInput(control) {
    const { field, input, list } = control;
    const value = input.value;
    if (field.type === "file") return input.files[0];
    if (input.type === "datetime-local") return Math.floor(new Date(value).getTime() / 1000);
    if (list) return [...new Set(value.split(/[\s,，]+/).filter(Boolean))];
    if (["integer", "number"].includes(field.type)) {
      const number = Number(value);
      if (!Number.isFinite(number) || (field.type === "integer" && !Number.isSafeInteger(number))) {
        throw new Error(field.label + "格式有误");
      }
      return number;
    }
    if (["object", "array", "boolean"].includes(field.type)) {
      try {
        const parsed = JSON.parse(value);
        if (field.type === "array" && !Array.isArray(parsed) ||
            field.type === "object" && (!parsed || typeof parsed !== "object" || Array.isArray(parsed))) throw new Error();
        return parsed;
      } catch { throw new Error(field.label + "格式有误"); }
    }
    return value;
  }

  function request(operation, scope, values = {}, headers = {}) {
    if ((!scope.robot && !operation.noRobot) || !scope.current()) throw new Error("请重新选择当前微信账号。");
    const query = new URLSearchParams({ robot: scope.robot });
    if (scope.account) {
      query.set("account", scope.account.id);
      query.set("wxid", scope.account.wxid);
      if (scope.contact) query.set("contact", scope.contact);
    }
    const payload = Object.create(null);
    const multipart = operation.fields.some(field => field.input === "formData" && field.type === "file");
    const upload = multipart ? new FormData() : null;
    for (const field of operation.fields) {
      const value = values[field.key];
      if (value === undefined) continue;
      if (field.input === "query") query.set(field.key, typeof value === "object" ? JSON.stringify(value) : value);
      else if (multipart) upload.set(field.key, value);
      else payload[field.key] = value;
    }
    const path = "/api/wx/manage/" + operation.id + "?" + query;
    const body = upload || JSON.stringify(payload);
    return operation.download ? downloadMedia(path, headers, body) : operation.stream ? streamProgress(path, headers, scope) :
      api(path, { method: "POST", headers, body });
  }

  function formStatus() {
    const result = element("div", "", "wx-op-result");
    result.setAttribute("role", "status");
    return result;
  }

  function operationForm(operation, scope, options = {}) {
    const root = element("div", "", "wx-operation");
    if (options.heading !== false) root.append(element("h3", options.label || operation.label));
    const form = element("form");
    const controls = [];
    const bound = { ...options.values };
    if (scope.account && operation.fields.some(field => field.key === "id" && field.required)) bound.id = Number(scope.robot);
    for (const field of operation.fields) {
      if (Object.hasOwn(bound, field.key)) continue;
      const control = operationControl(field, options.fields?.[field.key]);
      if (options.initial?.[field.key] != null) control.input.value = options.initial[field.key];
      form.append(control.label, control.input);
      controls.push(control);
    }
    const footer = element("div", "", "form-footer");
    const button = element("button", options.button || operation.label, options.danger ? "secondary" : "primary");
    footer.append(button);
    form.append(footer);
    const result = formStatus();
    root.append(form, result);
    let running = false;
    form.addEventListener("submit", async event => {
      event.preventDefault();
      if (running || !scope.current()) return;
      const headers = {};
      if (operation.confirmText) {
        const entered = window.prompt("请输入「" + operation.confirmText + "」确认删除机器人数据。");
        if (entered !== operation.confirmText) return;
        headers["X-CodexRM-Confirm"] = "delete-robot";
      } else if ((options.confirm || operation.method === "DELETE") &&
          !window.confirm(options.confirm || "确认执行「" + operation.label + "」？")) return;
      button.disabled = running = true;
      result.textContent = "处理中…";
      try {
        const values = { ...bound };
        for (const control of controls) {
          if (control.field.type !== "file" && control.input.value === "") continue;
          const value = parseInput(control);
          if (control.input.required && control.list && !value.length) throw new Error("请填写" + control.label.textContent);
          values[control.field.key] = value;
        }
        const data = await request(operation, { ...scope, progress: message => {
          if (scope.current() && root.isConnected) result.textContent = message;
        } }, values, headers);
        if (!scope.current() || !root.isConnected) return;
        if (operation.id === "get-wx-admin-oauth-qr") {
          const qr = element("img");
          qr.alt = "微信公众号授权二维码";
          qr.width = qr.height = 224;
          qr.src = data.image;
          result.replaceChildren(qr);
        } else if (operation.id === "post-wx-admin-refresh-token") {
          const field = element("input");
          field.setAttribute("aria-label", "新 API 令牌");
          field.readOnly = true;
          field.type = "password";
          field.value = typeof data === "string" ? data : data.token;
          const toggle = element("button", "显示令牌", "secondary");
          toggle.type = "button";
          toggle.addEventListener("click", () => {
            field.type = field.type === "password" ? "text" : "password";
            toggle.textContent = field.type === "password" ? "显示令牌" : "隐藏令牌";
          });
          result.replaceChildren(field, toggle);
          if (data.warning) result.append(element("p", data.warning));
        } else if (operation.id === "delete-wx-admin-logout") {
          document.querySelector('a[data-route="wx-overview"]')?.click();
          await window.refreshWxPanel();
        } else if (options.onResult) options.onResult(data, result, values);
        else result.replaceChildren(renderResult(data));
      } catch (error) {
        if (scope.current() && root.isConnected) result.textContent = error.message;
      } finally { button.disabled = running = false; }
    });
    return root;
  }

  function settingsEditor(save, read, scope, options = {}) {
    const root = element("div", "", "wx-operation wx-settings-editor");
    const form = element("form");
    const result = formStatus();
    const refresh = element("button", "读取设置", "secondary");
    refresh.type = "button";
    const commit = element("button", "保存设置", "primary");
    const footer = element("div", "", "form-footer");
    footer.append(refresh, commit);
    const fields = element("div");
    form.append(fields, footer);
    root.append(form, result);
    let snapshot;
    let running = false;
    const controls = [];
    const identity = new Set(["id", "config_id", "system_settings_id", ...Object.keys(options.values || {})]);
    for (const field of save.fields) {
      if (identity.has(field.key) || options.exclude?.(field.key)) continue;
      if (["post-chat-room-settings", "post-friend-settings", "post-global-settings", "post-moments--settings"].includes(save.id) &&
          codexFields.has(field.key)) continue;
      const control = operationControl(field, { multiline: field.key.endsWith("_prompt") });
      fields.append(control.label, control.input);
      controls.push(control);
    }
    fields.hidden = commit.hidden = true;

    async function load(message) {
      snapshot = null;
      const data = await request(read, scope, options.values);
      if (!scope.current() || !root.isConnected) return;
      snapshot = options.patch ? data?.values : data;
      if (!snapshot || typeof snapshot !== "object" || Array.isArray(snapshot)) throw new Error("设置数据格式有误。");
      for (const { field, input } of controls) {
        const value = snapshot[field.key];
        input.value = value == null ? "" : typeof value === "object" ? JSON.stringify(value, null, 2) : String(value);
        if (field.type === "boolean") input.options[0].disabled = value != null;
        input.dataset.saved = input.value;
      }
      result.textContent = message || (data.inherited ? "当前沿用机器人的默认设置。" : "");
      fields.hidden = commit.hidden = false;
      refresh.textContent = "重新读取";
    }

    async function run(work, pending) {
      if (running || !scope.current()) return;
      refresh.disabled = commit.disabled = running = true;
      result.textContent = pending;
      try { await work(); }
      catch (error) {
        if (scope.current() && root.isConnected) result.textContent = error.message;
      } finally {
        if (!snapshot) fields.hidden = commit.hidden = true;
        refresh.disabled = commit.disabled = running = false;
      }
    }

    refresh.addEventListener("click", () => run(() => load(), "正在读取设置…"));
    queueMicrotask(() => {
      const disclosure = root.closest("details");
      if (!disclosure || disclosure.open) refresh.click();
      disclosure?.addEventListener("toggle", () => {
        if (disclosure.open && !snapshot) refresh.click();
      });
    });
    form.addEventListener("submit", event => {
      event.preventDefault();
      if (!snapshot) return;
      run(async () => {
        const changes = {};
        for (const control of controls) {
          const { field, input } = control;
          if (input.value === input.dataset.saved) continue;
          if (input.value === "" && field.type !== "string") {
            throw new Error("请为「" + control.label.textContent + "」填写值。清空列表请用 []，清空对象请用 {}。");
          }
          changes[field.key] = parseInput(control);
        }
        if (!Object.keys(changes).length) {
          result.textContent = "没有需要保存的改动。";
          return;
        }
        const values = options.patch ? changes : { ...snapshot, ...changes };
        if (!options.patch && save.fields.some(field => field.key === "id")) {
          values.id = Number(scope.robot);
        }
        if (save.id === "post-system-settings") values.system_settings_id = snapshot.system_settings_id ?? snapshot.id;
        await request(save, scope, values);
        try { await load("设置已保存。"); }
        catch { result.textContent = "设置已保存，请重新读取以确认。"; }
      }, "正在保存设置…");
    });
    return root;
  }

  async function operationResponse(path, headers, body) {
    const requestHeaders = new Headers(headers);
    if (token) requestHeaders.set("Authorization", "Bearer " + token);
    if (typeof body === "string") requestHeaders.set("Content-Type", "application/json");
    const response = await fetch(path, { method: "POST", headers: requestHeaders, body, cache: "no-store" });
    if (!response.ok) {
      const error = await response.json().catch(() => ({}));
      throw new Error(error.error || "请求失败");
    }
    return response;
  }

  async function streamProgress(path, headers, scope) {
    const response = await operationResponse(path, headers);
    const reader = response.body.pipeThrough(new TextDecoderStream()).getReader();
    let pending = "";
    let complete = false;
    try {
      while (scope.current()) {
        const chunk = await reader.read();
        if (chunk.done) break;
        pending += chunk.value.replace(/\r/g, "");
        const packets = pending.split("\n\n");
        pending = packets.pop();
        if (pending.length > 65536) throw new Error("镜像进度响应无效");
        for (const packet of packets) {
          const lines = packet.split("\n");
          const event = lines.find(line => line.startsWith("event:"))?.slice(6).trim();
          const value = lines.find(line => line.startsWith("data:"))?.slice(5).trim();
          if (event === "error") throw new Error(value || "镜像拉取失败");
          if (event !== "progress" || !value) continue;
          const row = JSON.parse(value);
          if (row.error || row.status === "error") throw new Error(row.error || "镜像拉取失败");
          if (row.status === "all_complete") complete = true;
          if (row.image) scope.progress?.([row.image, row.status, row.progress].filter(Boolean).join(" · "));
        }
      }
      if (scope.current() && !complete) throw new Error("镜像拉取中断");
      return "镜像拉取完成。";
    } finally {
      await reader.cancel().catch(() => {});
    }
  }

  async function downloadMedia(path, headers, body) {
    const response = await operationResponse(path, headers, body);
    const url = URL.createObjectURL(await response.blob());
    const link = document.createElement("a");
    link.href = url;
    link.download = /filename=([A-Za-z0-9._-]+)/.exec(response.headers.get("Content-Disposition") || "")?.[1] || "wechat-media.bin";
    link.click();
    setTimeout(() => URL.revokeObjectURL(url), 30000);
    return "文件已下载。";
  }

  function renderResult(data) {
    const root = element("div");
    if (data == null) { root.textContent = "操作成功。"; return root; }
    const rows = Array.isArray(data) ? data : Array.isArray(data.items) ? data.items : null;
    const objects = rows || [data];
    for (const row of objects.slice(0, 200)) {
      if (row === null || typeof row !== "object") { root.append(element("p", String(row))); continue; }
      const line = element("dl");
      for (const [key, value] of Object.entries(row)) {
        if (secretKey(key) || value == null) continue;
        line.append(element("dt", key), element("dd", typeof value === "object" ? JSON.stringify(value) : String(value)));
      }
      root.append(line);
    }
    if (!objects.length) root.textContent = "暂无结果。";
    return root;
  }

  window.wxOperations = { loadCatalog, directorySection, element, request, operationForm, settingsEditor, renderResult };
})();
