// Automatic repair: the switch and its brakes, and what it did. It acts
// while the operator sleeps, which is why it is off until turned on and
// why every intervention is listed here.

import { h, clear, fmtTime, fmtAgo } from "../lib/dom.js";
import { t } from "../lib/i18n.js";
import { api, enc } from "../lib/api.js";
import { load } from "../components/page.js";
import { field, toggle } from "../components/dialog.js";
import { toastError } from "../components/toast.js";
import { cardHead, saveBar, fold } from "./settings-kit.js";

export function autofixCard() {
  const el = h("div", { class: "card set-card" });
  async function render() {
    const s = await api.get("/settings");
    const a = s.autofix || {};
    const on = toggle(t("autofix.enabled"), { checked: Boolean(a.enabled) });
    const attempts = h("input", { type: "number", min: "1", max: "5", value: a.attempts || 2 });
    const wait = h("input", { type: "number", min: "5", max: "1440", value: a.wait_minutes || 30 });
    const usd = h("input", { type: "number", min: "0.1", step: "0.1", value: a.daily_usd || 2 });
    const save = h("button", { type: "button", class: "btn primary" }, t("common.save"));
    const bar = saveBar(save);
    save.addEventListener("click", async () => {
      save.classList.add("busy");
      try {
        const cur = await api.get("/settings");
        await api.put("/settings", { ...cur, autofix: { enabled: on.input.checked, attempts: Number(attempts.value), wait_minutes: Number(wait.value), daily_usd: Number(usd.value) } });
        bar.saved();
      } catch (e) { toastError(e); } finally { save.classList.remove("busy"); }
    });
    const repairs = (await api.get("/autofix").catch(() => [])).slice(-8).reverse();
    // The switch is the decision; the brakes are right for most and fold,
    // saying what they are now.
    const brakes = t("autofix.brakesNote", { attempts: attempts.value, wait: wait.value, usd: usd.value });
    const form = h("div", null,
      h("div", { class: "field set-switch-field" }, on.el),
      fold(t("autofix.brakes"), brakes, [h("div", { class: "form-row" }, field(t("autofix.attempts"), attempts), field(t("autofix.wait"), wait), field(t("autofix.daily"), usd))]),
      bar.el);
    bar.watch(form);
    clear(el).append(
      cardHead(t("autofix.title"), t("autofix.hint")),
      form,
      h("div", { class: "set-recent" }, h("h3", null, t("autofix.recent")),
        repairs.length ? h("ul", { class: "set-recent-list" }, ...repairs.map((r) => h("li", null,
          h("a", { href: `#/sites/${enc(r.domain)}` }, r.domain),
          r.skipped ? h("span", { class: "muted" }, r.skipped) : h("span", { class: r.recovered === false ? "level-error" : "" }, (r.outcome || r.reason) + (r.cost_usd ? ` (${r.cost_usd.toFixed(2)} $)` : "")),
          h("span", { class: "muted", title: fmtTime(r.at) }, fmtAgo(r.at, t)))))
          : h("p", { class: "hint" }, t("autofix.none"))),
    );
    return null;
  }
  load(el, render);
  return el;
}

// The house's AI for automatic work: who repairs a site that arrived from
// a move and does not start, and the automatic repairs. Apart from the
// house's conversation, where the operator picks whom to talk to each
// time: this one acts with nobody there, so it is chosen once, here.
export function automationCard() {
  const el = h("div", { class: "card set-card" });
  async function render() {
    const [s, agents] = await Promise.all([api.get("/settings"), api.get("/agents").catch(() => ({ providers: [] }))]);
    const auto = s.automation || {};
    const providers = (agents.providers || []).filter((p) => p.available || p.provider === auto.provider);
    const prov = h("select", null, h("option", { value: "" }, t("automation.none")), ...providers.map((p) => h("option", { value: p.provider }, p.provider)));
    prov.value = auto.provider || "";
    const model = h("select");
    function fillModels() {
      const p = providers.find((x) => x.provider === prov.value);
      clear(model).append(h("option", { value: "" }, t("chat.defaultModel")), ...((p && p.models) || []).map((m) => h("option", { value: m }, (p.labels && p.labels[m]) ? `${m} (${p.labels[m]})` : m)));
      model.value = (p && (p.models || []).includes(auto.model)) ? auto.model : "";
      model.disabled = !p;
    }
    prov.addEventListener("change", fillModels);
    fillModels();
    const save = h("button", { type: "button", class: "btn primary" }, t("common.save"));
    const bar = saveBar(save);
    save.addEventListener("click", async () => {
      save.classList.add("busy");
      try {
        const cur = await api.get("/settings");
        await api.put("/settings", { ...cur, automation: prov.value ? { provider: prov.value, model: model.value || undefined } : {} });
        bar.saved();
      } catch (e) { toastError(e); } finally { save.classList.remove("busy"); }
    });
    const form = h("div", null, h("div", { class: "form-row" }, field(t("automation.provider"), prov), field(t("automation.model"), model)), bar.el);
    bar.watch(form);
    clear(el).append(cardHead(t("automation.title"), t("automation.hint")), form);
    return null;
  }
  load(el, render);
  return el;
}
