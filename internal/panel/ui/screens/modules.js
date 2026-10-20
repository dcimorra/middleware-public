// Modules: one card each, a form built from what the module says it
// needs, and Verify / Enable that show the result lines as a checklist.
// Secrets are password fields and are never echoed back: the API masks
// them, so an empty secret field on save means "keep what is stored".

import { h, clear } from "../lib/dom.js";
import { t } from "../lib/i18n.js";
import { api, enc } from "../lib/api.js";
import { onEvent } from "../lib/stream.js";
import { page, load, empty, notice } from "../components/page.js";
import { icon } from "../lib/icons.js";

import { checklist } from "../components/checklist.js";
import { confirm, field } from "../components/dialog.js";
import { toast, toastError } from "../components/toast.js";

export function modulesScreen() {
  const p = page({ title: t("nav.modules"), sub: t("modules.sub"), icon: "blocks" });
  const body = h("div", { class: "grid" });
  p.body.append(body);
  const subs = [];

  async function render() {
    const mods = await api.get("/modules");
    clear(body);
    if (!mods || mods.length === 0) { body.append(empty(t("modules.emptyTitle"), t("modules.emptyHint"))); return null; }
    for (const m of mods) body.append(card(m));
    return null;
  }

  function card(m) {
    const inputs = {};
    const form = h("form", { novalidate: true, class: "needs" });
    for (const n of m.needs || []) {
      const input = h("input", {
        type: n.secret ? "password" : "text", autocomplete: n.secret ? "new-password" : "off",
        class: "mono", placeholder: n.secret && m.enabled ? t("modules.secretKept") : "", value: n.value || "",
      });
      inputs[n.key] = input;
      form.append(field(n.label || n.key, input, n.hint));
    }
    const lines = checklist();
    const verifyBtn = h("button", { type: "button", class: "btn", hidden: !(m.needs && m.needs.length) }, t("modules.verify"));
    const enableBtn = h("button", { type: "submit", class: "btn primary" }, m.enabled ? t("modules.save") : t("modules.enable"));
    const disableBtn = h("button", { type: "button", class: "btn", hidden: !m.enabled }, t("modules.disable"));
    const forgetBtn = h("button", { type: "button", class: "btn ghost sm", hidden: !(m.needs && m.needs.length) }, t("modules.forget"));
    const config = () => {
      const c = {};
      for (const [k, input] of Object.entries(inputs)) if (input.value !== "") c[k] = input.value;
      return c;
    };
    const run = async (btn, path, done) => {
      btn.classList.add("busy");
      lines.set([{ state: "pending", text: t("modules.running") }]);
      try {
        const r = await api.post(path, { config: config() });
        lines.set(r && r.lines);
        if (r && r.ok === false) toast(t("modules.failed", { name: m.name }), "bad");
        else if (done) { toast(done, "ok"); }
        return r;
      } catch (e) {
        lines.set([{ ok: false, text: e.message }]);
      } finally { btn.classList.remove("busy"); }
      return null;
    };
    verifyBtn.addEventListener("click", () => run(verifyBtn, `/modules/${enc(m.key)}/verify`, t("modules.verified", { name: m.name })));
    form.addEventListener("submit", async (e) => {
      e.preventDefault();
      const r = await run(enableBtn, `/modules/${enc(m.key)}/enable`, t("modules.enabled", { name: m.name }));
      if (r && r.ok !== false) { m.enabled = true; state.replaceWith(stateBadge(m)); enableBtn.textContent = t("modules.save"); disableBtn.hidden = false; }
    });
    disableBtn.addEventListener("click", async () => {
      if (!(await confirm({ title: t("modules.disableQ", { name: m.name }), body: t("modules.disableBody"), confirmLabel: t("modules.disable"), danger: true }))) return;
      try { await api.post(`/modules/${enc(m.key)}/disable`); toast(t("modules.disabled", { name: m.name }), "ok"); load(body, render); } catch (err) { toastError(err); }
    });
    forgetBtn.addEventListener("click", async () => {
      if (!(await confirm({ title: t("modules.forgetQ", { name: m.name }), body: t("modules.forgetBody"), confirmLabel: t("modules.forget"), danger: true }))) return;
      try { await api.del(`/modules/${enc(m.key)}/credentials`); toast(t("modules.forgotten", { name: m.name }), "ok"); load(body, render); } catch (err) { toastError(err); }
    });
    form.append(lines.el, h("div", { class: "form-actions" }, verifyBtn, enableBtn, disableBtn, forgetBtn));
    const state = stateBadge(m);
    return h("div", { class: "card module-card" },
      h("div", { class: "log-bar" }, h("h2", { class: "grow" }, m.name || m.key), state),
      h("p", { class: "dim small" }, m.description || ""),
      m.installed === false && m.enabled ? notice("warn", t("modules.notInstalled")) : null,
      form,
    );
  }
  function stateBadge(m) {
    return m.enabled ? h("span", { class: "badge ok" }, t("modules.on")) : h("span", { class: "badge" }, t("modules.off"));
  }

  load(body, render);
  // A module event from elsewhere (the CLI, another tab) refreshes the
  // cards, unless the operator is typing or reading result lines here:
  // rebuilding would wipe the very lines they asked for.
  subs.push(onEvent("module.", () => {
    const typing = document.activeElement && document.activeElement.tagName === "INPUT";
    const reading = body.querySelector(".checklist li");
    if (!typing && !reading) load(body, render);
  }));
  return { el: p.el, destroy() { subs.forEach((u) => u()); } };
}
