// Settings: what this installation decided that is not about one site.
// The server part is data/settings.json through GET/PUT; theme and
// language are this browser's and never leave it.

import { icon } from "../lib/icons.js";
import { h, clear, copy } from "../lib/dom.js";
import { t, LANGUAGES, currentLang } from "../lib/i18n.js";
import { api, enc } from "../lib/api.js";
import { page, load, table, cell, empty, kv } from "../components/page.js";
import { open, confirm, field, toggle, checkbox } from "../components/dialog.js";
import { toast, toastError } from "../components/toast.js";
import { getTheme, setTheme, setLang } from "../lib/theme.js";
import { fmtTime, fmtAgo } from "../lib/dom.js";

const PAGES = ["unknown", "stopped", "hold", "unavailable"];

export function settingsScreen() {
  const p = page({ title: t("nav.settings"), icon: "sliders" });
  const general = h("div", { class: "card" });
  const pagesCard = h("div", { class: "card" });
  const agentsCard = h("div", { class: "card" });
  const tokensCard = h("div", { class: "card pad0" });
  const depsCard = h("div", { class: "card" });
  const localCard = h("div", { class: "card" });
  p.body.append(general, pagesCard, agentsCard, depsCard, tokensCard, localCard);

  // The deps card saves on its own, like the agents one.
  async function renderDeps() {
    const s = await api.get("/settings");
    const on = !(s.deps && s.deps.auto_update === false);
    const sw = toggle(t("settings.depsAutoUpdate"), { checked: on });
    sw.input.addEventListener("change", async () => {
      sw.input.disabled = true;
      try {
        const cur = await api.get("/settings");
        await api.put("/settings", { ...cur, deps: { ...(cur.deps || {}), auto_update: sw.input.checked } });
        toast(t("settings.depsSaved"), "ok");
      } catch (e) { sw.input.checked = !sw.input.checked; toastError(e); }
      finally { sw.input.disabled = false; }
    });
    clear(depsCard).append(h("h2", null, t("settings.deps")), h("p", { class: "small muted" }, t("settings.depsHint")), sw.el);
    return null;
  }

  // The agents card saves on its own: the general form keeps whatever the
  // agents section holds and this one keeps whatever the general one holds,
  // each re-reading the file before it writes so neither overwrites the
  // other's last save with stale fields.
  async function renderAgents() {
    const [s, agents] = await Promise.all([api.get("/settings"), api.get("/agents").catch(() => ({ providers: [] }))]);
    const a = s.agents || {};
    const providers = (agents.providers || []).map((x) => x.provider);
    for (const k of Object.keys(a.per_provider || {})) if (!providers.includes(k)) providers.push(k);
    if (a.default_provider && !providers.includes(a.default_provider)) providers.push(a.default_provider);
    const max = h("input", { type: "number", min: "1", max: "64", value: a.max_concurrent || 8 });
    const reserved = h("input", { type: "number", min: "0", max: "63", value: a.reserved_for_house ?? 1 });
    const def = h("select", null, ...providers.map((k) => h("option", { value: k, selected: k === a.default_provider }, k)));
    const per = {};
    const perFields = providers.map((k) => {
      per[k] = h("input", { type: "number", min: "0", max: "64", value: (a.per_provider && a.per_provider[k]) || 0 });
      return field(t("settings.perProviderCap", { provider: k }), per[k], t("settings.perProviderHint"));
    });
    const save = h("button", { type: "submit", class: "btn primary" }, t("common.save"));
    const err = h("div", { class: "error-box", hidden: true });
    const form = h("form", { novalidate: true },
      h("div", { class: "form-row" },
        field(t("settings.maxConcurrent"), max, t("settings.maxConcurrentHint")),
        field(t("settings.reservedForHouse"), reserved, t("settings.reservedForHouseHint")),
        field(t("settings.defaultProvider"), def, t("settings.defaultProviderHint")),
      ),
      h("div", { class: "form-row" }, ...perFields),
      err,
      h("div", { class: "form-actions" }, save),
    );
    form.addEventListener("submit", async (e) => {
      e.preventDefault();
      err.hidden = true;
      const m = Number(max.value), r = Number(reserved.value);
      const perOut = {};
      let bad = !(Number.isInteger(m) && m >= 1 && m <= 64 && Number.isInteger(r) && r >= 0 && r < m);
      for (const k of providers) {
        const v = Number(per[k].value);
        if (!(Number.isInteger(v) && v >= 0 && v <= 64)) bad = true;
        perOut[k] = v;
      }
      if (bad) { err.textContent = t("settings.agentsBad"); err.hidden = false; return; }
      save.classList.add("busy");
      try {
        const cur = await api.get("/settings");
        await api.put("/settings", { ...cur, agents: { max_concurrent: m, reserved_for_house: r, per_provider: perOut, default_provider: def.value } });
        toast(t("settings.agentsSaved"), "ok");
      } catch (ex) { err.textContent = ex.message; err.hidden = false; }
      finally { save.classList.remove("busy"); }
    });
    clear(agentsCard).append(h("h2", null, t("settings.agents")), h("p", { class: "small muted" }, t("settings.agentsHint")), form);
    return null;
  }

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
      const cur = await api.get("/settings").catch(() => s);
      const out = { ...cur, panel_domain: panelDomain.value.trim(), listen: listen.value.trim(), ports: { from: f, to: tt }, pages: {} };
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
      cell((tk.scopes || []).length ? h("code", { class: "small" }, tk.scopes.join(", ")) : h("span", { class: "muted" }, t("settings.tokenFull")), t("settings.tokenOpens")),
      cell(tk.created ? h("span", { title: fmtTime(tk.created) }, fmtAgo(tk.created, t)) : "", t("settings.created")),
      cell(tk.last_used ? h("span", { title: fmtTime(tk.last_used) }, fmtAgo(tk.last_used, t)) : h("span", { class: "muted" }, t("settings.neverUsed")), t("settings.lastUsed")),
      cell(revokeBtn(tk), null, "right"),
    ));
    clear(tokensCard).append(
      h("div", { class: "log-bar card-pad" }, h("h2", { class: "grow" }, t("settings.tokens")), newBtn),
      h("p", { class: "small muted card-pad" }, t("settings.tokensHint")),
      rows.length ? table([{ label: t("settings.tokenName") }, { label: t("settings.tokenId") }, { label: t("settings.tokenOpens") }, { label: t("settings.created") }, { label: t("settings.lastUsed") }, { label: "" }], rows, { responsive: true }).el
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
    // Full access, or a service door: some of read, restart and deploy,
    // for every site or for one.
    const kind = h("select", null, h("option", { value: "" }, t("settings.tokenKindFull")), h("option", { value: "scoped" }, t("settings.tokenKindScoped")));
    const actions = ["read", "restart", "deploy"].map((a) => ({ a, box: checkbox(t("settings.scope." + a), {}) }));
    const site = h("input", { type: "text", class: "mono", placeholder: "shop.example", autocomplete: "off" });
    const scopedBox = h("div", { hidden: true }, h("div", { class: "field" }, ...actions.map(({ box }) => h("div", null, box.el))), field(t("settings.tokenSite"), site, t("settings.tokenSiteHint")));
    kind.addEventListener("change", () => { scopedBox.hidden = kind.value !== "scoped"; });
    const r = await open({
      title: t("settings.newToken"),
      body: [field(t("settings.tokenName"), name, t("settings.tokenNameHint")), field(t("settings.tokenKind"), kind, t("settings.tokenKindHint")), scopedBox],
      buttons: [{ label: t("common.cancel"), value: null }, { label: t("common.create"), submit: true, primary: true }],
      onSubmit: async (_f, showError) => {
        if (!name.value.trim()) { showError(t("settings.tokenNameRequired")); return false; }
        const body = { name: name.value.trim() };
        if (kind.value === "scoped") {
          const d = site.value.trim().toLowerCase();
          body.scopes = actions.filter(({ box }) => box.input.checked).map(({ a }) => (d ? `${a}:${d}` : a));
          if (!body.scopes.length) { showError(t("settings.tokenScopeRequired")); return false; }
        }
        return api.post("/auth/tokens", body);
      },
    });
    if (!r || !r.token) return;
    const code = h("code", null, r.token);
    const copyBtn = h("button", { type: "button", class: "btn sm" }, icon("copy"), t("common.copy"));
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
  load(agentsCard, renderAgents);
  load(depsCard, renderDeps);
  load(tokensCard, renderTokens);
  return { el: p.el, destroy() {} };
}
