// Automatic repair: the switch and its brakes, and what it did. It acts
// while the operator sleeps, which is why it is off until turned on and
// why every intervention is listed here.

import { h, clear, fmtTime, fmtAgo } from "../lib/dom.js";
import { t } from "../lib/i18n.js";
import { api, enc } from "../lib/api.js";
import { load } from "../components/page.js";
import { field, toggle } from "../components/dialog.js";
import { toast, toastError } from "../components/toast.js";

export function autofixCard() {
  const el = h("div", { class: "card" });
  async function render() {
    const s = await api.get("/settings");
    const a = s.autofix || {};
    const on = toggle(t("autofix.enabled"), { checked: Boolean(a.enabled) });
    const attempts = h("input", { type: "number", min: "1", max: "5", value: a.attempts || 2 });
    const wait = h("input", { type: "number", min: "5", max: "1440", value: a.wait_minutes || 30 });
    const usd = h("input", { type: "number", min: "0.1", step: "0.1", value: a.daily_usd || 2 });
    const save = h("button", { type: "button", class: "btn primary" }, t("common.save"));
    save.addEventListener("click", async () => {
      save.classList.add("busy");
      try {
        const cur = await api.get("/settings");
        await api.put("/settings", { ...cur, autofix: { enabled: on.input.checked, attempts: Number(attempts.value), wait_minutes: Number(wait.value), daily_usd: Number(usd.value) } });
        toast(t("autofix.saved"), "ok");
      } catch (e) { toastError(e); } finally { save.classList.remove("busy"); }
    });
    const repairs = (await api.get("/autofix").catch(() => [])).slice(-8).reverse();
    clear(el).append(
      h("h2", null, t("autofix.title")),
      h("p", { class: "small muted" }, t("autofix.hint")),
      h("div", { class: "field" }, on.el),
      h("div", { class: "form-row" }, field(t("autofix.attempts"), attempts), field(t("autofix.wait"), wait), field(t("autofix.daily"), usd)),
      h("div", { class: "form-actions" }, save),
      repairs.length ? h("div", { class: "stack-sm small" }, h("h3", null, t("autofix.recent")), ...repairs.map((r) => h("div", null,
        h("a", { href: `#/sites/${enc(r.domain)}` }, r.domain), " · ", h("span", { title: fmtTime(r.at) }, fmtAgo(r.at, t)), " · ",
        r.skipped ? h("span", { class: "muted" }, r.skipped) : h("span", { class: r.recovered === false ? "level-error" : "" }, (r.outcome || r.reason) + (r.cost_usd ? ` (${r.cost_usd.toFixed(2)} $)` : ""))))) : h("p", { class: "small muted" }, t("autofix.none")),
    );
    return null;
  }
  load(el, render);
  return el;
}
