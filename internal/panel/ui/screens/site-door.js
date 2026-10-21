// The site's Claude door, on its page: open or closed, what Claude may do
// when the site's code asks, its daily cap, how many conversations it
// keeps, its model, and what it spent. Everything is written into the
// site's manifest by the server, and opening or closing restarts the site
// so the process gets (or loses) its token.

import { h, clear } from "../lib/dom.js";
import { t } from "../lib/i18n.js";
import { api, enc } from "../lib/api.js";
import { notice, kv } from "../components/page.js";
import { confirm, field, checkbox, toggle } from "../components/dialog.js";
import { toast, toastError } from "../components/toast.js";
import { icon } from "../lib/icons.js";

const LEVELS = ["none", "read", "web", "write"];

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
      try {
        const r = await api.post(path, on ? { enabled: true, ...(d.limits || d.defaults) } : { enabled: false });
        toast(t(on ? "door.opened" : "door.closedDone"), "ok");
        if (r.warn) toast(r.warn, "bad");
        load();
      } catch (e) { sw.input.checked = !on; toastError(e); }
    });
    if (!d.enabled) return;

    const lim = d.limits || d.defaults;
    const tools = h("select", null, ...LEVELS.map((l) => h("option", { value: l }, t("door.level." + l))));
    tools.value = lim.tools || "none";
    const toolsHint = h("div", { class: "hint" });
    const sayLevel = () => { toolsHint.textContent = t("door.levelHint." + tools.value); };
    tools.addEventListener("change", sayLevel);
    sayLevel();
    const daily = h("input", { type: "number", min: "0.01", step: "0.01", value: String(lim.daily_usd) });
    const maxChats = h("input", { type: "number", min: "1", step: "1", value: String(lim.max_chats) });
    const model = h("select", null, h("option", { value: "" }, t("door.houseModel")), ...(d.models || []).map((m) => h("option", { value: m }, m)));
    model.value = lim.model || "";
    const hide = checkbox(t("door.hide"), { checked: Boolean(lim.hide_chats) });
    const save = h("button", { type: "submit", class: "btn primary" }, t("common.save"));
    const form = h("form", { novalidate: true },
      h("div", { class: "form-row" }, h("div", { class: "field" }, h("label", null, t("door.tools")), tools, toolsHint), field(t("door.model"), model)),
      h("div", { class: "form-row" }, field(t("door.daily"), daily, t("door.dailyHint")), field(t("door.maxChats"), maxChats, t("door.maxChatsHint"))),
      h("div", { class: "field" }, hide.el),
      h("div", { class: "form-actions" }, save));
    form.addEventListener("submit", async (e) => {
      e.preventDefault();
      if (tools.value !== lim.tools && ["web", "write"].includes(tools.value)) {
        if (!(await confirm({ title: t("door.raiseQ", { level: t("door.level." + tools.value) }), body: t("door.raiseBody"), confirmLabel: t("common.save"), danger: true }))) return;
      }
      save.classList.add("busy");
      try {
        await api.post(path, { enabled: true, tools: tools.value, daily_usd: Number(daily.value), max_chats: Number(maxChats.value), model: model.value, hide_chats: hide.input.checked });
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
