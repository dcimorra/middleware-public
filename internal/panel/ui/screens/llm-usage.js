// What the agents cost, every tool's: how much of each subscription's
// windows is gone, what each project's conversations spent, and what each
// site's code spent through its Claude door — today against its cap, the
// last 7 and 30 days, this month, in total, a bar per day and the models
// it went on. The doors open at zero are listed too: open and unused is a
// fact.

import { h, clear, svg } from "../lib/dom.js";
import { t } from "../lib/i18n.js";
import { api, enc } from "../lib/api.js";
import { page, table, cell, empty, errorBox, loading } from "../components/page.js";
import { usageBlock } from "../components/usage.js";

export function llmUsageScreen() {
  const p = page({ title: t("nav.llm"), sub: t("llmUse.sub"), icon: "gauge" });
  const usage = h("div", { class: "card" });
  const cost = h("div", { class: "card pad0" });
  const range = h("select", { class: "auto", "aria-label": t("claudeUse.days") }, ...[7, 30, 90].map((n) => h("option", { value: String(n) }, t("claudeUse.lastDays", { n }))));
  range.value = "30";
  const body = h("div", { class: "card pad0" });
  p.body.append(h("div", { class: "grid-2" }, usage, cost),
    h("div", { class: "row" }, h("div", { class: "grow" }, h("h2", null, t("llmUse.doors")), h("p", { class: "small muted" }, t("claudeUse.sub"))), range), body);
  const usd = (v) => `$${Number(v || 0).toFixed(2)}`;

  // A bar per day, scaled to the busiest one: a few cents and a few
  // dollars must both show their shape.
  function bars(series, days) {
    const W = 120, H = 28, n = series.length || 1, w = W / n;
    const max = Math.max(...series, 0);
    const s = svg("svg", { class: "bars", viewBox: `0 0 ${W} ${H}`, width: W, height: H, role: "img", "aria-label": t("claudeUse.series") });
    series.forEach((v, i) => {
      const bh = max > 0 ? Math.max(v > 0 ? 1 : 0, (v / max) * (H - 2)) : 0;
      const r = svg("rect", { x: (i * w + 0.5).toFixed(1), y: (H - bh).toFixed(1), width: Math.max(0.5, w - 1).toFixed(1), height: bh.toFixed(1) });
      r.append(svg("title", null, `${days[i]}: ${usd(v)}`));
      s.append(r);
    });
    return s;
  }
  function models(m) {
    const list = Object.entries(m || {}).sort((a, b) => b[1] - a[1]);
    return list.length ? h("span", { class: "small muted" }, list.map(([k, v]) => `${k} ${usd(v)}`).join(" · ")) : "";
  }

  // What an open door may do, said in its permissions.
  function permsLabel(perms) {
    return perms && perms.length ? perms.map((p) => t("door.perm." + p)).join(", ") : t("door.permNone");
  }

  async function load() {
    clear(body).append(loading());
    try {
      const d = await api.get(`/claude/sites?days=${range.value}`);
      if (!d.sites.length) { clear(body).append(empty(t("claudeUse.none"), t("claudeUse.noneHint"))); return; }
      const rows = d.sites.map((s) => h("tr", null,
        cell(s.exists ? h("a", { href: `#/sites/${enc(s.domain)}/overview` }, s.domain) : h("span", { class: "muted" }, s.domain), null, "primary"),
        cell(s.open ? h("span", { class: "badge ok", title: permsLabel(s.permissions) }, `${s.provider || "claude"} · ${permsLabel(s.permissions)}`) : h("span", { class: "badge" }, t("claudeUse.closed")), t("claudeUse.door")),
        cell(s.open ? h("span", { class: s.today >= s.daily_usd ? "level-error" : "" }, `${usd(s.today)} / ${usd(s.daily_usd)}`) : usd(s.today), t("claudeUse.today"), "num mono"),
        cell(usd(s.d7), t("door.d7"), "num mono"),
        cell(usd(s.d30), t("door.d30"), "num mono"),
        cell(usd(s.month), t("door.month"), "num mono"),
        cell(usd(s.total), t("claudeUse.total"), "num mono"),
        cell(String(s.turns30 || 0), t("claudeUse.turns"), "num mono"),
        cell(bars(s.series || [], d.days), t("claudeUse.series")),
        cell(models(s.models), t("claudeUse.models")),
      ));
      const tt = d.total || {};
      rows.push(h("tr", { class: "total" },
        cell(h("strong", null, t("claudeUse.all")), null, "primary"), cell(""),
        cell(usd(tt.today), null, "num mono"), cell(usd(tt.d7), null, "num mono"), cell(usd(tt.d30), null, "num mono"),
        cell(usd(tt.month), null, "num mono"), cell(usd(tt.total), null, "num mono"), cell(String(tt.turns30 || 0), null, "num mono"),
        cell(bars(tt.series || [], d.days)), cell(models(tt.models))));
      clear(body).append(table([
        { label: t("claudeUse.site") }, { label: t("claudeUse.door") }, { label: t("claudeUse.today"), class: "num" }, { label: t("door.d7"), class: "num" },
        { label: t("door.d30"), class: "num" }, { label: t("door.month"), class: "num" }, { label: t("claudeUse.total"), class: "num" },
        { label: t("claudeUse.turns"), class: "num" }, { label: t("claudeUse.series") }, { label: t("claudeUse.models") },
      ], rows, { responsive: true }).el);
    } catch (e) { clear(body).append(errorBox(e)); }
  }
  (async () => {
    clear(usage).append(h("h2", null, t("llmUse.subscriptions")));
    try {
      const list = await api.get("/agents/usage");
      if (!list.length) usage.append(h("p", { class: "small muted" }, t("usage.noSubscription")));
      for (const st of list) usage.append(h("h3", null, st.name), usageBlock(st));
    } catch (e) { usage.append(h("p", { class: "small muted" }, e.message)); }
  })();
  (async () => {
    try {
      const c = await api.get("/agents/cost?days=30");
      const rows = (c.books || []).map((b) => {
        const door = b.book.endsWith("#door");
        const name = door ? b.book.slice(0, -5) : b.book;
        return h("tr", null,
          cell(h("span", null, name, door ? h("span", { class: "badge" }, t("llmUse.siteDoor")) : null), null, "primary"),
          cell(`$${b.total_usd.toFixed(2)}`, t("llmUse.col.cost"), "num"),
          cell(String(b.turns), t("llmUse.col.turns"), "num"),
          cell(Object.entries(b.models || {}).map(([m, v]) => `${m} $${v.toFixed(2)}`).join(" · "), t("llmUse.col.models"), "small"));
      });
      clear(cost).append(h("div", { class: "card-pad" }, h("h2", null, t("llmUse.cost"))),
        rows.length ? table([{ label: t("llmUse.col.project") }, { label: t("llmUse.col.cost"), class: "num" }, { label: t("llmUse.col.turns"), class: "num" }, { label: t("llmUse.col.models") }], rows, { responsive: true }).el
          : h("p", { class: "card-pad small muted" }, t("llmUse.noCost")));
    } catch (e) { clear(cost).append(h("p", { class: "card-pad small muted" }, e.message)); }
  })();
  range.addEventListener("change", load);
  load();
  return { el: p.el, destroy() {} };
}
