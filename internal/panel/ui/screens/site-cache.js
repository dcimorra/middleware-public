// The house's response cache for one site: its rules, what it holds, and
// a button to empty it. The rules live in the manifest (they travel with
// the folder); this screen only writes them, so nobody has to learn the
// JSON to use it.

import { h, clear, fmtBytes } from "../lib/dom.js";
import { t } from "../lib/i18n.js";
import { api, enc } from "../lib/api.js";
import { store } from "../lib/store.js";
import { kv } from "../components/page.js";
import { toast, toastError } from "../components/toast.js";
import { confirm } from "../components/dialog.js";
import { icon } from "../lib/icons.js";

const MODES = ["origin", "force", "never"];

export function cacheTab(domain) {
  const stats = h("div");
  const rulesBox = h("div");
  const edgeBox = h("div");
  const el = h("div", { class: "cards" },
    h("div", { class: "card" }, h("h2", null, t("cache.title")), h("p", { class: "small muted" }, t("cache.hint")), stats),
    h("div", { class: "card" }, h("h2", null, t("cache.rules")), h("p", { class: "small muted" }, t("cache.rulesHint")), rulesBox),
    h("div", { class: "card" }, h("h2", null, t("cache.edge")), h("p", { class: "small muted" }, t("cache.edgeHint")), edgeBox));

  // The edge's cache rules belong to the zone, and a zone holds other
  // sites: each rule shows whom it reaches, and the other domains' are
  // set apart and need saying so to delete.
  async function loadEdge() {
    const CF = "/modules/cloudflare";
    try {
      const cr = await api.get(`${CF}/cache-rules?domain=${enc(domain)}`);
      const purge = h("button", { type: "button", class: "btn sm" }, icon("refresh"), t("cache.edgePurge"));
      purge.addEventListener("click", async () => {
        try { await api.post(`${CF}/purge`, { domain }); toast(t("cache.edgePurged"), "ok"); } catch (e) { toastError(e); }
      });
      const row = (r) => {
        const del = h("button", { type: "button", class: "btn sm ghost danger" }, icon("trash"));
        del.addEventListener("click", async () => {
          const other = r.scope !== "site";
          if (!(await confirm({ title: t("cache.edgeDeleteQ"), body: other ? t("cache.edgeDeleteOther", { scope: t("cache.scope." + r.scope) }) : r.expression, confirmLabel: t("common.delete"), danger: true }))) return;
          try {
            await api.del(`${CF}/cache-rules/${enc(cr.zone_id)}/${enc(cr.ruleset_id)}/${enc(r.id)}?domain=${enc(domain)}${other ? "&other=1" : ""}`);
            toast(t("cache.edgeDeleted"), "ok"); loadEdge();
          } catch (e) { toastError(e); }
        });
        return h("div", { class: "row cache-rule" },
          h("span", { class: "badge " + (r.scope === "site" ? "ok" : r.scope === "zone" ? "warn" : "") }, t("cache.scope." + r.scope)),
          h("code", { class: "small grow" }, r.expression), h("span", { class: "small dim" }, r.description || ""), del);
      };
      const mine = cr.rules.filter((r) => r.scope !== "other");
      const others = cr.rules.filter((r) => r.scope === "other");
      const path = h("input", { type: "text", class: "mono", placeholder: "/product", "aria-label": t("cache.path") });
      const ttl = h("input", { type: "number", min: "0", value: "3600", "aria-label": t("cache.ttl") });
      const add = h("button", { type: "button", class: "btn sm" }, icon("plus"), t("cache.edgeAdd"));
      add.addEventListener("click", async () => {
        try { await api.post(`${CF}/cache-rules`, { domain, path: path.value.trim() || "/", ttl: Number(ttl.value) || 0 }); toast(t("cache.edgeAdded"), "ok"); loadEdge(); } catch (e) { toastError(e); }
      });
      clear(edgeBox).append(...[
        h("p", { class: "small" }, t("cache.edgeZone", { zone: cr.zone }), " ", purge),
        h("h3", null, t("cache.edgeOrdered")),
        mine.length ? h("div", { class: "stack-sm" }, ...mine.map(row)) : h("p", { class: "small muted" }, t("cache.edgeNone")),
        h("div", { class: "row cache-rule" }, path, ttl, add),
        h("p", { class: "hint" }, t("cache.edgeAddHint")),
        others.length ? h("details", null, h("summary", { class: "small" }, t("cache.edgeOthers", { n: others.length })), h("div", { class: "stack-sm" }, ...others.map(row))) : null,
      ].filter(Boolean));
    } catch (e) {
      clear(edgeBox).append(h("p", { class: "small muted" }, e.status === 404 || e.status === 409 ? t("cache.edgeNeedsModule") : e.message));
    }
  }

  async function loadStats() {
    try {
      const s = await api.get(`/sites/${enc(domain)}/cache`);
      const total = s.hits + s.misses;
      const purge = h("button", { type: "button", class: "btn sm" }, icon("trash"), t("cache.purge"));
      purge.addEventListener("click", async () => {
        try { await api.del(`/sites/${enc(domain)}/cache`); toast(t("cache.purged"), "ok"); loadStats(); } catch (e) { toastError(e); }
      });
      clear(stats).append(kv([
        [t("cache.entries"), String(s.entries)],
        [t("cache.bytes"), fmtBytes(s.bytes) + (s.limit ? " / " + fmtBytes(s.limit) : "")],
        [t("cache.hitRate"), total ? `${Math.round((s.hits * 100) / total)}% (${s.hits} / ${total})` : "—"],
      ]), h("div", { class: "actions" }, purge));
    } catch (e) { clear(stats).append(h("p", { class: "small muted" }, e.message)); }
  }

  function renderRules() {
    const m = (store.site(domain) || {}).manifest || {};
    const cache = m.cache || {};
    let rules = (cache.rules || []).map((r) => ({ ...r }));
    const maxMB = h("input", { type: "number", min: "0", value: cache.max_mb || "", placeholder: t("cache.noLimit") });
    const list = h("div", { class: "stack-sm" });
    function draw() {
      clear(list);
      if (!rules.length) list.append(h("p", { class: "small muted" }, t("cache.noRules")));
      rules.forEach((r, i) => {
        const path = h("input", { type: "text", class: "mono", value: r.path || "/", "aria-label": t("cache.path") });
        path.addEventListener("input", () => { r.path = path.value.trim(); });
        // The three modes as buttons, not a select: they are the one
        // thing to understand here, and this way all three read at once.
        const picks = h("div", { class: "actions" }, ...MODES.map((mode) => {
          const b = h("button", { type: "button", class: "btn sm " + (r.mode === mode ? "primary" : ""), title: t("cache.mode." + mode + "Hint") }, t("cache.mode." + mode));
          b.addEventListener("click", () => { r.mode = mode; if (mode === "force" && !r.ttl) r.ttl = 3600; draw(); });
          return b;
        }));
        const ttl = h("input", { type: "number", min: "1", value: r.ttl || "", hidden: r.mode !== "force", "aria-label": t("cache.ttl"), placeholder: t("cache.ttl") });
        ttl.addEventListener("input", () => { r.ttl = Number(ttl.value) || 0; });
        const del = h("button", { type: "button", class: "btn sm ghost danger", "aria-label": t("common.delete") }, icon("trash"));
        del.addEventListener("click", () => { rules.splice(i, 1); draw(); });
        list.append(h("div", { class: "row cache-rule" }, path, picks, ttl, del));
      });
    }
    draw();
    const add = h("button", { type: "button", class: "btn sm" }, icon("plus"), t("cache.addRule"));
    add.addEventListener("click", () => { rules.push({ path: "/", mode: "origin" }); draw(); });
    const err = h("div", { class: "error-box", hidden: true, role: "alert" });
    const save = h("button", { type: "button", class: "btn primary" }, t("common.save"));
    save.addEventListener("click", async () => {
      err.hidden = true;
      const out = { ...cache, rules: rules.map((r) => ({ path: r.path, mode: r.mode, ...(r.mode === "force" ? { ttl: r.ttl } : {}), ...(r.note ? { note: r.note } : {}) })) };
      if (Number(maxMB.value) > 0) out.max_mb = Number(maxMB.value); else delete out.max_mb;
      if (!out.rules.length) delete out.rules;
      save.classList.add("busy");
      try {
        const updated = await api.patch(`/sites/${enc(domain)}/manifest`, { cache: Object.keys(out).length ? out : null });
        if (updated && updated.domain) store.putSite(updated);
        toast(t("cache.saved"), "ok");
      } catch (e) { err.textContent = e.message; err.hidden = false; }
      finally { save.classList.remove("busy"); }
    });
    clear(rulesBox).append(list, h("div", { class: "actions" }, add),
      h("div", { class: "form-row" }, h("div", { class: "field" }, h("label", null, t("cache.maxMB")), maxMB, h("div", { class: "hint" }, t("cache.maxMBHint")))),
      err, h("div", { class: "form-actions" }, save));
  }

  loadStats();
  renderRules();
  loadEdge();
  const timer = setInterval(() => { if (!document.hidden) loadStats(); }, 10000);
  return { el, destroy() { clearInterval(timer); } };
}
