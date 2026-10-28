// Edge and domains: what V1's panel did in Cloudflare's dashboard's place.
// Searching domains needs nothing; the rest needs the Cloudflare module,
// and says so when it is off.
//
// Off, each tab used to say it in a grey line of small print, under a
// zone picker with nothing in it and a disabled "Add a record": now each
// says it once, as an empty state with the way to Modules in it.

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
  p.el.classList.add("np-page");
  p.body.append(tabs(TABS.map((k) => ({ key: k, href: `#/edge/${k}`, label: t("edge.tab." + k) })), active));
  const body = h("div", { class: "np-stack" });
  p.body.append(body);
  const view = { domains: domainsTab, dns: dnsTab, access: accessTab, token: tokenTab }[active]();
  body.append(view);
  return { el: p.el, destroy() {} };
}

// A Cloudflare call that says "turn the module on" instead of a raw 409.
async function cf(path, opts) {
  try { return await api.get(CF + path, opts); }
  catch (e) {
    if (e.status === 404 || e.status === 409) {
      const off = new Error(t("edge.needsModule"));
      off.moduleOff = true;
      throw off;
    }
    throw e;
  }
}

// failed is what a tab shows when Cloudflare cannot be asked: the way to
// turn the module on when that is why, the error itself otherwise.
function failed(e) {
  if (e && e.moduleOff) {
    const el = empty(t("edge.offTitle"), t("edge.needsModule"), h("a", { class: "btn primary", href: "#/modules" }, icon("blocks"), t("edge.toModules")), "blocks");
    el.classList.add("np-empty");
    return el;
  }
  return h("div", { class: "card-pad" }, errorBox(e));
}

function domainsTab() {
  const el = h("div", { class: "np-stack" });
  const name = h("input", { type: "search", placeholder: t("edge.searchPlaceholder"), autocomplete: "off", spellcheck: "false", "aria-label": t("edge.searchTitle") });
  const go = h("button", { type: "submit", class: "btn primary" }, icon("search"), t("edge.search"));
  const results = h("div", { class: "np-results" });
  const form = h("form", { class: "np-search", novalidate: true }, name, go);
  form.addEventListener("submit", async (e) => {
    e.preventDefault();
    const q = name.value.trim().toLowerCase().replace(/\..*$/, "");
    if (!q) { name.focus(); return; }
    clear(results).append(loading());
    go.classList.add("busy");
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
    finally { go.classList.remove("busy"); }
  });
  const mine = h("div");
  el.append(h("div", { class: "card" }, h("h2", null, t("edge.searchTitle")), h("p", { class: "np-lead" }, t("edge.searchHint")), form, results),
    h("div", { class: "card pad0" }, h("div", { class: "log-bar card-pad np-card-head" }, h("h2", null, t("edge.mine"))), mine),
    pricesCard());
  (async () => {
    clear(mine).append(h("div", { class: "card-pad" }, loading()));
    // Every zone of the account, not only what was bought at Cloudflare's
    // registrar: a domain bought elsewhere and run in Cloudflare is the
    // usual case, and this list said "no domains" for an account full of
    // them. The registrar's facts (expiry, renewal) go beside the ones
    // bought there; a token that cannot read the registrar still lists the
    // zones.
    try {
      const [zones, reg] = await Promise.all([cf("/zones"), cf("/registrar").catch(() => null)]);
      const byName = new Map((reg || []).map((d) => [d.name, d]));
      const names = new Set([...(zones || []).map((z) => z.name), ...byName.keys()]);
      if (!names.size) { clear(mine).append(h("p", { class: "np-none card-pad" }, t("edge.mineNone"))); return; }
      const zoneOf = new Map((zones || []).map((z) => [z.name, z]));
      const soon = Date.now() + 30 * 86400000;
      const rows = [...names].sort().map((name) => {
        const z = zoneOf.get(name), d = byName.get(name);
        return h("tr", null, cell(name, null, "primary"),
          cell(z ? h("span", { class: "badge " + (z.status === "active" ? "ok" : "warn") }, z.status || "—") : h("span", { class: "muted" }, t("edge.noZone")), t("edge.zoneStatus")),
          cell(d ? t("edge.atCloudflare") : (reg ? t("edge.elsewhere") : "—"), t("edge.registrar")),
          cell(d ? h("span", { class: new Date(d.expires_at) < soon && !d.auto_renew ? "level-error" : "" }, fmtTime(d.expires_at)) : "—", t("edge.expires")),
          cell(d ? (d.auto_renew ? t("common.yes") : h("span", { class: "level-warn" }, t("edge.no"))) : "—", t("edge.autoRenew")));
      });
      clear(mine).append(table([{ label: t("edge.domain") }, { label: t("edge.zoneStatus") }, { label: t("edge.registrar") }, { label: t("edge.expires") }, { label: t("edge.autoRenew") }], rows, { responsive: true }).el,
        reg ? "" : h("p", { class: "small muted card-pad" }, t("edge.noRegistrarRead")));
    } catch (e) { clear(mine).append(failed(e)); }
  })();
  return el;
}

function dnsTab() {
  const el = h("div", { class: "card pad0" });
  const zoneSel = h("select", { class: "np-zone", "aria-label": t("edge.zone") });
  const list = h("div", null, h("div", { class: "card-pad" }, loading()));
  const add = h("button", { type: "button", class: "btn primary", hidden: true }, icon("plus"), t("edge.addRecord"));
  zoneSel.hidden = true;
  el.append(h("div", { class: "log-bar card-pad np-card-head" }, h("h2", { class: "grow" }, t("edge.tab.dns")), zoneSel, add), list);
  async function load() {
    clear(list).append(h("div", { class: "card-pad" }, loading()));
    try {
      const recs = await cf(`/zones/${enc(zoneSel.value)}/records`);
      recs.sort((a, b) => a.name.localeCompare(b.name) || a.type.localeCompare(b.type));
      if (!recs.length) { clear(list).append(h("p", { class: "np-none card-pad" }, t("edge.noRecords"))); return; }
      clear(list).append(table([{ label: t("edge.name") }, { label: t("edge.type") }, { label: t("edge.content") }, { label: t("edge.proxied") }, { label: "" }],
        recs.map((r) => {
          const ed = h("button", { type: "button", class: "icon-btn sm", title: t("edge.editRecord"), "aria-label": t("edge.editRecord") }, icon("edit"));
          ed.addEventListener("click", () => edit(r));
          const del = h("button", { type: "button", class: "icon-btn sm np-danger", title: t("common.delete"), "aria-label": t("edge.deleteQ", { name: r.name }) }, icon("trash"));
          del.addEventListener("click", async () => {
            if (!(await confirm({ title: t("edge.deleteQ", { name: r.name }), body: `${r.type} ${r.content}`, confirmLabel: t("common.delete"), danger: true }))) return;
            try { await api.del(`${CF}/zones/${enc(zoneSel.value)}/records/${enc(r.id)}`); toast(t("edge.deleted"), "ok"); load(); } catch (e) { toastError(e); }
          });
          return h("tr", null, cell(h("code", null, r.name), null, "primary"), cell(h("span", { class: "badge" }, r.type), t("edge.type")),
            cell(h("code", { class: "small np-wrap" }, r.content), t("edge.content"), "mono"),
            cell(r.proxied ? h("span", { class: "status" }, h("span", { class: "dot ok" }), t("common.yes")) : h("span", { class: "muted" }, t("edge.no")), t("edge.proxied")),
            cell(h("div", { class: "np-row-actions" }, ed, del), null, "right"));
        }), { responsive: true }).el);
    } catch (e) { clear(list).append(failed(e)); }
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
      if (zones.length) { zoneSel.hidden = false; add.hidden = false; load(); }
      else { const e = empty(t("edge.noZones"), ""); e.classList.add("np-empty"); clear(list).append(e); }
    } catch (e) { clear(list).append(failed(e)); }
  })();
  return el;
}

function accessTab() {
  const el = h("div", { class: "card pad0" });
  el.append(h("div", { class: "log-bar card-pad np-card-head" }, h("h2", null, t("edge.tab.access"))));
  const list = h("div", null, h("div", { class: "card-pad" }, loading()));
  el.append(list);
  (async () => {
    try {
      const apps = await cf("/access");
      clear(list).append(h("p", { class: "np-lead card-pad" }, t("edge.accessHint")), apps.length ? table([{ label: t("edge.domain") }, { label: t("edge.name") }, { label: t("edge.type") }],
        apps.map((a) => h("tr", null, cell(h("code", null, a.domain), null, "primary"), cell(a.name, t("edge.name")), cell(a.type, t("edge.type")))), { responsive: true }).el
        : h("p", { class: "np-none card-pad" }, t("edge.accessNone")));
    } catch (e) { clear(list).append(failed(e)); }
  })();
  return el;
}

function tokenTab() {
  const el = h("div", { class: "card" });
  el.append(h("h2", null, t("edge.tab.token")), loading());
  (async () => {
    try {
      const s = await cf("/token");
      if (s.unreadable) {
        clear(el).append(h("h2", null, t("edge.tab.token")), notice("warn", t("edge.unreadable")));
        return;
      }
      // Narrowing is the thing to do only when the token allows more than
      // the house uses: then it is the primary action, otherwise a quiet one.
      const narrow = h("button", { type: "button", class: "btn" + (s.extra.length ? " primary" : "") }, icon("key"), t("edge.narrow"));
      narrow.addEventListener("click", async () => {
        if (!(await confirm({ title: t("edge.narrowQ"), body: t("edge.narrowBody"), confirmLabel: t("edge.narrow") }))) return;
        narrow.classList.add("busy");
        try { const r = await api.post(`${CF}/token/narrow`); toast(t("edge.narrowed", { id: r.revoke || "?" }), "ok"); } catch (e) { toastError(e); }
        finally { narrow.classList.remove("busy"); }
      });
      const extra = new Set(s.extra);
      clear(el).append(...[h("h2", null, t("edge.tab.token")), h("p", { class: "np-lead" }, t("edge.tokenHint")),
        h("div", { class: "np-perms" }, h("span", { class: "np-perms-label" }, t("edge.has")),
          ...(s.permissions.length ? s.permissions.map((x) => h("span", { class: "badge " + (extra.has(x) ? "warn" : "") }, x)) : [h("span", { class: "muted" }, "—")])),
        s.extra.length ? notice("warn", t("edge.wide", { list: s.extra.join(", ") })) : notice("ok", t("edge.narrowAlready")),
        s.missing.length ? notice("bad", t("edge.missing", { list: s.missing.join(", ") })) : null,
        h("div", { class: "form-actions" }, narrow)].filter(Boolean));
    } catch (e) {
      clear(el).append(h("h2", null, t("edge.tab.token")), failed(e));
    }
  })();
  return el;
}

// The prices the search shows: written by hand, one ending per line, and
// dated when saved, so that six months on the search says they may be old.
// Folded: written once in a while, and a textarea eight lines tall under
// the search on every visit.
function pricesCard() {
  const el = h("div", { class: "card" });
  (async () => {
    let p = { usd: {} };
    try { p = await api.get("/domains/prices"); } catch (e) { el.append(h("h2", null, t("edge.prices")), errorBox(e)); return; }
    const text = h("textarea", { class: "mono", rows: "8", placeholder: "com 10.44\nes 9.50", spellcheck: "false", "aria-label": t("edge.prices") });
    text.value = Object.entries(p.usd || {}).sort(([a], [b]) => a.localeCompare(b)).map(([k, v]) => `${k} ${v}`).join("\n");
    const save = h("button", { type: "button", class: "btn primary" }, t("common.save"));
    const when = h("span", { class: "np-quiet" }, p.updated ? t("edge.pricesFrom", { at: p.updated }) : t("edge.pricesNever"));
    save.addEventListener("click", async () => {
      const usd = {};
      for (const line of text.value.split("\n")) {
        const [k, v] = line.trim().split(/[\s=:]+/);
        if (!k) continue;
        const n = Number(String(v || "").replace(",", "."));
        if (!(n >= 0)) { toastError(new Error(t("edge.pricesBad", { line }))); return; }
        usd[k.replace(/^\./, "")] = n;
      }
      save.classList.add("busy");
      try { const r = await api.put("/domains/prices", { usd }); when.textContent = t("edge.pricesFrom", { at: r.updated }); toast(t("edge.saved"), "ok"); }
      catch (e) { toastError(e); } finally { save.classList.remove("busy"); }
    });
    const n = Object.keys(p.usd || {}).length;
    el.append(h("details", { class: "np-fold np-fold-card" },
      h("summary", null, h("span", { class: "np-fold-title" }, t("edge.prices")), h("span", { class: "np-quiet" }, n ? t("edge.pricesCount", { n }) : t("edge.pricesNever"))),
      h("div", { class: "np-fold-body np-fold-form" }, h("p", { class: "np-lead" }, t("edge.pricesHint")), text, h("div", { class: "form-actions np-prices-foot" }, save, when))));
  })();
  return el;
}
