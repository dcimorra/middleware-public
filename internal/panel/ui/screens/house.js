// The house's own conversation, with what the agents cost around it: the
// Claude account's windows (as /usage shows them) and what each project
// spent in the last thirty days, the site door's book apart.

import { h, clear } from "../lib/dom.js";
import { t } from "../lib/i18n.js";
import { api } from "../lib/api.js";
import { page, table, cell } from "../components/page.js";
import { chatTab } from "./site-chat.js";

export function houseScreen() {
  const p = page({ title: t("nav.house"), sub: t("house.sub"), icon: "sparkles" });
  const usage = h("div", { class: "card" });
  const cost = h("div", { class: "card pad0" });
  const chat = chatTab("middleware");
  p.body.append(h("div", { class: "grid-2" }, usage, cost), chat.el);

  function bar(label, w) {
    if (!w) return null;
    const pct = Math.max(0, Math.min(100, w.pct));
    // The width through CSSOM: the panel's CSP refuses style attributes.
    const fill = h("div", { class: "fill" + (pct >= 90 ? " bad" : pct >= 70 ? " warn" : "") });
    fill.style.width = `${pct}%`;
    return h("div", { class: "usage-row" },
      h("div", { class: "row" }, h("span", { class: "grow" }, label), h("strong", null, `${pct.toFixed(0)}%`)),
      h("div", { class: "meter", role: "meter", "aria-valuenow": String(pct), "aria-valuemin": "0", "aria-valuemax": "100", "aria-label": label }, fill),
      w.reset ? h("div", { class: "small muted" }, t("house.resets", { when: new Date(w.reset).toLocaleString() })) : null);
  }
  (async () => {
    clear(usage).append(h("h2", null, t("house.account")));
    try {
      const u = await api.get("/agents/usage");
      usage.append(bar(t("house.session"), u.session) || "", bar(t("house.week"), u.week) || "", bar(t("house.opus"), u.opus) || "");
    } catch (e) { usage.append(h("p", { class: "small muted" }, e.message)); }
  })();
  (async () => {
    try {
      const c = await api.get("/agents/cost?days=30");
      const rows = (c.books || []).map((b) => {
        const door = b.book.endsWith("#door");
        const name = door ? b.book.slice(0, -5) : b.book;
        return h("tr", null,
          cell(h("span", null, name, door ? h("span", { class: "badge" }, t("house.siteDoor")) : null), null, "primary"),
          cell(`$${b.total_usd.toFixed(2)}`, t("house.col.cost"), "num"),
          cell(String(b.turns), t("house.col.turns"), "num"),
          cell(Object.entries(b.models || {}).map(([m, v]) => `${m} $${v.toFixed(2)}`).join(" · "), t("house.col.models"), "small"));
      });
      clear(cost).append(h("div", { class: "card-pad" }, h("h2", null, t("house.cost"))),
        rows.length ? table([{ label: t("house.col.project") }, { label: t("house.col.cost"), class: "num" }, { label: t("house.col.turns"), class: "num" }, { label: t("house.col.models") }], rows, { responsive: true }).el
          : h("p", { class: "card-pad small muted" }, t("house.noCost")));
    } catch (e) { clear(cost).append(h("p", { class: "card-pad small muted" }, e.message)); }
  })();
  return { el: p.el, destroy() { chat.destroy(); } };
}
