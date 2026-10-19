// Guard: the rules it cuts with and the incidents it recorded. Rules are
// written in the binary; the panel only shows them. Incidents can be
// dismissed, which is the only write here.

import { h, clear, fmtTime, fmtAgo } from "../lib/dom.js";
import { t } from "../lib/i18n.js";
import { api, enc } from "../lib/api.js";
import { onEvent } from "../lib/stream.js";
import { page, load, table, cell, empty } from "../components/page.js";
import { levelBadge } from "../components/badge.js";
import { toast, toastError } from "../components/toast.js";

export function guardScreen() {
  const p = page({ title: t("nav.guard"), sub: t("guard.sub") });
  const body = h("div");
  p.body.append(body);
  const subs = [];

  async function render() {
    const g = await api.get("/guard");
    const rules = g.rules || [];
    const incidents = g.incidents || [];
    const ruleRows = rules.map((r) => h("tr", null,
      cell(h("code", null, r.key), null, "primary"),
      cell(levelBadge(r.level), t("guard.col.level")),
      cell(r.description || "", t("guard.col.description")),
    ));
    const incRows = incidents.map((inc, i) => h("tr", { class: "level-" + (inc.level || "info") },
      cell(h("span", { title: fmtTime(inc.at) }, fmtAgo(inc.at, t)), t("guard.col.when"), "nowrap"),
      cell(inc.domain ? h("a", { href: `#/sites/${enc(inc.domain)}` }, inc.domain) : "—", null, "primary"),
      cell(h("code", null, inc.rule || ""), t("guard.col.rule")),
      cell(levelBadge(inc.level), t("guard.col.level")),
      cell(inc.where || "", t("guard.col.where"), "mono"),
      cell(inc.action || "", t("guard.col.action")),
      cell(dismissBtn(inc.id ?? String(i)), null, "right"),
    ));
    clear(body).append(
      h("div", { class: "card pad0" },
        h("div", { class: "log-bar card-pad" }, h("h2", null, t("guard.incidents"))),
        incRows.length ? table([{ label: t("guard.col.when") }, { label: t("guard.col.domain") }, { label: t("guard.col.rule") }, { label: t("guard.col.level") }, { label: t("guard.col.where") }, { label: t("guard.col.action") }, { label: "" }], incRows, { responsive: true }).el
          : empty(t("guard.noIncidents"), t("guard.noIncidentsHint")),
      ),
      h("div", { class: "card pad0" },
        h("div", { class: "log-bar card-pad" }, h("h2", null, t("guard.rules"))),
        ruleRows.length ? table([{ label: t("guard.col.rule") }, { label: t("guard.col.level") }, { label: t("guard.col.description") }], ruleRows, { responsive: true }).el
          : empty(t("guard.noRules"), ""),
      ),
    );
    return null;
  }

  function dismissBtn(id) {
    const b = h("button", { type: "button", class: "btn sm" }, t("guard.dismiss"));
    b.addEventListener("click", async () => {
      b.classList.add("busy");
      try { await api.post(`/guard/incidents/${enc(id)}/dismiss`); toast(t("guard.dismissed"), "ok"); load(body, render); }
      catch (e) { toastError(e); } finally { b.classList.remove("busy"); }
    });
    return b;
  }

  load(body, render);
  subs.push(onEvent("guard.", () => load(body, render)));
  return { el: p.el, destroy() { subs.forEach((u) => u()); } };
}
