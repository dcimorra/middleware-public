// Chat with the agent that works inside this site's folder. Turns come
// from GET /chat with their steps (words and tools, in order); while a
// turn runs, the same arrives on the event stream as agent.* events with
// the domain as subject: agent.text, agent.tool when a tool starts,
// agent.tool.result when it answers, agent.subagent when a subagent
// changes state, and the end. What a subagent does goes to its own tab,
// as in V1, out of the way of what the agent itself answers.

import { icon } from "../lib/icons.js";
import { h, clear, debounce } from "../lib/dom.js";
import { t } from "../lib/i18n.js";
import { api, enc } from "../lib/api.js";
import { store } from "../lib/store.js";
import { onEvent } from "../lib/stream.js";
import { loading, errorBox } from "../components/page.js";
import { confirm, open } from "../components/dialog.js";
import { toast, toastError } from "../components/toast.js";
import { render as renderMd } from "../lib/markdown.js";
import { usageLine } from "../components/usage.js";

// chatTab is a site's chat. For the house's own conversation, domain is
// "middleware": no other conversations, no site manifest to write the
// provider into.
export function chatTab(domain) {
  const house = domain === "middleware";
  const turns = h("div", { class: "chat-turns", "aria-live": "polite" });
  const text = h("textarea", { placeholder: t("chat.placeholder"), "aria-label": t("chat.message") });
  const provider = h("select", { "aria-label": t("chat.provider") });
  // The model: a list while it is short, a box that searches as you type
  // once a provider brings hundreds (OpenCode with OpenRouter connected).
  const modelSel = h("select", { "aria-label": t("chat.model") });
  const modelListID = "chat-models-" + Math.random().toString(36).slice(2, 8);
  const modelIn = h("input", { type: "text", list: modelListID, class: "model-input", placeholder: t("chat.defaultModel"), "aria-label": t("chat.model"), autocomplete: "off", spellcheck: "false", hidden: true });
  const modelList = h("datalist", { id: modelListID });
  let manyModels = false;
  const model = {
    get value() { return manyModels ? modelIn.value.trim() : modelSel.value; },
    set value(v) { modelSel.value = v; modelIn.value = v; },
  };
  const send = h("button", { type: "submit", class: "btn primary" }, icon("send"), t("chat.send"));
  const stop = h("button", { type: "button", class: "btn danger", hidden: true }, icon("stop"), t("chat.stop"));
  const reset = h("button", { type: "button", class: "btn ghost sm" }, icon("sparkles"), t("chat.new"));
  // What the conversation left running, setsid escapes included: listed
  // from the environment mark every turn carries, and killed one by one or
  // all at once. Otherwise they live for days and nobody sees them.
  const procsBtn = h("button", { type: "button", class: "btn ghost sm" }, icon("list"), t("chat.processes"));
  const status = h("span", { class: "small muted", role: "status" });
  const capacity = h("span", { class: "small muted chat-capacity", title: t("chat.capacityTitle") });
  // Attachments go with the next message; they are kept in the site's own
  // HOME and lent to the turn read-only.
  const fileInput = h("input", { type: "file", multiple: true, hidden: true });
  const attachBtn = h("button", { type: "button", class: "btn ghost sm", title: t("chat.attachHint") }, icon("plus"), t("chat.attach"));
  const chips = h("div", { class: "chat-chips row" });
  // The usage windows of the agent picked, under the box.
  let usageEl = usageLine(null);
  const form = h("form", { class: "chat-form" }, chips, text, h("div", { class: "row" }, provider, modelSel, modelIn, modelList, attachBtn, fileInput, h("span", { class: "grow" }), status, procsBtn, reset, stop, send), usageEl);
  // Which conversation is open: the site's first one, another of the
  // panel's, or one the site's code opened through the site door (read
  // and stopped from here, never written into: a program is on the other
  // side, and its turns follow its own rules).
  const base = `/sites/${enc(domain)}`;
  // Where the conversations beyond the first live: a site's, or the house's.
  const chatsBase = house ? "/chat/middleware" : base;
  const mainConv = () => ({ name: domain, path: house ? "/chat/middleware" : `${base}/chat`, origin: "panel" });
  const convOf = (id, origin) => id ? { name: `${domain}#${id}`, path: `${chatsBase}/chats/${enc(id)}`, origin: origin || "panel" } : mainConv();
  // A new conversation is a draft until its first message (or file) goes:
  // it has no name to list yet, and the title comes from that message.
  const draftConv = () => ({ draft: true, name: null, path: null, origin: "panel" });
  let cur = mainConv();
  const picker = h("select", { "aria-label": t("chat.conversation") });
  const newConv = h("button", { type: "button", class: "btn ghost sm" }, icon("plus"), t("chat.newConversation"));
  const purge = h("button", { type: "button", class: "btn ghost sm", title: t("chat.purgeHint") }, icon("trash"), t("chat.purge"));
  const readonlyNote = h("div", { class: "small muted", hidden: true }, t("chat.siteDoorNote"));
  const idOf = (c) => (c.draft ? "__draft" : c.name === domain ? "" : c.name.split("#")[1]);
  // The list, the last used first; the first time, the last used one is
  // what opens.
  async function fillPicker(first) {
    let list = [];
    try { list = await api.get(`${chatsBase}/chats?main=1`); } catch { list = []; }
    if (first && list.length) cur = convOf(list[0].id, list[0].origin);
    clear(picker);
    const curID = idOf(cur);
    if (!list.some((c) => c.id === curID)) picker.append(h("option", { value: curID, "data-origin": cur.origin }, t("chat.newDraft")));
    for (const c of list) {
      const label = (c.origin === "site" ? t("chat.fromSite") + " · " : "") + (c.title || t("chat.untitled")) + (c.running ? " ●" : "");
      picker.append(h("option", { value: c.id, "data-origin": c.origin, title: c.updated ? new Date(c.updated).toLocaleString() : "" }, label));
    }
    picker.value = curID;
    purge.hidden = list.length === 0;
    applyOrigin();
  }
  function applyOrigin() {
    form.hidden = cur.origin === "site";
    readonlyNote.hidden = cur.origin !== "site";
  }
  picker.addEventListener("change", () => {
    const id = picker.value;
    if (id === "__draft") return;
    cur = convOf(id, picker.selectedOptions[0].dataset.origin);
    applyOrigin();
    loadTurns().then(showUploads);
  });
  newConv.addEventListener("click", async () => {
    if (cur.draft) { text.focus(); return; }
    cur = draftConv();
    await fillPicker();
    loadTurns().then(showUploads);
    text.focus();
  });
  purge.addEventListener("click", async () => {
    if (!(await confirm({ title: t(house ? "chat.purgeQHouse" : "chat.purgeQ", { name: domain }), body: t("chat.purgeBody"), confirmLabel: t("chat.purge"), danger: true }))) return;
    try {
      const r = await api.del(`${chatsBase}/chats`);
      toast(t("chat.purged", { n: (r && r.purged) || 0 }), "ok");
      cur = mainConv();
      await fillPicker();
      loadTurns().then(showUploads);
    } catch (e) { toastError(e); }
  });
  // ensure turns a draft into a conversation, when something is sent.
  async function ensure() {
    if (!cur.draft) return;
    const r = await api.post(`${chatsBase}/chats`, {});
    cur = convOf(r.id, "panel");
  }
  // One tab per subagent of the last turn (or opened from an older one),
  // beside the main conversation.
  const tabs = h("div", { class: "chat-tabs", role: "tablist", hidden: true });
  const views = h("div", { class: "chat-views" }, turns);
  const el = h("div", { class: "chat" }, h("div", { class: "chat-head row" }, picker, newConv, purge, h("span", { class: "grow" }), capacity), readonlyNote, tabs, views, form);
  let agents = [];
  let running = null; // {turnEl, textEl, textBuf}
  const subs = [];

  // The "3 of 8 running" indicator is house-wide, not this site's: it is
  // what tells the operator why a send was refused. Events move it at
  // once and a debounced read of /agents/capacity corrects any drift (a
  // turn that ended while the stream was reconnecting).
  const cap = { used: 0, max: 0 };
  function showCapacity() {
    capacity.textContent = cap.max ? t("chat.capacity", { used: cap.used, max: cap.max }) : "";
    capacity.classList.toggle("full", cap.max > 0 && cap.used >= cap.max);
  }
  const syncCapacity = debounce(async () => {
    try {
      const c = await api.get("/agents/capacity");
      cap.used = c.used; cap.max = c.max;
      showCapacity();
    } catch { /* the indicator keeps what the events said */ }
  }, 300);
  subs.push(onEvent("agent.", (ev) => {
    const d = ev.data || {};
    if (ev.type === "agent.started") cap.used++;
    else if (ev.type === "agent.done" || (ev.type === "agent.error" && d.turn_id && ev.level === "error")) cap.used = Math.max(0, cap.used - 1);
    else if (ev.type === "agent.capacity") { if (d.used !== undefined) cap.used = d.used; if (d.max !== undefined) cap.max = d.max; }
    else return;
    showCapacity();
    syncCapacity();
  }));

  // --- usage -------------------------------------------------------------

  // What the agent picked has spent of its subscription's windows. Read
  // when the agent changes; told by the bus when a figure moves (Claude
  // says it on every turn's stream); and asked every minute while a turn
  // of this conversation runs, for the agents whose stream says nothing
  // (Codex, Antigravity): a long turn moves the window with nobody typing.
  // The server keeps the vendor's answer a minute, so tabs open on the
  // same agent ask it once.
  function showUsage(st) {
    const n = usageLine(st);
    usageEl.replaceWith(n);
    usageEl = n;
  }
  async function loadUsage() {
    const p = provider.value;
    if (!p) { showUsage(null); return; }
    try {
      const list = await api.get(`/agents/usage?provider=${enc(p)}`);
      if (provider.value === p) showUsage((list || []).find((x) => x.provider === p) || null);
    } catch { /* the line keeps what it showed */ }
  }
  let usageTimer = null;
  function followUsage(on) {
    clearInterval(usageTimer);
    usageTimer = on ? setInterval(loadUsage, 60000) : null;
  }
  subs.push(onEvent("agent.usage", (ev) => {
    const d = ev.data || {};
    if (d.provider && d.provider === provider.value && d.usage) showUsage({ provider: d.provider, usage: d.usage });
  }));
  subs.push(() => followUsage(false));

  // --- steps -------------------------------------------------------------

  // What each tool did, said as a person would: "Reads …/src/app.js".
  function toolSummary(name, input) {
    const s = (k) => (input && typeof input[k] === "string" ? input[k] : "");
    const short = (p) => { const parts = p.split("/"); return parts.length <= 3 ? p : "…/" + parts.slice(-2).join("/"); };
    const oneLine = (x, n) => { const l = x.replace(/\s+/g, " ").trim(); return l.length > n ? l.slice(0, n) + "…" : l; };
    switch (name) {
      case "Read": return t("chat.do.read", { path: short(s("file_path")) });
      case "Write": return t("chat.do.write", { path: short(s("file_path")) });
      case "Edit": case "MultiEdit": return t("chat.do.edit", { path: short(s("file_path")) });
      case "NotebookEdit": return t("chat.do.edit", { path: short(s("notebook_path")) });
      case "Bash": return t("chat.do.bash", { cmd: oneLine(s("command"), 120) });
      case "BashOutput": return t("chat.do.bashOutput");
      case "KillShell": case "KillBash": return t("chat.do.kill");
      case "Glob": return t("chat.do.glob", { pattern: s("pattern") });
      case "Grep": return t("chat.do.grep", { pattern: oneLine(s("pattern"), 60) });
      case "WebFetch": return t("chat.do.fetch", { url: s("url") });
      case "WebSearch": return t("chat.do.search", { query: oneLine(s("query"), 60) });
      case "TodoWrite": return t("chat.do.todo");
      case "Task": case "Agent": return t("chat.do.agent", { what: oneLine(s("description"), 60) });
      case "Skill": return t("chat.do.skill", { skill: s("skill") || s("command") });
      default: return t("chat.do.other", { name });
    }
  }

  // The body of a tool's fold: what it was given, the way it reads best,
  // then what it answered.
  function toolBody(tool) {
    const input = tool.input || {};
    const parts = [];
    if (tool.name === "Bash" && input.command) parts.push(h("pre", { class: "cmd" }, "$ " + input.command));
    else if ((tool.name === "Edit") && (input.old_string || input.new_string)) {
      parts.push(h("pre", { class: "diff" },
        ...String(input.old_string || "").split("\n").map((l) => h("span", { class: "del" }, "- " + l + "\n")),
        ...String(input.new_string || "").split("\n").map((l) => h("span", { class: "add" }, "+ " + l + "\n"))));
    } else if (tool.name === "TodoWrite" && Array.isArray(input.todos)) {
      parts.push(h("ul", { class: "todos" }, ...input.todos.map((td) => h("li", { class: td.status }, (td.status === "completed" ? "☑ " : td.status === "in_progress" ? "◐ " : "☐ ") + (td.content || td.activeForm || "")))));
    } else if (tool.name === "Write" && input.content) {
      parts.push(h("pre", null, input.content));
    } else if (Object.keys(input).length && !["Task", "Agent"].includes(tool.name)) {
      parts.push(h("pre", null, JSON.stringify(input, null, 2)));
    }
    const out = h("pre", { class: "out" + (tool.error ? " err" : "") }, tool.output || "");
    out.hidden = !tool.output;
    parts.push(out);
    return { parts, out };
  }

  const RUN_DONE = new Set(["completed", "done"]);
  function runClass(run) {
    if (!run || run === "running") return "live";
    return RUN_DONE.has(run) ? "ok" : "bad";
  }
  function runLabel(run) {
    const k = "chat.run." + (run || "running");
    const s = t(k);
    return s === k ? run : s;
  }

  // toolEls finds a tool's row by its call id, to put its answer on it.
  let toolEls = new Map();
  function toolStepEl(tool, threadData) {
    const state = h("span", { class: "tool-state " + (tool.done ? (tool.error ? "err" : "ok") : "live"), "aria-hidden": "true" });
    const summary = h("summary", null, state, h("span", { class: "tool-what" }, toolSummary(tool.name, tool.input)));
    const body = toolBody(tool);
    const det = h("details", { class: "chat-tool" }, summary, ...body.parts);
    const row = { det, state, out: body.out, tool };
    if (tool.run) {
      const badge = h("span", { class: "badge run " + runClass(tool.run) }, runLabel(tool.run));
      const see = h("button", { type: "button", class: "btn sm ghost" }, t("chat.seeAgent"));
      see.addEventListener("click", (e) => {
        e.preventDefault(); e.stopPropagation();
        agentView(threadData || { id: tool.id, description: (tool.input || {}).description, prompt: (tool.input || {}).prompt, run: tool.run });
        renderTabs();
        showTab(tool.id);
      });
      summary.append(badge, see);
      row.badge = badge;
    }
    if (tool.id) toolEls.set(tool.id, row);
    return det;
  }
  function settleTool(id, d) {
    const row = toolEls.get(id);
    if (!row) return;
    if (d.output !== undefined) { row.out.textContent = d.output; row.out.hidden = !d.output; }
    row.out.classList.toggle("err", Boolean(d.error));
    row.state.className = "tool-state " + (d.error ? "err" : "ok");
    if (d.run && row.badge) { row.badge.className = "badge run " + runClass(d.run); row.badge.textContent = runLabel(d.run); }
  }

  function textStepEl(text) {
    const md = renderMd(stripQuestions(text || ""), { breaks: true }).el;
    md.classList.add("chat-md");
    return md;
  }

  function stepsEl(steps, threads) {
    const box = h("div", { class: "steps" });
    const byId = new Map((threads || []).map((th) => [th.id, th]));
    for (const st of steps || []) {
      if (st.kind === "text") box.append(textStepEl(st.text));
      else if (st.tool) box.append(toolStepEl(st.tool, byId.get(st.tool.id)));
    }
    return box;
  }

  // --- subagent tabs --------------------------------------------------------

  let agentsUI = new Map();
  let activeTab = "";
  function agentView(th) {
    let a = agentsUI.get(th.id);
    if (a) {
      if (th.description && !a.description) { a.description = th.description; a.title.textContent = th.description; }
      if (th.run) setAgentRun(th.id, th.run);
      return a;
    }
    const title = h("strong", null, th.description || t("chat.subagent"));
    const badge = h("span", { class: "badge run " + runClass(th.run) }, runLabel(th.run));
    const body = h("div", { class: "steps" });
    const foot = h("div", { class: "small muted agent-foot" });
    const view = h("div", { class: "chat-agent", hidden: true }, ...[
      h("div", { class: "row" }, title, badge),
      th.prompt ? h("details", { class: "chat-tool brief" }, h("summary", null, t("chat.brief")), textStepEl(th.prompt)) : null,
      body, foot].filter(Boolean));
    if (th.steps) for (const st of th.steps) body.append(st.kind === "text" ? textStepEl(st.text) : toolStepEl(st.tool));
    views.append(view);
    a = { id: th.id, description: th.description, run: th.run || "running", view, body, badge, title, foot };
    agentsUI.set(th.id, a);
    setAgentRun(th.id, a.run);
    return a;
  }
  function setAgentRun(id, run) {
    const a = agentsUI.get(id);
    if (!a || !run) return;
    a.run = run;
    a.badge.className = "badge run " + runClass(run);
    a.badge.textContent = runLabel(run);
    a.foot.textContent = run === "running" ? t("chat.agentWorking") : t("chat.agentEnded", { state: runLabel(run) });
    a.foot.classList.toggle("typing", run === "running");
    renderTabs();
  }
  function renderTabs() {
    tabs.hidden = agentsUI.size === 0;
    if (tabs.hidden) return;
    const live = [...agentsUI.values()].filter((a) => a.run === "running").length;
    const tab = (id, label, cls) => {
      const b = h("button", { type: "button", role: "tab", class: "chat-tab" + (activeTab === id ? " on" : ""), "aria-selected": String(activeTab === id) }, cls ? h("span", { class: "dot " + cls }) : null, label);
      b.addEventListener("click", () => showTab(id));
      return b;
    };
    clear(tabs).append(...[tab("", t("chat.mainTab")), ...[...agentsUI.values()].map((a) => tab(a.id, a.description || t("chat.subagent"), runClass(a.run))),
      live ? h("span", { class: "small muted live-count" }, t("chat.agentsRunning", { n: live })) : null].filter(Boolean));
  }
  function showTab(id) {
    if (id && !agentsUI.has(id)) id = "";
    activeTab = id;
    turns.hidden = id !== "";
    for (const a of agentsUI.values()) a.view.hidden = a.id !== id;
    renderTabs();
  }
  function resetAgents() {
    for (const a of agentsUI.values()) a.view.remove();
    agentsUI = new Map();
    toolEls = new Map();
  }

  function turnEl(role, body, cost, at) {
    const who = h("div", { class: "who" }, role === "user" ? t("chat.you") : t("chat.agent"), at ? h("span", { class: "muted" }, new Date(at).toLocaleTimeString()) : null, cost ? h("span", { class: "cost" }, fmtCost(cost)) : null);
    const textEl = role === "user" ? h("div", { class: "text" }, body || "") : h("div", { class: "steps" });
    if (role !== "user" && body) textEl.append(textStepEl(body));
    const el = h("div", { class: "turn " + role }, who, textEl);
    return { el, textEl, who };
  }

  // A question block is shown as its buttons, not as the JSON the agent
  // wrote; the text around it stays.
  function stripQuestions(s) {
    return s.replace(/^[ \t]*```question[ \t]*\n[\s\S]*?\n[ \t]*```[ \t]*$/gm, "").replace(/\n{3,}/g, "\n\n").trim();
  }

  // The buttons answer by sending the option as the next message; the
  // last one opens the box. Once one is pressed the group is disabled so
  // the same question is not answered twice by accident. No options means
  // the agent wants words, so only the box is offered.
  function questionEl(q) {
    const group = h("div", { class: "chat-question", role: "group", "aria-label": q.question });
    group.append(h("div", { class: "q" }, q.question));
    const row = h("div", { class: "row" });
    const settle = () => { for (const b of row.querySelectorAll("button")) b.disabled = true; };
    for (const opt of q.options || []) {
      const label = opt === q.recommended ? t("chat.recommended", { option: opt }) : opt;
      const b = h("button", { type: "button", class: "btn sm" + (opt === q.recommended ? " primary" : "") }, label);
      b.addEventListener("click", () => { if (running) return; settle(); text.value = opt; form.requestSubmit(); });
      row.append(b);
    }
    const other = h("button", { type: "button", class: "btn sm ghost" }, t("chat.somethingElse"));
    other.addEventListener("click", () => { settle(); text.focus(); });
    row.append(other);
    group.append(row);
    return group;
  }
  function fmtCost(c) {
    const n = Number(c);
    return isNaN(n) ? String(c) : t("chat.cost", { usd: n.toFixed(4) });
  }

  async function loadAgents() {
    try {
      const a = await api.get("/agents");
      agents = a.providers || [];
      if (a.capacity) { cap.used = a.capacity.used; cap.max = a.capacity.max; showCapacity(); }
    } catch { agents = []; }
    clear(provider);
    for (const a of agents) provider.append(h("option", { value: a.provider, disabled: !a.available }, a.provider + (a.available ? "" : " " + t("chat.unavailable"))));
    if (agents.length === 0) provider.append(h("option", { value: "" }, t("chat.noAgents")));
    fillModels();
  }
  function fillModels() {
    const a = agents.find((x) => x.provider === provider.value);
    const list = (a && a.models) || [];
    manyModels = list.length > 30;
    modelSel.hidden = manyModels;
    modelIn.hidden = !manyModels;
    clear(modelSel).append(h("option", { value: "" }, t("chat.defaultModel")), ...list.map((m) => h("option", { value: m }, m)));
    clear(modelList).append(...list.map((m) => h("option", { value: m })));
    if (manyModels && !list.includes(modelIn.value.trim())) modelIn.value = "";
  }

  // quiet re-reads without the spinner: after a turn ends, the turn is
  // already on screen and only its final state changes.
  async function loadTurns(quiet) {
    if (!quiet) clear(turns).append(loading());
    held = [];
    try {
      const conv = cur.draft ? { turns: [] } : await api.get(cur.path);
      clear(turns);
      if (conv.provider && [...provider.options].some((o) => o.value === conv.provider)) provider.value = conv.provider;
      fillModels();
      if (conv.model) model.value = conv.model;
      const list = conv.turns || [];
      const keepTab = activeTab;
      resetAgents();
      list.forEach((tn, i) => {
        const role = tn.role === "user" ? "user" : "agent";
        // Turns from before steps were kept show their words alone.
        const hasSteps = role !== "user" && (tn.steps || []).length;
        const tu = turnEl(role, hasSteps ? "" : tn.text, tn.cost, tn.at);
        if (hasSteps) tu.textEl.replaceWith(stepsEl(tn.steps, tn.threads));
        if (tn.role === "error" && hasSteps) tu.el.append(h("div", { class: "level-error" }, tn.text.split("\n\n").pop()));
        // The last turn's subagents get their tabs at once; older ones open
        // from their row.
        if (role !== "user" && i >= list.length - 2) for (const th of tn.threads || []) if (th.run) agentView(th);
        // Only the last turn's questions are still open; earlier ones
        // were answered by whatever came after them.
        if (i === list.length - 1 && tn.role !== "user" && !conv.running) for (const q of tn.questions || []) tu.el.append(questionEl(q));
        turns.append(tu.el);
      });
      if (!(conv.turns || []).length) turns.append(h("div", { class: "empty" }, h("strong", null, t("chat.emptyTitle")), t(house ? "chat.emptyHintHouse" : "chat.emptyHint")));
      // A turn still running (it does not depend on this tab: it goes on
      // with the tab closed) is drawn where it is, then followed live.
      running = null;
      seen = { turn: "", seq: 0 };
      showTab(keepTab);
      if (conv.running) {
        begin();
        for (const lv of conv.live || []) handle({ type: lv.type, subject: cur.name, data: lv.data });
      } else {
        followUsage(false);
        stop.hidden = true;
        send.textContent = t("chat.send");
        status.textContent = "";
      }
      loadUsage();
      scroll();
    } catch (e) {
      clear(turns).append(errorBox(e));
    } finally {
      const late = held || [];
      held = null;
      late.forEach(handle);
    }
  }
  function scroll() { turns.lastElementChild && turns.lastElementChild.scrollIntoView({ block: "end" }); }

  function begin() {
    const tu = turnEl("agent", "");
    const working = h("div", { class: "small muted working typing" }, t("chat.working"));
    tu.el.append(working);
    turns.append(tu.el);
    running = { ...tu, working };
    followUsage(true);
    stop.hidden = false;
    send.textContent = t("chat.queue");
    status.textContent = t("chat.working");
    scroll();
  }
  function end(ev) {
    if (!running) return;
    running.working.remove();
    const cost = ev && ev.data && (ev.data.cost ?? ev.data.cost_usd ?? ev.data.usd);
    if (cost !== undefined) running.who.append(h("span", { class: "cost" }, fmtCost(cost)));
    running = null;
    followUsage(false);
    loadUsage();
    stop.hidden = true;
    send.textContent = t("chat.send");
    status.textContent = "";
  }

  // Events: agent.text (delta), agent.tool / agent.tool.result, agent.cost,
  // agent.end / agent.error / agent.stopped.
  // Events sent while the stream was down are not sent again: on coming
  // back, the conversation is read afresh rather than left showing a
  // turn as running that ended meanwhile.
  let wasLive = store.get("streamState") === "live";
  subs.push(store.subscribe("streamState", (st) => {
    if (st === "live" && !wasLive) loadTurns();
    wasLive = st === "live";
  }));
  // While the conversation is being read, events wait: drawn now they
  // would land in turns that the read is about to replace.
  let held = null;
  subs.push(onEvent("agent.", (ev) => {
    if (ev.subject !== cur.name) return;
    if (held) held.push(ev); else handle(ev);
  }));
  // seen is the last progress event drawn of the running turn: the ones
  // read from the conversation on opening it can arrive again on the
  // stream, and must not be drawn twice.
  let seen = { turn: "", seq: 0 };
  function handle(ev) {
    if (ev.type === "agent.capacity") return;
    const d = ev.data || {};
    if (d.seq) {
      if (d.turn_id === seen.turn && d.seq <= seen.seq) return;
      seen = { turn: d.turn_id, seq: d.seq };
    }
    if (ev.type === "agent.start" || ev.type === "agent.turn.started") { if (!running) begin(); return; }
    // The end of a turn this tab was not drawing (it ended while the
    // conversation was being read): the saved turn says it all.
    if (!running && /^agent\.(done|end|error|stopped|turn\.ended)$/.test(ev.type)) { loadTurns(); return; }
    if (!running) begin();
    // Where it goes: the subagent's tab, or the turn.
    const into = () => (d.thread ? agentView({ id: d.thread }).body : running.textEl);
    if (ev.type === "agent.text" || ev.type === "agent.delta") {
      const piece = d.delta ?? d.text ?? ev.message ?? "";
      if (piece) into().append(textStepEl(piece));
      scroll();
    } else if (ev.type === "agent.tool") {
      const tool = { id: d.id, name: d.tool || d.name || "tool", input: d.input, run: d.run };
      into().append(toolStepEl(tool));
      if (d.run) {
        const inp = d.input || {};
        agentView({ id: d.id, description: inp.description, prompt: inp.prompt, run: d.run });
        renderTabs();
      }
      scroll();
    } else if (ev.type === "agent.tool.result") {
      settleTool(d.id, d);
      if (d.run) setAgentRun(d.id, d.run);
    } else if (ev.type === "agent.subagent") {
      settleTool(d.id, { run: d.run });
      setAgentRun(d.id, d.run);
    } else if (ev.type === "agent.question") {
      // After the text that carried it, before whatever the agent says next.
      running.textEl.append(questionEl(d));
      scroll();
    } else if (ev.type === "agent.cost") {
      running.who.append(h("span", { class: "cost" }, fmtCost(d.cost ?? d.usd ?? ev.message)));
    } else if (ev.type === "agent.end" || ev.type === "agent.done" || ev.type === "agent.turn.ended" || ev.type === "agent.stopped") {
      end(ev);
      // The saved turn has every subagent's last state.
      loadTurns(true);
    } else if (ev.type === "agent.error") {
      running.el.append(h("div", { class: "level-error" }, ev.message || d.error || t("chat.failed")));
      end(null);
      if (d.turn_id) loadTurns(true);
    }
  }

  form.addEventListener("submit", async (e) => {
    e.preventDefault();
    const msg = text.value.trim();
    if (!msg) return;
    const whileRunning = Boolean(running);
    turns.querySelector(".empty")?.remove();
    turns.querySelector(".chat-notice")?.remove();
    const mine = turnEl("user", msg).el;
    turns.append(mine);
    text.value = "";
    send.classList.add("busy");
    try {
      const s = store.site(domain);
      const agentCfg = (s && s.manifest && s.manifest.agent) || {};
      if (!house && provider.value && provider.value !== agentCfg.provider) {
        await api.patch(`/sites/${enc(domain)}/manifest`, { agent: { ...agentCfg, provider: provider.value, model: model.value } });
      }
      await ensure();
      const r = await api.post(cur.path, { text: msg, model: model.value || undefined, provider: provider.value || undefined });
      // The title comes from this message: the list shows it now.
      fillPicker();
      if (r && r.queued) {
        // Kept for when the running turn ends; it goes out with any other
        // queued message as the next one.
        mine.querySelector(".who").append(h("span", { class: "muted" }, t("chat.queued")));
      } else if (!running && !whileRunning) begin();
    } catch (err) {
      if (err.status === 429) {
        // The house is full: the message goes back in the box, whole, and
        // the reason is said where the operator is looking.
        mine.remove();
        text.value = msg;
        turns.append(h("div", { class: "notice warn chat-notice" }, t("chat.capacityFull", { used: cap.used, max: cap.max })));
        scroll();
        syncCapacity();
        end(null);
        return;
      }
      toastError(err);
      end(null);
    } finally {
      send.classList.remove("busy");
    }
  });
  text.addEventListener("keydown", (e) => {
    if (e.key === "Enter" && (e.metaKey || e.ctrlKey)) { e.preventDefault(); form.requestSubmit(); }
  });
  provider.addEventListener("change", () => { fillModels(); loadUsage(); });
  stop.addEventListener("click", async () => {
    try { await api.post(`${cur.path}/stop`); status.textContent = t("chat.stopping"); } catch (err) { toastError(err); }
  });
  procsBtn.addEventListener("click", async () => {
    const list = h("div", { class: "stack-sm" });
    const fill = async () => {
      clear(list);
      const procs = await api.get(`${cur.path}/processes`);
      if (!procs.length) { list.append(h("p", { class: "muted" }, t("chat.noProcesses"))); return; }
      for (const pr of procs) {
        const kill = h("button", { type: "button", class: "btn sm danger" }, t("chat.kill"));
        kill.addEventListener("click", async () => {
          try { await api.post(`${cur.path}/processes/${pr.pid}/kill`); await fill(); } catch (err) { toastError(err); }
        });
        list.append(h("div", { class: "row" }, h("code", { class: "grow small" }, `${pr.pid}  ${pr.command}`), kill));
      }
    };
    try { await fill(); } catch (err) { toastError(err); return; }
    await open({
      title: t("chat.processesTitle"),
      body: [h("p", { class: "dim" }, t("chat.processesBody")), list],
      buttons: [{ label: t("common.close"), value: null }, { label: t("chat.killAll"), primary: true, onClick: async () => {
        const r = await api.post(`${cur.path}/reap`);
        toast(t("chat.killed", { n: r.killed }), "ok");
        await fill();
        return false;
      } }],
    });
  });
  reset.addEventListener("click", async () => {
    if (!(await confirm({ title: t("chat.newQ"), body: t("chat.newBody"), confirmLabel: t("chat.new") }))) return;
    try { await api.del(cur.path); await loadTurns(); } catch (err) { toastError(err); }
  });

  async function showUploads() {
    clear(chips);
    let ups = [];
    if (cur.draft) return;
    try { ups = await api.get(`${cur.path}/files`); } catch { ups = []; }
    for (const u of ups) {
      const x = h("button", { type: "button", class: "btn sm ghost", "aria-label": t("chat.removeFile", { name: u.name }) }, "×");
      x.addEventListener("click", async () => { try { await api.del(`${cur.path}/files/${enc(u.name)}`); showUploads(); } catch (e) { toastError(e); } });
      chips.append(h("span", { class: "badge" }, u.name, " ", x));
    }
  }
  async function upload(files) {
    try { await ensure(); } catch (e) { toastError(e); return; }
    for (const f of files) {
      try { await api.postRaw(`${cur.path}/files?name=${enc(f.name)}`, f, f.type || "application/octet-stream"); }
      catch (e) { toastError(e); }
    }
    showUploads();
  }
  attachBtn.addEventListener("click", () => fileInput.click());
  fileInput.addEventListener("change", () => { upload([...fileInput.files]); fileInput.value = ""; });
  el.addEventListener("dragover", (e) => { e.preventDefault(); });
  el.addEventListener("drop", (e) => { e.preventDefault(); if (e.dataTransfer && e.dataTransfer.files.length) upload([...e.dataTransfer.files]); });
  subs.push(onEvent("agent.started", (ev) => { if (ev.subject === cur.name) showUploads(); }));
  // The running mark beside each conversation follows the turns.
  subs.push(onEvent("agent.", (ev) => { if (/^agent\.(started|done|error)$/.test(ev.type) && ev.subject && ev.subject.split("#")[0] === domain) fillPicker(); }));

  loadAgents().then(() => fillPicker(true)).then(() => loadTurns()).then(showUploads);
  return { el, destroy() { subs.forEach((u) => u()); } };
}
