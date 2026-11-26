(() => {
  const { loadCatalog, directorySection, element, request, operationForm, settingsEditor, renderResult } = window.wxOperations;
  const tabs = $("wx-contact-tabs");
  const body = $("wx-contact-operations");
  const settings = $("wx-contact-settings");
  const plugins = $("wx-contact-plugin-settings");
  const directory = $("wx-directory-operations");
  const directoryDetails = $("wx-directory-actions");
  const list = (label) => ({ multiline: true, list: true, label });
  const memberFields = {
    is_admin: { label: "管理员", optionalLabel: "保持当前" },
    is_blacklisted: { label: "黑名单", optionalLabel: "保持当前" },
    score_action: { label: "积分调整方式" },
    score: { label: "积分调整量" },
    temporary_score_action: { label: "临时积分调整方式" },
    temporary_score: { label: "临时积分调整量" }
  };
  let selection;
  let lastDirectory = "";
  let preferredTab = "";

  function legacyOperation() {
    return new URLSearchParams(location.search).get("wx-op") || "";
  }

  function scopeFor(account, contact) {
    const robot = window.wxContactRobot(account);
    const robotID = robot ? String(robot.id) : "";
    return {
      robot: robotID,
      account,
      contact: contact?.id,
      current() {
        const active = selectedWxAccount();
        return active?.id === account.id && active.wxid === account.wxid &&
          String(window.wxContactRobot(active)?.id || "") === robotID &&
          (!contact || selection === contact);
      }
    };
  }

  function disclosure(label, content, open = false) {
    const details = element("details", "", "wx-contact-tool");
    details.append(element("summary", label), content);
    details.open = open;
    return details;
  }

  async function fillFromCatalog(root, scope, render) {
    if (!scope.robot) {
      root.replaceChildren(element("p", "当前账号的机器人管理服务暂不可用。", "hint"));
      return;
    }
    root.replaceChildren(element("p", "正在读取功能…", "hint"));
    try {
      const catalog = await loadCatalog();
      if (scope.current() && root.isConnected) root.replaceChildren(...render(catalog));
    } catch (error) {
      if (!scope.current() || !root.isConnected) return;
      const retry = element("button", "重试", "secondary");
      retry.type = "button";
      retry.addEventListener("click", () => fillFromCatalog(root, scope, render));
      root.replaceChildren(element("p", error.message, "wx-error"), retry);
    }
  }

  function showDirectory(account) {
    const legacy = legacyOperation();
    const scope = account && scopeFor(account);
    const key = JSON.stringify([scope?.robot, account?.id, account?.wxid, legacy]);
    if (lastDirectory === key) return;
    lastDirectory = key;
    if (!scope) {
      directory.replaceChildren();
      return;
    }
    if (directorySection(legacy) === "directory") directoryDetails.open = true;
    fillFromCatalog(directory, scope, catalog => {
      const rows = [
        ["post-chat-room--create", "创建群聊", { contact_ids: list("邀请的好友 WxID") }],
        ["post-chat-room--join", "接受入群邀请", { system_message_id: { label: "邀请消息 ID" } }],
        ["post-contact--friend--search", "搜索微信用户"],
        ["post-contact--friend--add", "发送好友申请", { verify_content: { multiline: true } }],
        ["post-contact--friend--pass-verify", "通过好友申请", { system_message_id: { label: "申请消息 ID" } }]
      ];
      const messages = operationForm(catalog.get("get-system-messages"), scope, { heading: false, button: "查询系统消息" });
      return rows.map(([id, label, fields]) => disclosure(label, operationForm(catalog.get(id), scope, {
        heading: false, button: label, fields, onResult(data, status) {
          status.replaceChildren(renderResult(data));
          if (["post-chat-room--create", "post-chat-room--join", "post-contact--friend--pass-verify"].includes(id)) {
            window.wxRefreshContacts();
          }
        }
      }), legacy === id)).concat(disclosure("邀请与申请消息", messages, /--(join|pass-verify)$/.test(legacy)));
    });
  }

  function profileTools(catalog, scope, contact, group) {
    const { item, id } = contact;
    const rows = group ? [
      ["post-chat-room--announcement", "发布群公告", "content", ""],
      ["post-chat-room--name", "编辑群名称", "content", item.nickname || ""],
      ["post-chat-room--remark", "编辑群备注", "content", item.remark || ""],
      ["delete-chat-room--quit", "退出群聊"]
    ] : [
      ["post-contact--friend--remark", "编辑好友备注", "remarks", item.remark || ""],
      ["delete-contact--friend", "删除好友"]
    ];
    const facts = element("dl", "", "wx-contact-facts");
    facts.append(element("dt", group ? "群名称" : "昵称"), element("dd", item.nickname || "—"),
      element("dt", "备注"), element("dd", item.remark || "—"));
    const legacy = legacyOperation();
    const tools = rows.map(([op, label, field, initial]) => {
      const danger = op.startsWith("delete-");
      const values = group ? { chat_room_id: id } : danger ? { contact_id: id } : { to_wxid: id };
      const form = operationForm(catalog.get(op), scope, {
        heading: false, button: label, danger, values,
        initial: field ? { [field]: initial } : {},
        fields: field ? { [field]: {
          label: op.endsWith("announcement") ? "公告内容" : op.endsWith("--name") ? "群名称" : "备注",
          multiline: op.endsWith("announcement")
        } } : {},
        confirm: danger ? "确认" + label + "「" + $("wx-contact-selection").textContent + "」？" : "",
        onResult(_, status, changes) {
          if (danger) {
            window.wxClearContact(contact);
            notice(label + "成功。");
          } else {
            status.textContent = op.endsWith("announcement") ? "群公告已发布。" : "已保存。";
            if (op.endsWith("--name") || op.endsWith("--remark")) {
              const name = op.endsWith("--name");
              item[name ? "nickname" : "remark"] = changes[field];
              updateTitle(contact);
              facts.children[name ? 1 : 3].textContent = changes[field] || "—";
            }
          }
          window.wxRefreshContacts();
        }
      });
      return disclosure(label, form, legacy === op);
    });
    return [facts, ...tools];
  }

  function memberTools(catalog, scope, contact) {
    const values = { chat_room_id: contact.id };
    const listArea = element("div", "", "wx-member-list");
    const detail = element("div", "", "wx-member-detail");
    const search = element("form", "", "wx-member-search");
    const keyword = element("input");
    keyword.type = "search";
    keyword.placeholder = "昵称、备注或 WxID";
    keyword.setAttribute("aria-label", "搜索群成员");
    const submit = element("button", "搜索", "secondary");
    const sync = element("button", "同步群成员", "secondary");
    sync.type = "button";
    const includeLeft = element("input");
    includeLeft.type = "checkbox";
    const filter = element("label", "", "inline-field");
    filter.append(includeLeft, "包含已退群成员");
    const status = element("p", "", "hint");
    status.setAttribute("role", "status");
    search.append(keyword, submit, sync, filter);
    const pager = element("div", "", "form-footer");
    const prev = element("button", "上一页", "text-button");
    const next = element("button", "下一页", "text-button");
    pager.append(prev, next);
    pager.hidden = true;
    let page = 1;
    let revision = 0;
    let memberRevision = 0;

    async function loadMembers(index = 1, synchronize = false) {
      const current = ++revision;
      memberRevision++;
      listArea.replaceChildren();
      detail.replaceChildren();
      pager.hidden = true;
      status.textContent = synchronize ? "正在同步群成员…" : "正在读取群成员…";
      sync.disabled = true;
      const matches = () => current === revision && scope.current();
      try {
        if (synchronize) {
          await request(catalog.get("post-chat-room--members--sync"), scope, { ...values, id: Number(scope.robot) });
          if (!matches()) return;
        }
        const id = includeLeft.checked ? "get-chat-room--members" : "get-chat-room--not-left-members";
        const data = await request(catalog.get(id), scope, { ...values, keyword: keyword.value.trim(), page_index: index, page_size: 20 });
        if (!matches()) return;
        const items = Array.isArray(data) ? data : data?.items || [];
        page = index;
        const total = data?.total ?? items.length;
        status.textContent = items.length ? "成员 " + total + " 人" : "没有匹配的群成员。";
        listArea.replaceChildren(...items.map(row => {
          const button = element("button", "", "wx-contact");
          button.type = "button";
          button.setAttribute("aria-pressed", "false");
          button.append(element("strong", row.remark || row.nickname || row.wechat_id),
            element("span", row.is_leaved ? "已退群" : row.is_admin ? "管理员" : "", "hint"),
            element("span", row.wechat_id, "hint"));
          button.addEventListener("click", () => showMember(row, button));
          return button;
        }));
        if (includeLeft.checked && total > 20) {
          pager.hidden = false;
          prev.disabled = index <= 1;
          next.disabled = index * 20 >= total;
        }
      } catch (error) {
        if (matches()) status.textContent = error.message;
      } finally {
        if (matches()) sync.disabled = false;
      }
    }

    async function showMember(row, button) {
      for (const peer of listArea.children) peer.setAttribute("aria-pressed", String(peer === button));
      const revisionAtStart = ++memberRevision;
      const memberScope = { ...scope, current: () => scope.current() && revisionAtStart === memberRevision };
      const target = { ...values, wechat_id: row.wechat_id };
      detail.replaceChildren(element("p", "正在读取成员详情…", "hint"));
      try {
        const data = await request(catalog.get("get-chat-room--member"), memberScope, target);
        if (!memberScope.current()) return;
        const member = { ...row, ...data };
        const facts = element("dl", "", "wx-contact-facts");
        facts.append(element("dt", "管理员"), element("dd", member.is_admin ? "是" : "否"),
          element("dt", "黑名单"), element("dd", member.is_blacklisted ? "是" : "否"),
          element("dt", "积分"), element("dd", String(member.score ?? "—")),
          element("dt", "临时积分"), element("dd", String(member.temporary_score ?? "—")));
        const editor = operationForm(catalog.get("post-chat-room--member"), memberScope, {
          heading: false, button: "保存成员配置", values: { ...target, batch: false },
          fields: memberFields,
          onResult() { showMember(row, button); notice("成员配置已保存。"); }
        });
        const tools = [element("h3", member.remark || member.nickname || member.wechat_id), facts, editor];
        if (!member.is_leaved) {
          const remove = operationForm(catalog.get("delete-chat-room--members"), memberScope, {
            heading: false, button: "移出本群", danger: true,
            values: { ...values, member_ids: [row.wechat_id] },
            confirm: "确认将「" + (member.remark || member.nickname || row.wechat_id) + "」移出本群？",
            onResult() { loadMembers(); notice("已移出本群。"); }
          });
          tools.push(disclosure("移出本群", remove));
        }
        if (Number.isSafeInteger(member.id) && member.id > 0) {
          tools.push(disclosure("发送好友申请", operationForm(catalog.get("post-contact--friend--add-from-chat-room"), memberScope, {
            heading: false, values: { chat_room_member_id: member.id }
          })));
        }
        detail.replaceChildren(...tools);
      } catch (error) {
        if (memberScope.current()) detail.textContent = error.message;
      }
    }

    search.addEventListener("submit", event => { event.preventDefault(); loadMembers(); });
    includeLeft.addEventListener("change", () => loadMembers());
    sync.addEventListener("click", () => loadMembers(1, true));
    prev.addEventListener("click", () => loadMembers(page - 1));
    next.addEventListener("click", () => loadMembers(page + 1));
    const invite = operationForm(catalog.get("post-chat-room--invite"), scope, {
      heading: false, button: "发送邀请", values, fields: { contact_ids: list("好友 WxID") }
    });
    loadMembers();
    return [search, status, listArea, pager, detail, disclosure("邀请成员", invite, legacyOperation().endsWith("--invite"))];
  }

  function conversationTools(catalog, scope, contact, key, group) {
    const legacy = legacyOperation();
    if (key === "send") {
      return [...catalog.values()].filter(operation => directorySection(operation.id) === "send").map(op => {
        const values = { to_wxid: contact.id };
        if (!group) values.at = [];
        const form = operationForm(op, scope, {
          heading: false, values, fields: { content: { label: "消息内容", multiline: true }, at: list("要 @ 的群成员 WxID") }
        });
        return disclosure(op.label, form, legacy === op.id || !legacy && op.id === "post-message--send--text");
      });
    }
    const result = operationForm(catalog.get("get-chat--history"), scope, {
      heading: false, button: "查询聊天记录",
      values: { contact_id: contact.id, ...(!group && { chat_room_member: "" }) },
      initial: { page_index: 1, page_size: 20 },
      fields: { keyword: { label: "关键词" }, chat_room_member: { label: "群成员 WxID" },
        time_start: { label: "开始时间", datetime: true }, time_end: { label: "结束时间", datetime: true },
        page_index: { label: "页码" }, page_size: { label: "每页条数" } },
      onResult(data, status) {
        const rows = data?.items || [];
        const total = data?.total ?? rows.length;
        status.replaceChildren(element("p", total ? "共 " + total + " 条消息" : "暂无匹配消息。", "hint"));
        for (const row of rows) {
          const message = element("article", "", "wx-history-message");
          const time = row.created_at ? new Date(row.created_at * 1000).toLocaleString() : "";
          const sender = row.sender_nickname || row.sender_wxid || row.from_wxid;
          message.append(element("p", [sender, time].filter(Boolean).join(" · "), "hint"),
            element("p", row.display_full_content || row.content || "（消息）"));
          if (Number.isSafeInteger(row.id) && row.id > 0 && !row.is_recalled) {
            const media = ({3: "image", 34: "voice", 43: "video", 62: "video"})[row.type] ||
              (row.type === 49 && row.app_msg_type === 6 ? "file" : "");
            if (media) {
              const download = catalog.get("get-chat--" + media + "--download");
              message.append(operationForm(download, scope, { heading: false,
                values: { message_id: row.id, attach_url: row.attachment_url } }));
            }
            if ((row.sender_wxid || row.from_wxid) === scope.account.wxid) {
              message.append(operationForm(catalog.get("post-message--revoke"), scope, {
                heading: false, values: { message_id: row.id }, danger: true, confirm: "确认撤回这条消息？",
                onResult(_, result) { result.textContent = "消息已撤回。"; }
              }));
            }
          }
          status.append(message);
        }
      }
    });
    return [result];
  }

  function updateTitle(contact) {
    const item = contact?.item;
    $("wx-contact-selection").textContent = item ? item.remark || item.nickname || contact.id : "联系人详情";
    $("wx-contact-identity").textContent = item ? contact.id : "从列表选择好友或群聊。";
  }

  function select(contact) {
    if (contact === selection && contact) return;
    selection = contact;
    updateTitle(contact);
    tabs.replaceChildren();
    body.replaceChildren();
    plugins.replaceChildren();
    tabs.hidden = !contact;
    body.hidden = true;
    settings.hidden = !!contact;
    $("wx-friend-manual").hidden = !!contact;
    $("wx-friend-manual").open = false;
    if (!contact) return;
    const account = selectedWxAccount();
    if (!account) return;
    const scope = scopeFor(account, contact);
    const group = contact.id.endsWith("@chatroom");
    const sections = new Map([
      ["profile", group ? "群资料" : "好友资料"],
      ...(group ? [["members", "成员"]] : []),
      ["settings", "机器人设置"], ["history", "聊天记录"], ["send", "发送消息"]
    ]);
    const panels = new Map();
    const buttons = new Map();

    function show(key) {
      preferredTab = key;
      for (const [name, button] of buttons) button.setAttribute("aria-pressed", String(name === key));
      settings.hidden = key !== "settings";
      body.hidden = key === "settings";
      for (const [name, panel] of panels) panel.hidden = name !== key;
      if (panels.has(key)) return;
      if (key === "settings") {
        if (plugins.hasChildNodes()) return;
        fillFromCatalog(plugins, scope, catalog => {
          const [save, read] = group ? ["post-chat-room-settings", "get-chat-room-settings"] : ["post-friend-settings", "get-friend-settings"];
          const editor = settingsEditor(catalog.get(save), catalog.get(read), scope, {
            patch: true, exclude: field => field === "chat_room_id" || field === "wechat_id" ||
              field.startsWith("welcome_") || field.startsWith("leave_chat_room_alert_")
          });
          return [disclosure(group ? "本群插件设置" : "好友插件设置", editor, directorySection(legacyOperation()) === "plugins")];
        });
        return;
      }
      const panel = element("div", "", "wx-contact-pane");
      panels.set(key, panel);
      body.append(panel);
      fillFromCatalog(panel, scope, catalog => {
        if (key === "profile") return profileTools(catalog, scope, contact, group);
        if (key === "members") return memberTools(catalog, scope, contact);
        return conversationTools(catalog, scope, contact, key, group);
      });
    }

    for (const [key, label] of sections) {
      const button = element("button", label, "secondary");
      button.type = "button";
      button.addEventListener("click", () => show(key));
      buttons.set(key, button);
      tabs.append(button);
    }
    const legacy = directorySection(legacyOperation()).replace("plugins", "settings");
    show(sections.has(legacy) ? legacy : sections.has(preferredTab) ? preferredTab : "profile");
  }

  function updateDirectory() {
    if (window.settingsNavigation.view === "wx-contacts") showDirectory(selectedWxAccount());
  }
  document.addEventListener("wx:robots", updateDirectory);
  document.addEventListener("settings:view", updateDirectory);
  window.wxContactActions = { select, directory: showDirectory };
})();
