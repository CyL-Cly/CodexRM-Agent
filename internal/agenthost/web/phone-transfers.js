"use strict";
(() => {
  const page = document.querySelector('[data-page="phone-transfers"]');
  const refreshButton = $("transfer-refresh");
  const statusLine = $("transfer-status");
  let loading = false;
  let reloadAfterCurrent = false;
  let acting = false;
  let rendered = "";
  const route = "/api/phone-transfers";

  function showStatus(message, error = false) {
    statusLine.hidden = !message;
    statusLine.textContent = message;
    statusLine.classList.toggle("transfer-error", error);
  }

  function sizeLabel(bytes) {
    if (bytes < 1024) return `${bytes} B`;
    if (bytes < 1024 ** 2) return `${(bytes / 1024).toFixed(1)} KB`;
    return `${(bytes / 1024 ** 2).toFixed(1)} MB`;
  }

  async function perform(button, work) {
    if (acting) return;
    acting = button.disabled = true;
    try {
      await work();
    } catch (error) {
      notice(error.message, true);
    } finally {
      acting = button.disabled = false;
    }
  }

  function list(kind, entries) {
    const received = kind === "inbox";
    const element = $(`transfer-${kind}`);
    const rows = document.createDocumentFragment();
    $(`transfer-${kind}-count`).textContent = `（${entries.length}）`;
    for (const entry of entries) {
      const row = document.createElement("div");
      row.className = "device";
      const info = document.createElement("div");
      info.className = "device-info";
      const name = document.createElement(received ? "button" : "strong");
      name.textContent = name.title = entry.name;
      if (received) {
        name.className = "text-button transfer-file-name";
        name.type = "button";
        name.setAttribute("aria-label", `打开 ${entry.name}`);
        name.onclick = () => perform(name, async () => {
          await api(`${route}/inbox/${encodeURIComponent(entry.id)}/open`, { method: "POST" });
        });
      }
      const meta = document.createElement("p");
      meta.className = "hint";
      const peer = received ? `来自 ${entry.sender || "已配对设备"}` : `发往 ${entry.recipient || "已配对手机"}`;
      meta.textContent = [peer, sizeLabel(entry.size), new Date(entry.created).toLocaleString()].join(" · ");
      info.append(name, meta);
      row.append(info);
      if (!received) {
        const cancel = document.createElement("button");
        cancel.className = "secondary transfer-cancel";
        cancel.type = "button";
        cancel.textContent = "取消发送";
        cancel.setAttribute("aria-label", `取消发送 ${entry.name}`);
        cancel.onclick = async () => {
          if (!confirm(`取消发送「${entry.name}」？电脑上的原文件会保留。`)) return;
          await perform(cancel, async () => {
            await api(`${route}/outbox/${encodeURIComponent(entry.bucket)}/${encodeURIComponent(entry.id)}`, { method: "DELETE" });
            notice("已取消发送。");
          });
          await refresh(false, true);
        };
        row.append(cancel);
      }
      rows.append(row);
    }
    if (!entries.length) {
      const empty = document.createElement("p");
      empty.className = "empty";
      empty.textContent = received ? "尚无接收记录。" : "没有待发送的文件。";
      rows.append(empty);
    }
    element.replaceChildren(rows);
  }

  async function refresh(showLoading = false, retryWhenBusy = false) {
    if (page.hidden || $("settings-content").hidden || acting) return;
    if (loading) {
      if (retryWhenBusy) reloadAfterCurrent = true;
      return;
    }
    loading = refreshButton.disabled = true;
    if (showLoading || !rendered) showStatus("正在读取文件记录…");
    try {
      const data = await api(route);
      const state = JSON.stringify(data);
      if (!acting && state !== rendered) {
        list("outbox", data.outbox);
        list("inbox", data.inbox);
        rendered = state;
      }
      showStatus("");
    } catch (error) {
      if (!acting) {
        for (const kind of ["outbox", "inbox"]) {
          $(`transfer-${kind}`).replaceChildren();
          $(`transfer-${kind}-count`).textContent = "";
        }
        rendered = "";
      }
      showStatus(error.message, true);
    } finally {
      loading = refreshButton.disabled = false;
      if (reloadAfterCurrent) {
        reloadAfterCurrent = false;
        void refresh();
      }
    }
  }

  refreshButton.onclick = () => refresh(true);
  document.addEventListener("settings:view", event => {
    if (event.detail === "phone-transfers") refresh();
  });
  setInterval(() => { if (!document.hidden) refresh(); }, 5000);
  void refresh();
})();
