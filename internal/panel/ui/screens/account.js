// A delegated user's own account: the one thing they change about it is
// their password. The operator's is set on the server (`middleware
// setup`), so this is drawn for delegated users only.

import { h } from "../lib/dom.js";
import { t } from "../lib/i18n.js";
import { api } from "../lib/api.js";
import { open, field } from "../components/dialog.js";
import { toast } from "../components/toast.js";

// passwordDialog changes it; the server ends every session of theirs,
// this one too, so onSignedOut takes them to the login.
export async function passwordDialog(onSignedOut) {
  const current = h("input", { type: "password", autocomplete: "current-password", required: true });
  const next = h("input", { type: "password", autocomplete: "new-password", minlength: "8", required: true });
  const done = await open({
    title: t("account.changePassword"),
    body: [field(t("account.current"), current), field(t("account.new"), next, t("auth.passwordHint")), h("p", { class: "dim small" }, t("account.signsOut"))],
    buttons: [{ label: t("common.cancel"), value: null }, { label: t("common.save"), submit: true, primary: true }],
    onSubmit: async (_f, showError) => {
      if (!current.value || next.value.length < 8) { showError(t("auth.fillIn")); return false; }
      try { await api.put("/auth/password", { current: current.value, password: next.value }, { allow401: true }); return true; }
      catch (e) { showError(e.status === 401 ? t("account.wrongCurrent") : e.message); return false; }
    },
  });
  if (done) { toast(t("account.changed"), "ok"); onSignedOut(); }
}
