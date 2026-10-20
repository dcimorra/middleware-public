// Access & WAF for one site: forms over the manifest's access and waf
// sections, saved with a PATCH that only touches those two keys so a
// concurrent edit to run or deploy is never overwritten by this form.

import { h } from "../lib/dom.js";
import { t } from "../lib/i18n.js";
import { api, enc } from "../lib/api.js";
import { store } from "../lib/store.js";
import { field } from "../components/dialog.js";
import { toast, toastError } from "../components/toast.js";

const lines = (arr) => (arr || []).join("\n");
const parseLines = (s) => s.split(/[\n,]/).map((x) => x.trim()).filter(Boolean);

export function accessTab(domain) {
  const s = store.site(domain);
  const m = s.manifest || {};
  const access = m.access || {};
  const waf = m.waf || {};
  const rate = waf.rate || {};

  const password = h("input", { type: "password", autocomplete: "new-password", value: access.password || "" });
  const allow = h("textarea", { class: "mono", value: lines(access.allow), rows: "3" });
  const denyIPs = h("textarea", { class: "mono", value: lines(access.deny), rows: "3" });
  const deny = h("input", { type: "text", class: "mono", value: (access.deny_countries || []).join(", ") });
  const routes = h("textarea", { class: "mono", rows: "4", value: Object.keys(access.routes || {}).length ? JSON.stringify(access.routes, null, 2) : "" });

  const requests = h("input", { type: "number", min: "0", value: rate.requests ?? "" });
  const window_ = h("input", { type: "text", class: "mono", placeholder: "1m", value: rate.window || "" });
  const burst = h("input", { type: "number", min: "0", value: rate.burst ?? "" });
  const maxBody = h("input", { type: "text", class: "mono", placeholder: "20m", value: waf.max_body || "" });

  const save = h("button", { type: "submit", class: "btn primary" }, t("common.save"));
  const err = h("div", { class: "error-box", hidden: true, role: "alert" });
  const form = h("form", { novalidate: true },
    h("div", { class: "card" },
      h("h2", null, t("access.title")),
      h("p", { class: "small muted" }, t("access.hint")),
      field(t("access.password"), password, t("access.passwordHint")),
      field(t("access.allow"), allow, t("access.allowHint")),
      field(t("access.deny"), denyIPs, t("access.denyIPsHint")),
      field(t("access.denyCountries"), deny, t("access.denyHint")),
      field(t("access.routes"), routes, t("access.routesHint")),
    ),
    h("div", { class: "card" },
      h("h2", null, t("access.wafTitle")),
      h("div", { class: "form-row" },
        field(t("access.rateRequests"), requests),
        field(t("access.rateWindow"), window_),
        field(t("access.rateBurst"), burst),
        field(t("access.maxBody"), maxBody, t("access.maxBodyHint")),
      ),
      h("p", { class: "small muted" }, t("access.attackMoved")),
      err,
      h("div", { class: "form-actions" }, save),
    ),
  );

  form.addEventListener("submit", async (e) => {
    e.preventDefault();
    err.hidden = true;
    let routesObj;
    try {
      routesObj = routes.value.trim() ? JSON.parse(routes.value) : undefined;
      if (routesObj !== undefined && (typeof routesObj !== "object" || Array.isArray(routesObj))) throw new Error(t("access.routesBad"));
    } catch (ex) {
      err.textContent = t("access.routesBad") + ": " + ex.message;
      err.hidden = false;
      routes.focus();
      return;
    }
    const newAccess = {};
    if (password.value) newAccess.password = password.value;
    // Not on this form, so carried as it was: the PATCH replaces the
    // whole access section and would drop it.
    if (access.message) newAccess.message = access.message;
    const al = parseLines(allow.value); if (al.length) newAccess.allow = al;
    const di = parseLines(denyIPs.value); if (di.length) newAccess.deny = di;
    const dc = parseLines(deny.value).map((x) => x.toUpperCase()); if (dc.length) newAccess.deny_countries = dc;
    if (routesObj && Object.keys(routesObj).length) newAccess.routes = routesObj;
    const newWaf = {};
    const r = {};
    if (requests.value !== "") r.requests = Number(requests.value);
    if (window_.value) r.window = window_.value.trim();
    if (burst.value !== "") r.burst = Number(burst.value);
    if (Object.keys(r).length) newWaf.rate = r;
    if (maxBody.value) newWaf.max_body = maxBody.value.trim();
    // Saving must not switch off a manifest under attack through the old
    // boolean; the Security tab's switch moves it to `attack`.
    if (waf.under_attack && !m.attack) newWaf.under_attack = true;
    save.classList.add("busy");
    try {
      const updated = await api.patch(`/sites/${enc(domain)}/manifest`, { access: newAccess, waf: newWaf });
      if (updated && updated.domain) store.putSite(updated);
      toast(t("access.saved"), "ok");
    } catch (ex) {
      err.textContent = ex.message;
      err.hidden = false;
    } finally { save.classList.remove("busy"); }
  });

  return { el: form, destroy() {} };
}
