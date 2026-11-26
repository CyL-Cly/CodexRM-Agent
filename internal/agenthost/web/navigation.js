"use strict";
(() => {
  const byId = id => document.getElementById(id);
  const nav = byId("settings-nav");
  const menu = byId("nav-toggle");
  const scrim = byId("nav-scrim");
  const mobile = matchMedia("(max-width: 760px)");
  const links = [...nav.querySelectorAll("[data-route]")];
  const pages = [...document.querySelectorAll("[data-page]")];
  const accountViews = new Set(["wx-contacts", "wx-commands"]);
  const robotViews = new Set(["wx-robots", "wx-operations"]);
  let view = "overview";
  let wxReady = false;

  function setMenu(open, restoreFocus = false) {
    nav.classList.toggle("is-open", open);
    menu.setAttribute("aria-expanded", String(open));
    menu.lastElementChild.textContent = open ? "关闭导航" : "打开导航";
    scrim.hidden = !open;
    document.body.classList.toggle("nav-open", open);
    if (open) {
      const current = nav.querySelector('[aria-current="page"]:not([hidden] *)');
      (current || nav.querySelector('[data-route="' + view + '"]'))?.focus();
    }
    else if (restoreFocus) menu.focus();
  }

  function syncWxScope() {
    const needsAccount = wxReady && accountViews.has(view);
    byId("wx-robot-scope").hidden = !wxReady || !robotViews.has(view);
    byId("wx-account-scope").hidden = !needsAccount;
    byId("wx-account-required").hidden = !needsAccount || !!byId("wx-account").value;
    byId("wx-unavailable").hidden = !view.startsWith("wx-") || view === "wx-overview" || wxReady;
  }

  function showView(focus = false) {
    const requested = new URL(location.href).searchParams.get("view") || "overview";
    const current = links.find(link => link.dataset.route === requested && !link.closest("[hidden]")) || links[0];
    view = current.dataset.route;
    for (const link of links) {
      if (link === current) link.setAttribute("aria-current", "page");
      else link.removeAttribute("aria-current");
    }
    for (const page of pages) page.hidden = page.dataset.page !== view;
    byId("wx-card").hidden = !view.startsWith("wx-");
    const title = current.lastChild.textContent.trim();
    const heading = byId("view-title");
    heading.textContent = title;
    byId("view-crumb").textContent = title;
    document.title = title + " · CodexRM";
    syncWxScope();
    if (focus) {
      heading.focus({ preventScroll: true });
      window.scrollTo(0, 0);
    }
    document.dispatchEvent(new CustomEvent("settings:view", { detail: view }));
  }

  document.addEventListener("click", event => {
    const link = event.target.closest("a[data-route]");
    if (!link || event.defaultPrevented || event.button || event.ctrlKey || event.metaKey || event.shiftKey || event.altKey) return;
    event.preventDefault();
    const url = new URL(location.href);
    const target = new URL(link.href);
    for (const key of ["view", "wx-module", "wx-op"]) {
      const value = target.searchParams.get(key);
      if (value && !(key === "view" && value === "overview")) url.searchParams.set(key, value);
      else url.searchParams.delete(key);
    }
    if (url.href !== location.href) history.pushState(null, "", url);
    setMenu(false);
    showView(true);
  });
  window.addEventListener("popstate", () => {
    if (!nav.hidden) showView(true);
  });
  mobile.addEventListener("change", () => setMenu(false));
  menu.addEventListener("click", () => {
    const opening = menu.getAttribute("aria-expanded") !== "true";
    setMenu(opening, !opening);
  });
  scrim.addEventListener("click", () => setMenu(false, true));
  document.addEventListener("keydown", event => {
    if (event.key === "Escape" && !scrim.hidden) setMenu(false, true);
  });
  byId("wx-account").addEventListener("change", syncWxScope);
  window.settingsNavigation = {
    init(data) {
      byId("nav-wx").hidden = data.wxSupported !== true;
      byId("overview-wx").hidden = data.wxSupported !== true;
      byId("nav-onebot").hidden = data.oneBotSupported !== true;
      byId("overview-onebot").hidden = data.oneBotSupported !== true;
      byId("nav-phone-transfers").hidden = data.phoneTransfersSupported !== true;
      byId("nav-updates").hidden = !["windows-agent", "linux-agent", "linux-manual"].includes(data.updateMode);
      nav.hidden = menu.hidden = false;
      document.body.classList.add("has-settings");
      showView();
    },
    lock() {
      nav.hidden = menu.hidden = true;
      document.body.classList.remove("has-settings");
      document.title = "Agent 设置 · CodexRM";
      byId("view-crumb").textContent = "Agent 设置";
      setMenu(false);
    },
    wxReady(ready) {
      wxReady = ready;
      syncWxScope();
    },
    openContacts(operation) {
      const url = new URL(location.href);
      url.searchParams.set("view", "wx-contacts");
      url.searchParams.delete("wx-module");
      if (operation) url.searchParams.set("wx-op", operation);
      else url.searchParams.delete("wx-op");
      history.replaceState(null, "", url);
      showView();
    },
    syncWxScope,
    get view() { return view; }
  };
})();
