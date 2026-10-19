// Settings: what this installation decided that is not about one site.
// The server part is data/settings.json through GET/PUT; theme and
// language are this browser's and never leave it.

import { h, clear, copy } from "../lib/dom.js";
import { t, LANGUAGES, currentLang } from "../lib/i18n.js";
import { api, enc } from "../lib/api.js";
import { page, load, table, cell, empty, kv } from "../components/page.js";
import { open, confirm, field } from "../components/dialog.js";
import { toast, toastError } from "../components/toast.js";
import { getTheme, setTheme, setLang } from "../lib/theme.js";
import { fmtTime, fmtAgo } from "../lib/dom.js";

const PAGES = ["unknown", "stopped", "hold", "unavailable"];

export function settingsScreen() {
  const p = page({ title: t("nav.settings") });
  const general = h("div", { class: "card" });
  const pagesCard = h("div", { class: "card" });
  const tokensCard = h("div", { class: "card pad0" });
  const localCard = h("div", { class: "card" });
  p.body.append(general, pagesCard, tokensCard, localCard);

  async function renderServer() {
    const s = await api.get("/settings");
    const panelDomain = h("input", { type: "text", class: "mono", value: s.panel_domain || "", placeholder: "panel.example.com", autocapitalize: "off" });
    const listen = h("input", { type: "text", class: "mono", value: s.listen || "", placeholder: "127.0.0.1:46100" });
    const from = h("input", { type: "number", min: "1024", max: "65535", value: (s.ports && s.ports.from) || "" });
    const to = h("input", { type: "number", min: "1024", max: "65535", value: (s.ports && s.ports.to) || "" });
    const save = h("button", { type: "submit", class: "btn primary" }, t("common.save"));
    const err = h("div", { class: "error-box", hidden: true });
    const pages = {};
    const pagesForm = h("div");
    for (const key of PAGES) {
      const cur = (s.pages && s.pages[key]) || ["", ""];
      const title = h("input", { type: "text", value: cur[0] || "" });
      const message = h("input", { type: "text", value: cur[1] || "" });
      pages[key] = [title, message];
      pagesForm.append(h("details", { class: "section" }, h("summary", null, t("settings.page." + key)), h("div", { class: "section-body form-row" }, field(t("settings.pageTitle"), title), field(t("settings.pageMessage"), message))));
    }
    const form = h("form", { novalidate: true },
      h("div", { class: "form-row" },
        field(t("settings.panelDomain"), panelDomain, t("settings.panelDomainHint")),
        field(t("settings.listen"), listen, t("settings.listenHint")),
      ),
      h("div", { class: "form-row" }, field(t("settings.portFrom"), from), field(t("settings.portTo"), to)),
      h("p", { class: "small muted" }, t("settings.portsHint")),
      err,
      h("div", { class: "form-actions" }, save),
    );
    form.addEventListener("submit", async (e) => {
      e.preventDefault();
      err.hidden = true;
      const f = Number(from.value), tt = Number(to.value);
      if (!(f > 0 && tt > f)) { err.textContent = t("settings.portsBad"); err.hidden = false; return; }
      const out = { ...s, panel_domain: panelDomain.value.trim(), listen: listen.value.trim(), ports: { from: f, to: tt }, pages: {} };
      for (const key of PAGES) {
        const [ti, me] = pages[key];
        if (ti.value || me.value) out.pages[key] = [ti.value, me.value];
      }
      if (Object.keys(out.pages).length === 0) delete out.pages;
      save.classList.add("busy");
      try { await api.put("/settings", out); toast(t("settings.saved"), "ok"); }
      catch (ex) { err.textContent = ex.message; err.hidden = false; }
      finally { save.classList.remove("busy"); }
    });
    clear(general).append(h("h2", null, t("settings.general")), form);
    clear(pagesCard).append(h("h2", null, t("settings.pages")), h("p", { class: "small muted" }, t("settings.pagesHint")), pagesForm, h("p", { class: "small muted" }, t("settings.pagesSaveHint")));
    return null;
  }

  async function renderTokens() {
    const tokens = await api.get("/auth/tokens");
    const newBtn = h("button", { type: "button", class: "btn sm primary", "data-shortcut": "new" }, t("settings.newToken"));
    newBtn.addEventListener("click", newToken);
    const rows = (tokens || []).map((tk) => h("tr", null,
      cell(tk.name || tk.id, null, "primary"),
      cell(h("code", { class: "small" }, tk.id), t("settings.tokenId")),
      cell(tk.created ? h("span", { title: fmtTime(tk.created) }, fmtAgo(tk.created, t)) : "", t("settings.created")),
      cell(tk.last_used ? h("span", { title: fmtTime(tk.last_used) }, fmtAgo(tk.last_used, t)) : h("span", { class: "muted" }, t("settings.neverUsed")), t("settings.lastUsed")),
      cell(revokeBtn(tk), null, "right"),
    ));
    clear(tokensCard).append(
      h("div", { class: "log-bar card-pad" }, h("h2", { class: "grow" }, t("settings.tokens")), newBtn),
      h("p", { class: "small muted card-pad" }, t("settings.tokensHint")),
      rows.length ? table([{ label: t("settings.tokenName") }, { label: t("settings.tokenId") }, { label: t("settings.created") }, { label: t("settings.lastUsed") }, { label: "" }], rows, { responsive: true }).el
        : empty(t("settings.noTokens"), t("settings.noTokensHint")),
    );
    return null;
  }
  function revokeBtn(tk) {
    const b = h("button", { type: "button", class: "btn sm danger ghost" }, t("settings.revoke"));
    b.addEventListener("click", async () => {
      if (!(await confirm({ title: t("settings.revokeQ", { name: tk.name || tk.id }), body: t("settings.revokeBody"), confirmLabel: t("settings.revoke"), danger: true }))) return;
      try { await api.del(`/auth/tokens/${enc(tk.id)}`); toast(t("settings.revoked"), "ok"); load(tokensCard, renderTokens); } catch (e) { toastError(e); }
    });
    return b;
  }
  async function newToken() {
    const name = h("input", { type: "text", placeholder: "ci", required: true });
    const r = await open({
      title: t("settings.newToken"),
      body: field(t("settings.tokenName"), name, t("settings.tokenNameHint")),
      buttons: [{ label: t("common.cancel"), value: null }, { label: t("common.create"), submit: true, primary: true }],
      onSubmit: async (_f, showError) => {
        if (!name.value.trim()) { showError(t("settings.tokenNameRequired")); return false; }
        return api.post("/auth/tokens", { name: name.value.trim() });
      },
    });
    if (!r || !r.token) return;
    const code = h("code", null, r.token);
    const copyBtn = h("button", { type: "button", class: "btn sm" }, t("common.copy"));
    copyBtn.addEventListener("click", async () => { copyBtn.textContent = (await copy(r.token)) ? t("common.copied") : t("common.copyFailed"); });
    await open({
      title: t("settings.tokenCreated"),
      body: [h("p", { class: "notice warn" }, t("settings.tokenOnce")), h("div", { class: "token-box" }, code, copyBtn)],
      buttons: [{ label: t("common.done"), value: true, primary: true }],
    });
    load(tokensCard, renderTokens);
  }

  // ---- local ----
  const theme = h("select", { "aria-label": t("settings.theme") }, ...["auto", "light", "dark"].map((v) => h("option", { value: v, selected: getTheme() === v }, t("theme." + v))));
  theme.addEventListener("change", () => setTheme(theme.value));
  const lang = h("select", { "aria-label": t("settings.language") }, ...LANGUAGES.map((v) => h("option", { value: v, selected: currentLang() === v }, v)));
  lang.addEventListener("change", () => { setLang(lang.value); location.reload(); });
  localCard.append(h("h2", null, t("settings.thisBrowser")), h("div", { class: "form-row" }, field(t("settings.theme"), theme, t("settings.themeHint")), field(t("settings.language"), lang, t("settings.languageHint"))));

  load(general, renderServer);
  load(tokensCard, renderTokens);
  return { el: p.el, destroy() {} };
}
