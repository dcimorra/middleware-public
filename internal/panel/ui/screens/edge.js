// Edge and domains: what V1's panel did in Cloudflare's dashboard's place.
// Searching domains needs nothing; the rest needs the Cloudflare module,
// and says so when it is off.

import { h, clear, fmtTime } from "../lib/dom.js";
import { t } from "../lib/i18n.js";
import { api, enc } from "../lib/api.js";
import { page, table, cell, empty, errorBox, loading, notice } from "../components/page.js";
import { tabs } from "../components/tabs.js";
import { open, confirm, field, checkbox } from "../components/dialog.js";
import { toast, toastError } from "../components/toast.js";
import { icon } from "../lib/icons.js";

const TABS = ["domains", "dns", "access", "token"];
const CF = "/modules/cloudflare";

export function edgeScreen({ tab }) {
  const active = TABS.includes(tab) ? tab : "domains";
  const p = page({ title: t("nav.edge"), sub: t("edge.sub"), icon: "globe" });
  p.body.append(tabs(TABS.map((k) => ({ key: k, href: `#/edge/${k}`, label: t("edge.tab." + k) })), active));
  const body = h("div", { class: "cards" });
  p.body.append(body);
  const view = { domains: domainsTab, dns: dnsTab, access: accessTab, token: tokenTab }[active]();
  body.append(view);
  return { el: p.el, destroy() {} };
}

// A Cloudflare call that says "turn the module on" instead of a raw 409.
async function cf(path, opts) {
  try { return await api.get(CF + path, opts); }
  catch (e) {
    if (e.status === 404 || e.status === 409) throw new Error(t("edge.needsModule"));
    throw e;
  }
}

function domainsTab() {
  const el = h("div", { class: "cards" });
  const name = h("input", { type: "search", placeholder: t("edge.searchPlaceholder"), autocomplete: "off", spellcheck: "false" });
  const go = h("button", { type: "submit", class: "btn primary" }, icon("search"), t("edge.search"));
  const results = h("div");
  const form = h("form", { class: "row", novalidate: true }, name, go);
  form.addEventListener("submit", async (e) => {
    e.preventDefault();
    const q = name.value.trim().toLowerCase().replace(/\..*$/, "");
    if (!q) return;
    clear(results).append(loading());
    try {
      const a = await api.get(`/domains/search?name=${enc(q)}`);
      const rows = a.results.map((r) => h("tr", null,
        cell(h("strong", null, r.domain), null, "primary"),
        cell(h("span", { class: "badge " + (r.status === "free" ? "ok" : r.status === "taken" ? "" : "warn") }, t("edge.status." + r.status)), t("edge.statusCol")),
        cell(r.status === "free" && !r.sure ? h("span", { class: "small muted" }, t("edge.notSure")) : (r.error ? h("span", { class: "small dim" }, r.error) : ""), ""),
        cell(r.price ? `$${r.price.toFixed(2)}` : h("span", { class: "muted" }, "—"), t("edge.price"), "num")));
      clear(results).append(...[
        a.stale_prices ? notice("warn", t("edge.stalePrices", { at: a.prices_from })) : null,
        table([{ label: t("edge.domain") }, { label: t("edge.statusCol") }, { label: "" }, { label: t("edge.price"), class: "num" }], rows, { responsive: true }).el,
      ].filter(Boolean));
    } catch (ex) { clear(results).append(errorBox(ex)); }
  });
  const mine = h("div");
  el.append(h("div", { class: "card" }, h("h2", null, t("edge.searchTitle")), h("p", { class: "small muted" }, t("edge.searchHint")), form, results),
    h("div", { class: "card pad0" }, h("div", { class: "log-bar card-pad" }, h("h2", null, t("edge.mine"))), mine));
  (async () => {
    clear(mine).append(loading());
    try {
      const list = await cf("/registrar");
      if (!list.length) { clear(mine).append(empty(t("edge.mineNone"), "")); return; }
      const soon = Date.now() + 30 * 86400000;
      clear(mine).append(table([{ label: t("edge.domain") }, { label: t("edge.expires") }, { label: t("edge.autoRenew") }],
        list.map((d) => h("tr", null, cell(d.name, null, "primary"),
          cell(h("span", { class: new Date(d.expires_at) < soon && !d.auto_renew ? "level-error" : "" }, fmtTime(d.expires_at)), t("edge.expires")),
          cell(d.auto_renew ? t("common.yes") : h("span", { class: "level-warn" }, t("edge.no")), t("edge.autoRenew")))), { responsive: true }).el);
    } catch (e) { clear(mine).append(h("p", { class: "small muted card-pad" }, e.message)); }
  })();
  return el;
}

function dnsTab() {
  const el = h("div", { class: "card pad0" });
  const zoneSel = h("select", { "aria-label": t("edge.zone") });
  const list = h("div");
  const add = h("button", { type: "button", class: "btn sm primary" }, icon("plus"), t("edge.addRecord"));
  el.append(h("div", { class: "log-bar card-pad" }, h("h2", { class: "grow" }, t("edge.tab.dns")), zoneSel, add), list);
  async function load() {
    clear(list).append(loading());
    try {
      const recs = await cf(`/zones/${enc(zoneSel.value)}/records`);
      recs.sort((a, b) => a.name.localeCompare(b.name) || a.type.localeCompare(b.type));
      clear(list).append(table([{ label: t("edge.name") }, { label: t("edge.type") }, { label: t("edge.content") }, { label: t("edge.proxied") }, { label: "" }],
        recs.map((r) => {
          const ed = h("button", { type: "button", class: "btn sm ghost" }, icon("edit"));
          ed.addEventListener("click", () => edit(r));
          const del = h("button", { type: "button", class: "btn sm ghost danger" }, icon("trash"));
          del.addEventListener("click", async () => {
            if (!(await confirm({ title: t("edge.deleteQ", { name: r.name }), body: `${r.type} ${r.content}`, confirmLabel: t("common.delete"), danger: true }))) return;
            try { await api.del(`${CF}/zones/${enc(zoneSel.value)}/records/${enc(r.id)}`); toast(t("edge.deleted"), "ok"); load(); } catch (e) { toastError(e); }
          });
          return h("tr", null, cell(h("code", null, r.name), null, "primary"), cell(r.type, t("edge.type")),
            cell(h("code", { class: "small" }, r.content), t("edge.content"), "mono"), cell(r.proxied ? t("common.yes") : "", t("edge.proxied")),
            cell(h("div", { class: "actions" }, ed, del), null, "right"));
        }), { responsive: true }).el);
    } catch (e) { clear(list).append(errorBox(e)); }
  }
  async function edit(rec) {
    const type = h("select", null, ...["A", "AAAA", "CNAME", "TXT", "MX"].map((v) => h("option", { value: v }, v)));
    type.value = rec ? rec.type : "A";
    const name = h("input", { type: "text", class: "mono", value: rec ? rec.name : "" });
    const content = h("input", { type: "text", class: "mono", value: rec ? rec.content : "" });
    const proxied = checkbox(t("edge.proxied"), { checked: rec ? rec.proxied : false });
    const ok = await open({
      title: rec ? t("edge.editRecord") : t("edge.addRecord"),
      body: [h("div", { class: "form-row" }, field(t("edge.type"), type), field(t("edge.name"), name)), field(t("edge.content"), content), h("div", { class: "field" }, proxied.el)],
      buttons: [{ label: t("common.cancel"), value: null }, { label: t("common.save"), submit: true, primary: true }],
      onSubmit: async () => {
        const body = { type: type.value, name: name.value.trim(), content: content.value.trim(), proxied: proxied.input.checked, ttl: 1 };
        if (rec) await api.put(`${CF}/zones/${enc(zoneSel.value)}/records/${enc(rec.id)}`, body);
        else await api.post(`${CF}/zones/${enc(zoneSel.value)}/records`, body);
      },
    });
    if (ok) { toast(t("edge.saved"), "ok"); load(); }
  }
  add.addEventListener("click", () => edit(null));
  zoneSel.addEventListener("change", load);
  (async () => {
    try {
      const zones = await cf("/zones");
      zones.sort((a, b) => a.name.localeCompare(b.name)).forEach((z) => zoneSel.append(h("option", { value: z.id }, z.name)));
      if (zones.length) load(); else clear(list).append(empty(t("edge.noZones"), ""));
    } catch (e) { clear(list).append(h("p", { class: "small muted card-pad" }, e.message)); add.disabled = true; }
  })();
  return el;
}

function accessTab() {
  const el = h("div", { class: "card pad0" });
  el.append(h("div", { class: "log-bar card-pad" }, h("h2", null, t("edge.tab.access"))), h("p", { class: "small muted card-pad" }, t("edge.accessHint")));
  const list = h("div");
  el.append(list);
  (async () => {
    try {
      const apps = await cf("/access");
      clear(list).append(apps.length ? table([{ label: t("edge.domain") }, { label: t("edge.name") }, { label: t("edge.type") }],
        apps.map((a) => h("tr", null, cell(h("code", null, a.domain), null, "primary"), cell(a.name, t("edge.name")), cell(a.type, t("edge.type")))), { responsive: true }).el
        : empty(t("edge.accessNone"), ""));
    } catch (e) { clear(list).append(h("p", { class: "small muted card-pad" }, e.message)); }
  })();
  return el;
}

function tokenTab() {
  const el = h("div", { class: "card" });
  el.append(h("h2", null, t("edge.tab.token")), loading());
  (async () => {
    try {
      const s = await cf("/token");
      const narrow = h("button", { type: "button", class: "btn primary" }, t("edge.narrow"));
      narrow.addEventListener("click", async () => {
        if (!(await confirm({ title: t("edge.narrowQ"), body: t("edge.narrowBody"), confirmLabel: t("edge.narrow") }))) return;
        narrow.classList.add("busy");
        try { const r = await api.post(`${CF}/token/narrow`); toast(t("edge.narrowed", { id: r.revoke || "?" }), "ok"); } catch (e) { toastError(e); }
        finally { narrow.classList.remove("busy"); }
      });
      clear(el).append(...[h("h2", null, t("edge.tab.token")), h("p", { class: "small muted" }, t("edge.tokenHint")),
        h("p", null, h("strong", null, t("edge.has")), " ", s.permissions.join(", ") || "—"),
        s.extra.length ? notice("warn", t("edge.wide", { list: s.extra.join(", ") })) : notice("ok", t("edge.narrowAlready")),
        s.missing.length ? notice("bad", t("edge.missing", { list: s.missing.join(", ") })) : null,
        h("div", { class: "form-actions" }, narrow)].filter(Boolean));
    } catch (e) { clear(el).append(h("h2", null, t("edge.tab.token")), h("p", { class: "small muted" }, e.message)); }
  })();
  return el;
}
