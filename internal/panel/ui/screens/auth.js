// Login and first-run setup. The setup screen shows when /auth/me says no
// operator exists yet; the one-time code comes from `middleware setup` on
// the server, so whoever can read that terminal is the operator.

import { h, svg } from "../lib/dom.js";
import { t } from "../lib/i18n.js";
import { api } from "../lib/api.js";
import { field } from "../components/dialog.js";
import { store } from "../lib/store.js";
import * as passkeys from "../lib/passkeys.js";

export function authScreen({ mode, onDone }) {
  const el = h("div", { class: "auth" });
  const card = h("div", { class: "card" });
  el.append(card);
  let current = mode;

  function render() {
    while (card.firstChild) card.removeChild(card.firstChild);
    const err = h("div", { class: "error-box", role: "alert", hidden: true });
    const password = h("input", { type: "password", autocomplete: current === "setup" ? "new-password" : "current-password", required: true, minlength: current === "setup" ? "8" : null });
    const form = h("form", { novalidate: true });
    form.append(h("div", { class: "brand" },
      svg("svg", { class: "mark", viewBox: "0 0 24 24", width: 24, height: 24, "aria-hidden": "true" },
        svg("rect", { width: 24, height: 24, rx: 6, fill: "currentColor" }),
        svg("path", { d: "M6 17V8l6 6 6-6v9", fill: "none", stroke: "var(--surface)", "stroke-width": 2.4, "stroke-linejoin": "round", "stroke-linecap": "round" })),
      h("span", null, t("app.title"))));
    const submit = h("button", { type: "submit", class: "btn primary" }, current === "setup" ? t("auth.create") : t("auth.signIn"));
    // A passkey, when one exists for this name and the browser can use it.
    // With the password closed, it is the only way in.
    const me = store.get("me") || {};
    const closed = current !== "setup" && me.password_closed;
    let code = null;
    if (current === "setup") {
      code = h("input", { type: "text", autocomplete: "one-time-code", required: true, class: "mono", autocapitalize: "off", spellcheck: "false" });
      form.append(
        h("h1", null, t("auth.setupTitle")),
        h("p", { class: "lead" }, t("auth.setupLead")),
        field(t("auth.code"), code, t("auth.codeHint")),
        field(t("auth.password"), password, t("auth.passwordHint")),
      );
    } else {
      form.append(h("h1", null, t("auth.loginTitle")), h("p", { class: "lead" }, t("auth.loginLead")));
      if (!closed) form.append(field(t("auth.password"), password));
    }
    let pkBtn = null;
    if (current !== "setup" && me.passkeys && passkeys.supported()) {
      pkBtn = h("button", { type: "button", class: "btn " + (closed ? "primary" : "") }, t("auth.withPasskey"));
      pkBtn.addEventListener("click", async () => {
        err.hidden = true;
        pkBtn.classList.add("busy");
        try { await passkeys.login(); onDone(); }
        catch (ex) { err.textContent = ex.name === "NotAllowedError" ? t("passkeys.cancelled") : (ex.status === 401 ? t("auth.passkeyWrong") : ex.message); err.hidden = false; }
        finally { pkBtn.classList.remove("busy"); }
      });
    }
    if (closed) form.append(h("p", { class: "small muted" }, t("auth.passwordClosed")));
    form.append(err, h("div", { class: "form-actions" }, closed ? null : submit, pkBtn));
    form.append(h("p", { class: "small muted" },
      current === "setup"
        ? h("a", { href: "#", onClick: (e) => { e.preventDefault(); current = "login"; render(); } }, t("auth.haveAccount"))
        : h("a", { href: "#", onClick: (e) => { e.preventDefault(); current = "setup"; render(); } }, t("auth.firstRun")),
    ));
    form.addEventListener("submit", async (e) => {
      e.preventDefault();
      err.hidden = true;
      if (!password.value || (code && !code.value)) {
        err.textContent = t("auth.fillIn");
        err.hidden = false;
        return;
      }
      submit.classList.add("busy");
      try {
        if (current === "setup") {
          await api.post("/auth/setup", { code: code.value.trim(), password: password.value }, { allow401: true });
          await api.post("/auth/login", { password: password.value }, { allow401: true });
        } else {
          await api.post("/auth/login", { password: password.value }, { allow401: true });
        }
        onDone();
      } catch (ex) {
        err.textContent = ex.status === 401 || ex.status === 403 ? t("auth.wrong") : ex.message;
        err.hidden = false;
        password.focus();
        password.select();
      } finally {
        submit.classList.remove("busy");
      }
    });
    card.append(form);
    (code || password).focus();
  }
  render();
  return { el, destroy() {} };
}
