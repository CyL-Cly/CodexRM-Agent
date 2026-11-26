import { readFileSync } from "node:fs";
import vm from "node:vm";
import test from "node:test";
import assert from "node:assert/strict";

const flush = () => new Promise(setImmediate);

function loadAgenthost({ providerRequest, confirm = () => true } = {}) {
  class Element {
    constructor() {
      this.children = []; this.dataset = {}; this.value = ""; this.hidden = false;
      this.classList = { toggle() {} };
    }
    get childElementCount() { return this.children.length; }
    append(...nodes) { this.children.push(...nodes); }
    replaceChildren(...nodes) { this.children = nodes; }
    addEventListener(type, listener) { (this.listeners ||= new Map()).set(type, listener); }
    setAttribute(name, value) { this[name] = value; }
  }
  const elements = new Map();
  const document = {
    listeners: new Map(),
    addEventListener(type, listener) { this.listeners.set(type, listener); },
    getElementById(id) { if (!elements.has(id)) elements.set(id, new Element()); return elements.get(id); },
    createElement() { return new Element(); },
  };
  let interval;
  let settingsCalls = 0;
  let deviceCalls = 0;
  const settings = { server: "https://server.example", status: "已连接", version: "1.2.3", packaged: false };
  const context = vm.createContext({
    document, location: { hash: "#token" }, console, URL, Date, JSON, Error, Promise, Headers, FormData,
    window: { settingsNavigation: { init() {}, lock() {} } },
    Option: class Option { constructor(text, value) { this.text = text; this.value = value; } },
    navigator: { clipboard: { writeText: async () => {} } }, confirm,
    setInterval(callback) { interval = callback; },
    setTimeout() {}, clearTimeout() {},
    fetch: async (path, options) => {
      if (providerRequest && path.startsWith("/api/providers/")) {
        const result = await providerRequest(path.split("/").pop(), JSON.parse(options.body));
        return { ok: true, text: async () => JSON.stringify(result) };
      }
      if (path === "/api/settings") {
        settingsCalls++;
        if (settingsCalls === 1) throw new Error("temporarily unavailable");
        return { ok: true, text: async () => JSON.stringify(settings) };
      }
      if (path === "/api/wx/accounts") return { ok: true, text: async () => "[]" };
      if (path === "/api/devices") {
        deviceCalls++;
        return { ok: true, text: async () => JSON.stringify([{ clientId: "browser", clientName: "Browser", deviceType: "desktop" }]) };
      }
      throw new Error(`unexpected ${path}`);
    },
  });
  document.getElementById("login-form").hidden = true;
  for (const id of ["server", "version", "status", "devices"]) document.getElementById(id);
  vm.runInContext(readFileSync(new URL("./web/app.js", import.meta.url), "utf8"), context);
  return { context, document, elements, settings, poll: () => interval(), counts: () => ({ settingsCalls, deviceCalls }) };
}

test("agenthost polling after the first settings failure applies settings and restores devices", async () => {
  const harness = loadAgenthost();
  await flush();
  assert.equal(harness.elements.get("notice").hidden, false);
  assert.equal(harness.elements.get("server").value, "");
  await harness.poll();
  assert.equal(harness.elements.get("server").value, harness.settings.server);
  assert.equal(harness.elements.get("allow-file-management").checked, false);
  assert.equal(harness.elements.get("version").textContent, "1.2.3");
  assert.equal(harness.elements.get("status").textContent, "已连接");
  assert.equal(harness.elements.get("devices").children.length, 1);
  assert.equal(harness.elements.get("devices").children[0].children[1].children[0].textContent, "Browser");
  assert.deepEqual(harness.counts(), { settingsCalls: 2, deviceCalls: 1 });
  assert.equal(vm.runInContext("saved.server", harness.context), harness.settings.server);
});


test("agenthost provider deletion confirms the exact target and keeps the row until success", async () => {
  const providers = [{ id: "", name: "当前 Codex 配置" }, { id: "a", name: "同名供应商" }, { id: "b", name: "同名供应商" }];
  const confirmations = [];
  const deletions = [];
  let accept = false;
  let fail = true;
  const { context, document, elements } = loadAgenthost({
    confirm(message) {
      confirmations.push(message);
      return accept;
    },
    async providerRequest(operation, params) {
      if (operation === "catalog") return { providers };
      assert.equal(operation, "delete");
      deletions.push(params);
      if (fail) throw new Error("无法保存供应商设置");
      providers.splice(providers.findIndex(provider => provider.id === params.id), 1);
      return { deleted: true };
    },
  });
  vm.runInContext(readFileSync(new URL("./web/providers.js", import.meta.url), "utf8"), context);
  document.listeners.get("settings:view")({ detail: "providers" });
  await flush();
  const content = elements.get("provider-content");
  const click = button => button.listeners.get("click")();
  const manage = index => click(content.children[index + 1].children[1]);
  const deleteButton = () => content.children[0].children.find(button => button.textContent === "删除供应商");
  manage(0);
  assert.equal(deleteButton(), undefined);
  click(elements.get("provider-back"));
  await flush();
  manage(2);
  const remove = deleteButton();
  click(remove);
  assert.match(confirmations[0], /删除「同名供应商」/);
  assert.deepEqual(deletions, []);

  accept = true;
  click(remove);
  click(remove);
  await flush();
  assert.deepEqual(deletions, [{ id: "b" }]);
  assert.equal(elements.get("provider-status").textContent, "无法保存供应商设置");
  assert.equal(deleteButton(), remove);

  fail = false;
  click(remove);
  await flush();
  assert.deepEqual(deletions, [{ id: "b" }, { id: "b" }]);
  assert.equal(elements.get("provider-title").textContent, "供应商列表");
  assert.equal(elements.get("provider-status").textContent, "供应商已删除");
  assert.deepEqual(content.children.slice(1).map(row => row.children[0].children[0].textContent), ["当前 Codex 配置", "同名供应商"]);
});
