"use strict";
(() => {
  const content = $("provider-content");
  const back = $("provider-back");
  const status = $("provider-status");
  let view = "list";
  let opened = false;
  let loading = false;
  let groups = [];
  let selected = null;
  const changes = new Map();

  const call = (operation, params = {}) => api(`/api/providers/${operation}`, {
    method: "POST", body: JSON.stringify(params),
  });
  function element(tag, text, className) {
    const node = document.createElement(tag);
    if (text) node.textContent = text;
    if (className) node.className = className;
    return node;
  }
  function button(label, run, kind = "secondary") {
    const node = element("button", label, kind);
    node.type = "button";
    node.addEventListener("click", run);
    return node;
  }
  function feedback(message = "", error = false) {
    status.textContent = message;
    status.classList.toggle("provider-error", error);
  }
  async function task(work) {
    if (loading) return;
    loading = true;
    back.disabled = content.inert = true;
    feedback("正在处理…");
    try {
      feedback(await work());
    } catch (error) {
      feedback(error.message, true);
    } finally {
      back.disabled = content.inert = loading = false;
    }
  }
  function page(name, title, label) {
    view = name;
    $("provider-title").textContent = title;
    back.textContent = label;
    back.hidden = name === "list";
    content.replaceChildren();
  }
  async function catalog() {
    const result = await call("catalog");
    groups = result.providers || [];
    return (result.warnings || []).join(" · ");
  }
  function list() {
    page("list", "供应商列表", "返回");
    const actions = element("div", "", "provider-actions");
    actions.append(button("添加供应商", () => editor("", { local: true })),
      button("刷新上游", () => task(async () => { const warning = await catalog(); list(); return warning; })),
      button("从 CC Switch 导入", () => task(async () => {
        const result = await call("import");
        await catalog();
        list();
        return result.count ? `已导入 ${result.count} 个供应商` : "没有可导入的新供应商";
      })));
    content.append(actions);
    for (const provider of groups) {
      const row = element("div", "", "provider-row");
      const info = element("div");
      info.append(element("strong", provider.name), element("p", provider.source, "hint"));
      row.append(info, button("管理模型", () => {
        selected = provider;
        changes.clear();
        feedback();
        models();
      }));
      content.append(row);
    }
  }
  function modelRows() {
    const entries = new Map();
    for (const model of selected.models || []) {
      const id = model.model || model.id;
      if (id && !entries.has(id)) entries.set(id, !!model.blacklisted);
    }
    for (const [id, action] of changes) entries.set(id, action === "hide");
    return [...entries].sort((a, b) => Number(a[1]) - Number(b[1]));
  }
  function models() {
    page("models", selected.name, changes.size ? "保存并返回" : "返回");
    const actions = element("div", "", "provider-actions");
    if (selected.id) actions.append(button("编辑接入配置", () => task(async () => {
      await saveModels();
      editor(selected.id, await call("details", { id: selected.id }));
    })));
    if (selected.id) actions.append(button("删除供应商", deleteProvider));
    content.append(actions);
    if (selected.defaultModel) content.append(element("p", `默认模型：${selected.defaultModel}`, "hint"));
    if (selected.warning) content.append(element("p", selected.warning, "hint"));
    const form = element("form", "", "provider-actions provider-add");
    const input = element("input");
    input.placeholder = "输入模型 ID";
    input.setAttribute("aria-label", "模型 ID");
    input.maxLength = 256;
    input.required = true;
    const add = element("button", "添加模型", "secondary");
    add.type = "submit";
    form.append(input, add);
    form.addEventListener("submit", event => {
      event.preventDefault();
      const id = input.value.trim();
      if (!id) return;
      const existing = modelRows().find(([name]) => name === id);
      if (existing && !existing[1]) return feedback("此模型已在列表中");
      changes.set(id, existing ? "restore" : "add");
      feedback("返回时一次性保存模型改动");
      models();
      content.querySelector("input").focus();
    });
    content.append(form);
    const rows = modelRows();
    if (!rows.length) content.append(element("p", "暂无模型，可输入模型 ID 手动添加。", "hint"));
    for (const [id, hidden] of rows) {
      const row = element("div", "", "provider-row");
      row.append(element(hidden ? "s" : "span", id), button(hidden ? "恢复" : "移除", () => {
        if (hidden && !(selected.models || []).some(model => (model.model || model.id) === id)) {
          changes.set(id, "add");
        } else {
          changes.set(id, hidden ? "restore" : "hide");
        }
        feedback("返回时一次性保存模型改动");
        models();
      }, "text-button"));
      content.append(row);
    }
  }
  function deleteProvider() {
    if (loading || !selected?.id) return;
    const { id, name } = selected;
    const unsaved = changes.size ? "\n此供应商未保存的模型改动将被丢弃。" : "";
    if (!confirm(`删除「${name}」？\n仅从此 Agent 删除，不影响 CC Switch、Codex 登录或聊天记录。使用此供应商的会话可能需要重新选择供应商。${unsaved}`)) return;
    void task(async () => {
      const result = await call("delete", { id });
      if (result?.deleted !== true) throw new Error("删除未完成，请检查 Agent 版本或重试");
      groups = groups.filter(provider => provider.id !== id);
      selected = null;
      changes.clear();
      list();
      try {
        await catalog();
        list();
      } catch (error) {
        return `供应商已删除，但刷新列表失败：${error.message}`;
      }
      return "供应商已删除";
    });
  }
  async function saveModels() {
    if (!changes.size) return;
    await call("models", { id: selected.id, changes: [...changes].map(([model, action]) => ({ model, action })) });
    selected.models = modelRows().map(([model, blacklisted]) => ({ model, blacklisted }));
    changes.clear();
    models();
  }
  function editor(id, details) {
    page("editor", id ? "编辑供应商" : "添加供应商", "返回");
    feedback();
    const form = element("form");
    function field(name, label, value = "", type = "text") {
      const input = element("input");
      input.id = `provider-field-${name}`;
      input.type = type;
      input.value = value;
      input.autocomplete = "off";
      const caption = element("label", label);
      caption.htmlFor = input.id;
      form.append(caption, input);
      return input;
    }
    if (id) form.append(element("p", details.local ? "保存在此 Agent" : "保存在 CC Switch，可在供应商列表导入到 Agent。", "hint"));
    const name = field("name", "名称", details.name);
    name.required = true;
    name.maxLength = 128;
    const notes = field("notes", "备注", details.notes);
    let url, key;
    if (!details.official) {
      url = field("url", "接入地址", details.baseUrl, "url");
      url.placeholder = "https://api.example.com/v1";
      url.required = !id;
      key = field("key", "API Key", "", "password");
      key.autocomplete = "new-password";
      key.placeholder = id ? "留空保留现有密钥" : "可选";
    }
    const model = field("model", "默认模型 ID", details.defaultModel);
    model.placeholder = "可选";
    model.maxLength = 256;
    const actions = element("div", "", "provider-actions");
    const save = element("button", "保存配置", "primary");
    save.type = "submit";
    actions.append(save);
    form.append(actions);
    form.addEventListener("submit", event => {
      event.preventDefault();
      void task(async () => {
        const patch = { id, name: name.value.trim(), notes: notes.value, defaultModel: model.value.trim() };
        if (url?.value.trim() && url.value.trim() !== details.baseUrl) patch.baseUrl = url.value.trim();
        if (key?.value.trim()) patch.apiKey = key.value.trim();
        const result = await call("save", patch);
        if (key) key.value = "";
        list();
        await catalog();
        selected = groups.find(provider => provider.id === result.id);
        if (selected) models(); else list();
        return "供应商配置已保存";
      });
    });
    content.append(form);
  }
  function goBack() {
    if (loading) return;
    if (view === "list") return;
    if (view === "editor") {
      if (selected) models(); else list();
      feedback();
    } else {
      void task(async () => { await saveModels(); selected = null; list(); });
    }
  }
  document.addEventListener("settings:view", event => {
    if (event.detail !== "providers" || opened) return;
    opened = true;
    list();
    void task(async () => { const warning = await catalog(); list(); return warning; });
  });
  back.addEventListener("click", goBack);
})();
