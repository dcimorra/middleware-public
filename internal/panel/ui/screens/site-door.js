// The site's LLM gateway (the door its code calls), on its page: open or
// closed, which agent answers and with which model, what it may do (each
// permission apart, only the ones that agent can hold apart), its daily
// caps, how many conversations it keeps, and what it spent. Everything is
// written into the site's manifest by the server, and opening or closing
// restarts the site so the process gets (or loses) its token.

import { h, clear } from "../lib/dom.js";
import { t } from "../lib/i18n.js";
import { api, enc } from "../lib/api.js";
import { notice, kv } from "../components/page.js";
import { confirm, field, checkbox, toggle } from "../components/dialog.js";
import { toast, toastError } from "../components/toast.js";
import { icon } from "../lib/icons.js";

const PERMS = ["read", "edit", "web", "shell"];
// Granting one of these is said out loud before saving: the site's
// process answers the internet.
const RISKY = new Set(["edit", "web", "shell"]);

export function doorCard(domain) {
  const el = h("div", { class: "card" });
  const path = `/sites/${enc(domain)}/claude-api`;
  const usd = (v) => `$${Number(v || 0).toFixed(2)}`;

  async function load() {
    let d;
    try { d = await api.get(path); } catch (e) { clear(el).append(h("h2", null, t("door.title")), notice("bad", e.message)); return; }
    clear(el).append(h("h2", null, t("door.title")), h("p", { class: "small muted" }, t("door.hint")));
    if (!d.process && !d.enabled) { el.append(notice("info", t("door.noProcess"))); return; }
    const sw = toggle(t(d.enabled ? "door.open" : "door.closed"), { checked: d.enabled });
    el.append(h("div", { class: "field" }, sw.el));
    sw.input.addEventListener("change", async () => {
      const on = sw.input.checked;
      if (!(await confirm({ title: t(on ? "door.openQ" : "door.closeQ", { domain }), body: t(on ? "door.openBody" : "door.closeBody"), confirmLabel: t(on ? "door.openBtn" : "door.closeBtn"), danger: !on }))) {
        sw.input.checked = !on;
        return;
      }
      // Opened with every permission its agent can be given (the
      // operator narrows it after), on Claude if Claude can run, else on
      // the first agent that can.
      let body = { enabled: false };
      if (on) {
        const agents = (d.agents || []).filter((a) => a.available);
        const a = agents.find((x) => x.provider === "claude") || agents[0];
        if (!a) { sw.input.checked = false; toast(t("door.noAgents"), "bad"); return; }
        body = { ...d.defaults, enabled: true, provider: a.provider, permissions: d.defaults.permissions.filter((p) => a.permissions.includes(p)), model: "" };
      }
      try {
        const r = await api.post(path, body);
        toast(t(on ? "door.opened" : "door.closedDone"), "ok");
        if (r.warn) toast(r.warn, "bad");
        load();
      } catch (e) { sw.input.checked = !on; toastError(e); }
    });
    if (!d.enabled) return;

    const lim = d.limits || d.defaults;
    const agents = d.agents || [];
    const agentOf = (key) => agents.find((a) => a.provider === key) || { provider: key, available: false, models: [], permissions: PERMS };
    const agent = h("select", null, ...agents.map((a) => h("option", { value: a.provider, disabled: !a.available }, a.provider + (a.available ? "" : " " + t("chat.unavailable")))));
    agent.value = lim.provider || "claude";
    const model = h("select");
    // One box per permission. One the agent cannot hold apart is shown
    // as it will be, not offered: Codex's shell goes with reading or
    // editing, Antigravity's web search is always there.
    const granted = new Set(lim.permissions || []);
    const boxes = {};
    const permsEl = h("div", { class: "door-perms" });
    const permsHint = h("div", { class: "hint" });
    function fillAgent(keepModel) {
      const a = agentOf(agent.value);
      const was = keepModel ? model.value : "";
      clear(model).append(h("option", { value: "" }, t(agent.value === "claude" ? "door.houseModel" : "door.agentModel")), ...(a.models || []).map((m) => h("option", { value: m }, m)));
      model.value = (a.models || []).includes(was) ? was : "";
      clear(permsEl);
      const fixed = [];
      for (const p of PERMS) {
        const own = a.permissions.includes(p);
        const cb = checkbox(t("door.perm." + p), { checked: own ? granted.has(p) : false, disabled: !own });
        cb.el.title = t("door.permHint." + p);
        boxes[p] = { ...cb, own };
        cb.input.addEventListener("change", () => { if (cb.input.checked) granted.add(p); else granted.delete(p); sayFixed(); });
        permsEl.append(cb.el);
        if (!own) fixed.push(p);
      }
      const sayFixed = () => {
        // What the agent does with a permission it cannot hold apart,
        // given the others as they are now.
        const notes = fixed.map((p) => t(`door.fixed.${agent.value}.${p}`) === `door.fixed.${agent.value}.${p}` ? t("door.fixed.other", { perm: t("door.perm." + p) }) : t(`door.fixed.${agent.value}.${p}`));
        for (const p of fixed) {
          boxes[p].input.checked = agent.value === "openai" && p === "shell" ? granted.has("read") || granted.has("edit") : true;
        }
        permsHint.textContent = notes.join(" ");
      };
      sayFixed();
    }
    agent.addEventListener("change", () => fillAgent(false));
    fillAgent(true);
    model.value = lim.model || "";
    const daily = h("input", { type: "number", min: "0.01", step: "0.01", value: String(lim.daily_usd) });
    const turns = h("input", { type: "number", min: "0", step: "1", value: String(lim.daily_turns || 0) });
    const maxChats = h("input", { type: "number", min: "1", step: "1", value: String(lim.max_chats) });
    const hide = checkbox(t("door.hide"), { checked: Boolean(lim.hide_chats) });
    const save = h("button", { type: "submit", class: "btn primary" }, t("common.save"));
    const form = h("form", { novalidate: true },
      h("div", { class: "form-row" }, field(t("door.agent"), agent, t("door.agentHint")), field(t("door.model"), model)),
      h("div", { class: "field" }, h("label", null, t("door.tools")), permsEl, permsHint),
      h("div", { class: "form-row" }, field(t("door.daily"), daily, t("door.dailyHint")), field(t("door.turns"), turns, t("door.turnsHint"))),
      h("div", { class: "form-row" }, field(t("door.maxChats"), maxChats, t("door.maxChatsHint")), h("div", { class: "field" }, hide.el)),
      h("div", { class: "form-actions" }, save));
    form.addEventListener("submit", async (e) => {
      e.preventDefault();
      const a = agentOf(agent.value);
      const perms = PERMS.filter((p) => a.permissions.includes(p) && granted.has(p));
      const before = new Set(lim.provider === agent.value ? lim.permissions || [] : []);
      const raised = perms.filter((p) => RISKY.has(p) && !before.has(p));
      if (raised.length) {
        if (!(await confirm({ title: t("door.raiseQ", { level: raised.map((p) => t("door.perm." + p)).join(", ") }), body: t("door.raiseBody"), confirmLabel: t("common.save"), danger: true }))) return;
      }
      save.classList.add("busy");
      try {
        await api.post(path, { enabled: true, provider: agent.value, permissions: perms, daily_usd: Number(daily.value), daily_turns: Number(turns.value), max_chats: Number(maxChats.value), model: model.value, hide_chats: hide.input.checked });
        toast(t("door.saved"), "ok");
        load();
      } catch (err) { toastError(err); } finally { save.classList.remove("busy"); }
    });

    const copy = h("button", { type: "button", class: "btn sm ghost", title: t("door.copy") }, icon("copy"));
    copy.addEventListener("click", async () => { try { await navigator.clipboard.writeText(d.token); toast(t("door.copied"), "ok"); } catch { /* the token stays visible */ } });
    const use = d.use || {};
    el.append(
      h("div", { class: "stats" },
        stat(t("door.today"), `${usd(d.spent_usd)} / ${usd(lim.daily_usd)}`),
        lim.daily_turns ? stat(t("door.turnsToday"), `${d.turns || 0} / ${lim.daily_turns}`) : null,
        stat(t("door.d7"), usd(use.d7)),
        stat(t("door.d30"), usd(use.d30)),
        stat(t("door.month"), usd(use.month)),
        stat(t("door.chats"), `${d.chats || 0} / ${lim.max_chats}`)),
      h("details", { class: "advanced" }, h("summary", null, t("door.forDevelopers")),
        h("p", { class: "small muted" }, t("door.envHint")),
        kv([
          ["MIDDLEWARE_API", h("code", null, d.api || "")],
          ["MIDDLEWARE_TOKEN", h("div", { class: "row" }, h("code", { class: "small grow" }, d.token || ""), copy)],
          ["MIDDLEWARE_API_SOCKET", d.socket ? h("code", null, d.socket) : null],
        ]),
        h("pre", { class: "snippet" }, `curl -s "$MIDDLEWARE_API/api/site/claude/ask" \\\n  -H "Authorization: Bearer $MIDDLEWARE_TOKEN" -H 'Content-Type: application/json' \\\n  -d '{"prompt": "…", "format": "json"}'`)),
      form,
    );
  }
  function stat(label, value) {
    return h("div", { class: "stat" }, h("div", { class: "label" }, label), h("div", { class: "value mono" }, value));
  }
  load();
  return el;
}
