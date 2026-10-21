// Security for one site: under-attack mode, who gets in by what it claims
// to be (robots, browsers, countries, User-Agent fragments), the paths it
// never serves, and the last requests it refused. Under attack switches at
// once, because it is switched on with a site falling over and "save, then
// scroll down to press" is one step too many; the rules save with their
// own button, through a PATCH that touches `security` only.

import { seenCard } from "./site-seen.js";
import { h, clear, fmtTime, fmtAgo } from "../lib/dom.js";
import { t } from "../lib/i18n.js";
import { api, enc } from "../lib/api.js";
import { store } from "../lib/store.js";
import { table, cell, empty, notice, loading, errorBox } from "../components/page.js";
import { field, toggle, checkbox } from "../components/dialog.js";
import { toast, toastError } from "../components/toast.js";

const lines = (arr) => (arr || []).join("\n");
const parseLines = (s) => s.split(/[\n,]/).map((x) => x.trim()).filter(Boolean);
const DEADLINES = ["", "2h", "6h", "24h"];

export function securityTab(domain) {
  // Three boxes redrawn apart: switching under attack must not wipe rules
  // half typed in the card below it.
  const attackBox = h("div");
  const rulesBox = h("div");
  const blocksBox = h("div");
  const el = h("div", { class: "cards" }, attackBox, rulesBox, blocksBox, seenCard(domain));
  let catalog = null;
  let deadlineLine = null;
  let counts = {};
  let blocks = [];
  let timer = null;

  async function loadAll() {
    clear(attackBox).append(loading());
    try {
      const [cat, waf, b] = await Promise.all([
        api.get("/security/catalog"),
        api.get("/waf").catch(() => ({})),
        api.get(`/sites/${enc(domain)}/security/blocks`).catch(() => []),
      ]);
      catalog = cat;
      counts = (waf.attack && waf.attack.counts) || {};
      blocks = b || [];
      renderAttack();
      renderRules();
      renderBlocks();
    } catch (e) {
      clear(attackBox).append(errorBox(e));
    }
  }

  async function refreshSite() {
    store.putSite(await api.get(`/sites/${enc(domain)}`));
  }

  const manifest = () => (store.site(domain) || {}).manifest || {};
  function renderAttack() { clear(attackBox).append(attackCard(manifest())); }
  function renderRules() { clear(rulesBox).append(rulesCard(manifest().security || {})); }
  function renderBlocks() { clear(blocksBox).append(blocksCard()); }

  // ---- under attack ----
  function attackCard(m) {
    const a = m.attack || ((m.waf && m.waf.under_attack) ? { on: true } : {});
    const sw = toggle(t("security.attack.switch"), { checked: Boolean(a.on) });
    const bits = h("input", { type: "number", min: "8", max: "24", value: a.bits || "", placeholder: "16" });
    const life = h("input", { type: "text", class: "mono", value: a.life || "", placeholder: "30m" });
    const skip = h("textarea", { class: "mono", rows: "3", value: lines(a.skip) });
    const message = h("input", { type: "text", value: a.message || "" });
    const c = counts[domain] || { served: 0, solved: 0 };

    // What the form says now, sent with every switch: the switch carries
    // the difficulty and the exemptions on screen, not the ones saved.
    const body = (extra) => ({
      domain, enabled: sw.input.checked,
      bits: bits.value ? Number(bits.value) : 0, life: life.value.trim(),
      skip: parseLines(skip.value), message: message.value.trim(), ...extra,
    });
    async function send(extra, okMsg) {
      try {
        await api.post("/waf/under-attack", body(extra));
        await refreshSite();
        toast(okMsg, "ok");
      } catch (e) { toastError(e); }
      renderAttack();
    }
    sw.input.addEventListener("change", () => send({}, sw.input.checked ? t("security.attack.on", { domain }) : t("security.attack.off", { domain })));

    // The deadline is never a selected state: "2 hours" stops being true
    // ten minutes later. These are actions, and what is true is the line
    // under them.
    const deadlineBtns = h("div", { class: "actions" }, ...DEADLINES.map((d) => {
      const b = h("button", { type: "button", class: "btn sm" }, d ? t("security.attack.for", { d }) : t("security.attack.permanent"));
      b.addEventListener("click", () => { sw.input.checked = true; send({ enabled: true, for: d }, t("security.attack.on", { domain })); });
      return b;
    }));
    let state;
    if (!a.on) state = h("p", { class: "small muted" }, t("security.attack.offLine"));
    else if (a.until) state = h("p", { class: "small" }, t("security.attack.until", { at: fmtTime(a.until), left: fmtAgo(a.until, t) }));
    else state = notice("warn", t("security.attack.noDeadline"));
    deadlineLine = a.on && a.until ? { el: state, until: a.until } : null;

    const save = h("button", { type: "button", class: "btn" }, t("security.attack.save"));
    save.addEventListener("click", () => send({}, t("security.attack.saved")));

    return h("div", { class: "card" },
      h("h2", null, t("security.attack.title")),
      h("p", { class: "small muted" }, t("security.attack.hint")),
      h("div", { class: "field" }, sw.el),
      state,
      deadlineBtns,
      a.on && !(a.skip || []).length ? notice("", t("security.attack.noSkip")) : null,
      h("div", { class: "form-row" },
        field(t("security.attack.bits"), bits, t("security.attack.bitsHint")),
        field(t("security.attack.life"), life, t("security.attack.lifeHint")),
      ),
      field(t("security.attack.skip"), skip, t("security.attack.skipHint")),
      field(t("security.attack.message"), message, t("security.attack.messageHint")),
      h("p", { class: "small" }, t("security.attack.counts", { served: c.served, solved: c.solved })),
      h("div", { class: "form-actions" }, save),
    );
  }

  // ---- who gets in ----
  function listMode(list) {
    if (list && (list.allow || []).length) return "allow";
    if (list && (list.deny || []).length) return "deny";
    return "";
  }
  function modeSelect(current) {
    const sel = h("select", null,
      h("option", { value: "" }, t("security.mode.everyone")),
      h("option", { value: "allow" }, t("security.mode.allow")),
      h("option", { value: "deny" }, t("security.mode.deny")));
    sel.value = current;
    return sel;
  }

  function rulesCard(sec) {
    const bots = sec.bots || {};
    const denied = new Set((bots.deny || []).map((x) => x.toLowerCase()));
    const fam = catalog.families.map((f) => ({ f, box: checkbox(f.name, { checked: denied.has(f.key) }) }));
    const except = h("input", { type: "text", value: (bots.except || []).join(", ") });
    const noUA = checkbox(t("security.bots.noUA"), { checked: Boolean(bots.no_ua) });
    const w = bots.window || {};
    const from = h("input", { type: "time", value: w.from || "" });
    const to = h("input", { type: "time", value: w.to || "" });
    const zone = h("input", { type: "text", class: "mono", value: w.zone || "", placeholder: Intl.DateTimeFormat().resolvedOptions().timeZone || "" });

    const bMode = modeSelect(listMode(sec.browsers));
    const bChosen = new Set([...(sec.browsers && sec.browsers.allow) || [], ...(sec.browsers && sec.browsers.deny) || []]);
    const br = catalog.browsers.map((b) => ({ b, box: checkbox(b.name, { checked: bChosen.has(b.key), title: b.note || "" }) }));

    const cMode = modeSelect(listMode(sec.countries));
    const countries = h("input", { type: "text", class: "mono", value: [...(sec.countries && sec.countries.allow) || [], ...(sec.countries && sec.countries.deny) || []].join(", ") });

    const agents = h("textarea", { class: "mono", rows: "2", value: lines(sec.agents && sec.agents.deny) });
    const paths = h("textarea", { class: "mono", rows: "3", value: lines(sec.paths) });
    const message = h("input", { type: "text", value: sec.message || "" });
    const err = h("div", { class: "error-box", hidden: true, role: "alert" });
    const save = h("button", { type: "submit", class: "btn primary" }, t("common.save"));

    const form = h("form", { class: "card", novalidate: true },
      h("h2", null, t("security.rules.title")),
      h("p", { class: "small muted" }, t("security.rules.hint")),
      h("h3", null, t("security.bots.title")),
      h("div", { class: "stack-sm" }, ...fam.map(({ f, box }) => h("div", null, box.el, h("div", { class: "hint" }, f.note)))),
      denied.has("search") ? notice("warn", t("security.bots.searchWarn")) : null,
      field(t("security.bots.except"), except, t("security.bots.exceptHint")),
      h("div", { class: "field" }, noUA.el),
      h("div", { class: "form-row" },
        field(t("security.bots.from"), from), field(t("security.bots.to"), to),
        field(t("security.bots.zone"), zone, t("security.bots.zoneHint"))),
      h("p", { class: "hint" }, t("security.bots.windowHint")),
      h("h3", null, t("security.browsers.title")),
      field(t("security.browsers.mode"), bMode),
      h("div", { class: "form-row" }, ...br.map(({ box }) => box.el)),
      h("h3", null, t("security.countries.title")),
      h("div", { class: "form-row" }, field(t("security.countries.mode"), cMode), field(t("security.countries.codes"), countries, t("security.countries.codesHint"))),
      h("p", { class: "small" }, catalog.your_country
        ? t("security.countries.yours", { cc: catalog.your_country })
        : t("security.countries.none")),
      h("h3", null, t("security.agents.title")),
      field(t("security.agents.deny"), agents, t("security.agents.hint")),
      h("h3", null, t("security.paths.title")),
      field(t("security.paths.own"), paths, t("security.paths.hint")),
      h("details", null, h("summary", { class: "small" }, t("security.paths.builtin")), h("p", { class: "small mono" }, catalog.builtin_paths.join("  "))),
      field(t("security.message"), message, t("security.messageHint")),
      err,
      h("div", { class: "form-actions" }, save),
    );

    form.addEventListener("submit", async (e) => {
      e.preventDefault();
      err.hidden = true;
      const out = {};
      const famDeny = fam.filter(({ box }) => box.input.checked).map(({ f }) => f.key);
      const exc = parseLines(except.value);
      const b = {};
      if (famDeny.length) b.deny = famDeny;
      if (exc.length) b.except = exc;
      if (noUA.input.checked) b.no_ua = true;
      if (from.value || to.value) b.window = { from: from.value, to: to.value, ...(zone.value.trim() ? { zone: zone.value.trim() } : {}) };
      if (Object.keys(b).length) out.bots = b;
      const chosen = br.filter(({ box }) => box.input.checked).map(({ b: x }) => x.key);
      if (bMode.value && chosen.length) out.browsers = { [bMode.value]: chosen };
      const cc = parseLines(countries.value).map((x) => x.toUpperCase());
      if (cMode.value && cc.length) out.countries = { [cMode.value]: cc };
      const ag = parseLines(agents.value);
      if (ag.length) out.agents = { deny: ag };
      const ps = parseLines(paths.value);
      if (ps.length) out.paths = ps;
      if (message.value.trim()) out.message = message.value.trim();
      save.classList.add("busy");
      try {
        const updated = await api.patch(`/sites/${enc(domain)}/manifest`, { security: Object.keys(out).length ? out : null });
        if (updated && updated.domain) store.putSite(updated);
        toast(t("security.saved"), "ok");
        renderRules();
      } catch (ex) {
        err.textContent = ex.message;
        err.hidden = false;
      } finally { save.classList.remove("busy"); }
    });
    return form;
  }

  // ---- what was refused ----
  function blocksCard() {
    const reload = h("button", { type: "button", class: "btn sm" }, t("common.reload"));
    reload.addEventListener("click", async () => {
      try { blocks = await api.get(`/sites/${enc(domain)}/security/blocks`); renderBlocks(); } catch (e) { toastError(e); }
    });
    const head = h("div", { class: "log-bar card-pad" }, h("h2", null, t("security.blocks.title")), reload);
    if (!blocks.length) return h("div", { class: "card pad0" }, head, empty(t("security.blocks.none"), t("security.blocks.noneHint"), null, "shield"));
    const rows = blocks.map((b) => h("tr", null,
      cell(h("span", { title: fmtTime(b.at) }, fmtAgo(b.at, t)), t("security.blocks.at")),
      cell(h("code", null, b.ip), t("security.blocks.ip")),
      cell(t("security.reason." + b.reason) + " · " + b.what, t("security.blocks.why")),
      cell(h("code", null, b.path), t("security.blocks.path"), "mono"),
      cell(h("span", { class: "small dim" }, (b.country ? b.country + " · " : "") + (b.agent || "")), t("security.blocks.agent")),
    ));
    return h("div", { class: "card pad0" }, head, table([
      { label: t("security.blocks.at") }, { label: t("security.blocks.ip") }, { label: t("security.blocks.why") },
      { label: t("security.blocks.path") }, { label: t("security.blocks.agent") },
    ], rows, { responsive: true }).el);
  }

  loadAll();
  // The deadline line counts down; a minute is fine-grained enough, and
  // only the line is redrawn.
  timer = setInterval(() => {
    if (deadlineLine) deadlineLine.el.textContent = t("security.attack.until", { at: fmtTime(deadlineLine.until), left: fmtAgo(deadlineLine.until, t) });
  }, 60000);
  return { el, destroy() { clearInterval(timer); } };
}
