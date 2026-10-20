// Chat with the agent that works inside this site's folder. Turns come
// from GET /chat; while a turn runs, progress arrives on the event stream
// as agent.* events with the domain as subject: text deltas, tool calls,
// cost, end. The shapes are tolerant on purpose: the adapter decides what
// it puts in data, and a field the panel does not know is just not shown.

import { icon } from "../lib/icons.js";
import { h, clear, debounce } from "../lib/dom.js";
import { t } from "../lib/i18n.js";
import { api, enc } from "../lib/api.js";
import { store } from "../lib/store.js";
import { onEvent } from "../lib/stream.js";
import { loading, errorBox } from "../components/page.js";
import { confirm, open } from "../components/dialog.js";
import { toast, toastError } from "../components/toast.js";

// chatTab is a site's chat. For the house's own conversation, domain is
// "middleware": no other conversations, no site manifest to write the
// provider into.
export function chatTab(domain) {
  const house = domain === "middleware";
  const turns = h("div", { class: "chat-turns", "aria-live": "polite" });
  const text = h("textarea", { placeholder: t("chat.placeholder"), "aria-label": t("chat.message") });
  const provider = h("select", { "aria-label": t("chat.provider") });
  const model = h("select", { "aria-label": t("chat.model") });
  const send = h("button", { type: "submit", class: "btn primary" }, icon("send"), t("chat.send"));
  const stop = h("button", { type: "button", class: "btn danger", hidden: true }, icon("stop"), t("chat.stop"));
  const reset = h("button", { type: "button", class: "btn ghost sm" }, icon("sparkles"), t("chat.new"));
  // What the conversation left running, setsid escapes included: listed
  // from the environment mark every turn carries, and killed one by one or
  // all at once. Otherwise they live for days and nobody sees them.
  const procsBtn = h("button", { type: "button", class: "btn ghost sm" }, icon("list"), t("chat.processes"));
  const status = h("span", { class: "small muted", role: "status" });
  const capacity = h("span", { class: "small muted chat-capacity", title: t("chat.capacityTitle") });
  const form = h("form", { class: "chat-form" }, text, h("div", { class: "row" }, provider, model, h("span", { class: "grow" }), status, procsBtn, reset, stop, send));
  // Which conversation is open: the site's first one, another of the
  // panel's, or one the site's code opened through the site door (read
  // and stopped from here, never written into: a program is on the other
  // side, and its turns follow its own rules).
  const base = `/sites/${enc(domain)}`;
  let cur = house ? { name: domain, path: "/chat/middleware", origin: "panel" } : { name: domain, path: `${base}/chat`, origin: "panel" };
  const picker = h("select", { "aria-label": t("chat.conversation") });
  const newConv = h("button", { type: "button", class: "btn ghost sm" }, icon("plus"), t("chat.newConversation"));
  const readonlyNote = h("div", { class: "small muted", hidden: true }, t("chat.siteDoorNote"));
  async function fillPicker() {
    if (house) { picker.hidden = true; newConv.hidden = true; return; }
    let others = [];
    try { others = await api.get(`${base}/chats`); } catch { others = []; }
    clear(picker).append(h("option", { value: "" }, t("chat.mainConversation")));
    for (const c of others) {
      const label = (c.origin === "site" ? t("chat.fromSite") + " · " : "") + (c.title || c.id) + (c.running ? " ●" : "");
      picker.append(h("option", { value: c.id, "data-origin": c.origin }, label));
    }
    picker.value = cur.name === domain ? "" : cur.name.split("#")[1];
    picker.hidden = others.length === 0;
  }
  picker.addEventListener("change", () => {
    const id = picker.value;
    const origin = id ? picker.selectedOptions[0].dataset.origin : "panel";
    cur = id ? { name: `${domain}#${id}`, path: `${base}/chats/${enc(id)}`, origin } : { name: domain, path: `${base}/chat`, origin: "panel" };
    form.hidden = origin === "site";
    readonlyNote.hidden = origin !== "site";
    loadTurns();
  });
  newConv.addEventListener("click", async () => {
    try {
      const r = await api.post(`${base}/chats`, {});
      cur = { name: `${domain}#${r.id}`, path: `${base}/chats/${enc(r.id)}`, origin: "panel" };
      await fillPicker();
      form.hidden = false; readonlyNote.hidden = true;
      loadTurns();
    } catch (e) { toastError(e); }
  });
  const el = h("div", { class: "chat" }, h("div", { class: "chat-head row" }, picker, newConv, h("span", { class: "grow" }), capacity), readonlyNote, turns, form);
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

  function turnEl(role, body, cost, at) {
    const who = h("div", { class: "who" }, role === "user" ? t("chat.you") : t("chat.agent"), at ? h("span", { class: "muted" }, new Date(at).toLocaleTimeString()) : null, cost ? h("span", { class: "cost" }, fmtCost(cost)) : null);
    const textEl = h("div", { class: "text" }, role === "user" ? (body || "") : stripQuestions(body || ""));
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
    clear(model);
    const a = agents.find((x) => x.provider === provider.value);
    model.append(h("option", { value: "" }, t("chat.defaultModel")));
    for (const m of (a && a.models) || []) model.append(h("option", { value: m }, m));
  }

  async function loadTurns() {
    clear(turns).append(loading());
    try {
      const conv = await api.get(cur.path);
      clear(turns);
      if (conv.provider && [...provider.options].some((o) => o.value === conv.provider)) provider.value = conv.provider;
      fillModels();
      if (conv.model) model.value = conv.model;
      const list = conv.turns || [];
      list.forEach((tn, i) => {
        const tu = turnEl(tn.role === "user" ? "user" : "agent", tn.text, tn.cost, tn.at);
        // Only the last turn's questions are still open; earlier ones
        // were answered by whatever came after them.
        if (i === list.length - 1 && tn.role !== "user" && !conv.running) for (const q of tn.questions || []) tu.el.append(questionEl(q));
        turns.append(tu.el);
      });
      if (!(conv.turns || []).length) turns.append(h("div", { class: "empty" }, h("strong", null, t("chat.emptyTitle")), t("chat.emptyHint")));
      scroll();
    } catch (e) {
      clear(turns).append(errorBox(e));
    }
  }
  function scroll() { turns.lastElementChild && turns.lastElementChild.scrollIntoView({ block: "end" }); }

  function begin() {
    const tu = turnEl("agent", "");
    tu.textEl.classList.add("typing");
    turns.append(tu.el);
    running = { ...tu, buf: "", tools: new Map() };
    stop.hidden = false;
    send.textContent = t("chat.queue");
    status.textContent = t("chat.working");
    scroll();
  }
  function end(ev) {
    if (!running) return;
    running.textEl.classList.remove("typing");
    const cost = ev && ev.data && (ev.data.cost ?? ev.data.cost_usd ?? ev.data.usd);
    if (cost !== undefined) running.who.append(h("span", { class: "cost" }, fmtCost(cost)));
    running = null;
    stop.hidden = true;
    send.textContent = t("chat.send");
    status.textContent = "";
  }

  // Events: agent.text (delta), agent.tool / agent.tool.result, agent.cost,
  // agent.end / agent.error / agent.stopped.
  subs.push(onEvent("agent.", (ev) => {
    if (ev.subject !== cur.name || ev.type === "agent.capacity") return;
    const d = ev.data || {};
    if (ev.type === "agent.start" || ev.type === "agent.turn.started") { if (!running) begin(); return; }
    if (!running) begin();
    if (ev.type === "agent.text" || ev.type === "agent.delta") {
      const piece = d.delta ?? d.text ?? ev.message ?? "";
      running.buf += piece;
      running.textEl.textContent = stripQuestions(running.buf);
      scroll();
    } else if (ev.type.startsWith("agent.tool")) {
      const id = d.id || d.tool_id || d.name || ev.time;
      let det = running.tools.get(id);
      if (!det) {
        det = h("details", null, h("summary", null, t("chat.tool", { name: d.name || d.tool || "tool" })), h("pre", null, ""));
        running.tools.set(id, det);
        // The row goes after the text streamed so far, and what the agent
        // says next starts a fresh paragraph below it, in the order it
        // happened.
        running.textEl.classList.remove("typing");
        if (!running.textEl.textContent) running.textEl.remove();
        const next = h("div", { class: "text typing" });
        running.el.append(det, next);
        running.textEl = next;
        running.buf = "";
      }
      const pre = det.querySelector("pre");
      const input = d.input !== undefined ? JSON.stringify(d.input, null, 2) : "";
      const output = d.output !== undefined ? (typeof d.output === "string" ? d.output : JSON.stringify(d.output, null, 2)) : "";
      if (ev.type.includes("result") || output) pre.textContent = (input ? input + "\n\n" : "") + output;
      else if (input) pre.textContent = input;
      else if (ev.message) pre.textContent = ev.message;
    } else if (ev.type === "agent.question") {
      // After the text that carried it, before whatever the agent says next.
      running.textEl.classList.remove("typing");
      if (!running.textEl.textContent) running.textEl.remove();
      const next = h("div", { class: "text typing" });
      running.el.append(questionEl(d), next);
      running.textEl = next;
      running.buf = "";
      scroll();
    } else if (ev.type === "agent.cost") {
      running.who.append(h("span", { class: "cost" }, fmtCost(d.cost ?? d.usd ?? ev.message)));
    } else if (ev.type === "agent.end" || ev.type === "agent.done" || ev.type === "agent.turn.ended" || ev.type === "agent.stopped") {
      end(ev);
    } else if (ev.type === "agent.error") {
      running.el.append(h("div", { class: "level-error" }, ev.message || d.error || t("chat.failed")));
      end(null);
    }
  }));

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
      const r = await api.post(cur.path, { text: msg, model: model.value || undefined });
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
  provider.addEventListener("change", fillModels);
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

  loadAgents().then(fillPicker).then(loadTurns);
  return { el, destroy() { subs.forEach((u) => u()); } };
}
