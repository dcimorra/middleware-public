// Dialogs on the native <dialog>: focus trapping, Escape and the backdrop
// come from the browser, which does them better than any hand-rolled
// overlay did, and a form with method="dialog" closes itself.

import { h, append } from "../lib/dom.js";
import { t } from "../lib/i18n.js";
import { icon } from "../lib/icons.js";

// open shows a dialog and resolves with the return value when it closes.
// body is a Node or array of nodes; buttons is [{label, value, class,
// primary, onClick}] where onClick may return false to keep it open.
export function open({ title, body, buttons, wide, onSubmit, initialFocus }) {
  return new Promise((resolve) => {
    const dlg = h("dialog", { class: wide ? "wide" : "" });
    const form = h("form", { class: "dlg", method: "dialog", novalidate: true });
    const closeBtn = h("button", { type: "button", class: "icon-btn sm", "aria-label": t("common.close") }, icon("x", { size: 18 }));
    const head = h("div", { class: "dlg-head" }, h("h2", null, title), closeBtn);
    const bodyEl = h("div", { class: "dlg-body" });
    append(bodyEl, [body]);
    const foot = h("div", { class: "dlg-foot" });
    const errBox = h("div", { class: "error-box", role: "alert", hidden: true });
    bodyEl.append(errBox);
    let settled = false;
    const finish = (value) => {
      if (settled) return;
      settled = true;
      resolve(value);
      dlg.close();
      dlg.remove();
    };
    for (const b of buttons || [{ label: t("common.close"), value: null }]) {
      const btn = h("button", {
        type: b.submit ? "submit" : "button",
        class: "btn " + (b.primary ? "primary " : "") + (b.danger ? "danger " : "") + (b.class || ""),
      }, b.label);
      if (!b.submit) {
        btn.addEventListener("click", async () => {
          // A second tap while the first is still at work sent the same
          // request twice.
          if (btn.classList.contains("busy")) return;
          if (b.onClick) {
            btn.classList.add("busy");
            btn.setAttribute("aria-busy", "true");
            try {
              const r = await b.onClick(form, showError);
              if (r === false) return;
              finish(r === undefined ? b.value : r);
            } catch (e) {
              showError(e.message || String(e));
            } finally {
              btn.classList.remove("busy");
              btn.removeAttribute("aria-busy");
            }
            return;
          }
          finish(b.value);
        });
      }
      if (b.left) btn.classList.add("left");
      foot.append(btn);
    }
    function showError(msg) {
      errBox.textContent = msg;
      errBox.hidden = !msg;
    }
    form.addEventListener("submit", async (e) => {
      e.preventDefault();
      if (!onSubmit) { finish(true); return; }
      const submitBtn = foot.querySelector("button[type=submit]");
      if (submitBtn && submitBtn.classList.contains("busy")) return;
      if (submitBtn) { submitBtn.classList.add("busy"); submitBtn.setAttribute("aria-busy", "true"); }
      try {
        const r = await onSubmit(form, showError);
        if (r === false) return;
        finish(r === undefined ? true : r);
      } catch (err) {
        showError(err.message || String(err));
      } finally {
        if (submitBtn) { submitBtn.classList.remove("busy"); submitBtn.removeAttribute("aria-busy"); }
      }
    });
    closeBtn.addEventListener("click", () => finish(null));
    dlg.addEventListener("close", () => finish(null));
    dlg.addEventListener("click", (e) => { if (e.target === dlg) finish(null); });
    form.append(head, bodyEl, foot);
    dlg.append(form);
    document.body.append(dlg);
    dlg.showModal();
    const focusEl = initialFocus ? form.querySelector(initialFocus) : form.querySelector("input, textarea, select");
    if (focusEl) focusEl.focus();
  });
}

// confirm names the thing in its title ("Stop example.com?") so a tired
// operator with three tabs open sees what is about to happen, not "Are
// you sure?".
export function confirm({ title, body, confirmLabel, danger, extra }) {
  return open({
    title,
    body: [body ? h("p", null, body) : null, extra || null],
    buttons: [
      { label: t("common.cancel"), value: false },
      { label: confirmLabel || t("common.confirm"), value: true, primary: true, danger: Boolean(danger) },
    ],
  }).then((v) => v === true);
}

export function field(label, input, hint) {
  const id = input.id || ("f" + Math.random().toString(36).slice(2, 8));
  input.id = id;
  return h("div", { class: "field" },
    h("label", { for: id }, label),
    input,
    hint ? h("div", { class: "hint" }, hint) : null,
  );
}

export function checkbox(label, attrs) {
  const input = h("input", { type: "checkbox", ...attrs });
  return { el: h("label", { class: "check" }, input, h("span", null, label)), input };
}

export function toggle(label, attrs) {
  const input = h("input", { type: "checkbox", role: "switch", ...attrs });
  const el = h("label", { class: "switch" }, input, h("span", { class: "track", "aria-hidden": "true" }), h("span", null, label));
  return { el, input };
}
