// Guard: the rules it cuts with and the incidents it recorded. Rules are
// written in the binary; the panel only shows them. Incidents can be
// dismissed, which is the only write here.
//
// An incident is a short story (where, which rule, what was done), so it
// is a row of sentences rather than seven table columns that became seven
// labelled lines on a phone. The rules are grouped by what the guard does
// when one fires; their level read "info" on every rule before, because
// the badge only knew the event levels and the guard's are intrusion and
// suspicious.

import { icon } from "../lib/icons.js";
import { h, clear, fmtTime, fmtAgo } from "../lib/dom.js";
import { t } from "../lib/i18n.js";
import { api, enc } from "../lib/api.js";
import { onEvent } from "../lib/stream.js";
import { page, load } from "../components/page.js";
import { levelBadge } from "../components/badge.js";
import { toast, toastError } from "../components/toast.js";

const KIND = { intrusion: "bad", suspicious: "warn" };

function guardBadge(level) {
  if (KIND[level]) return h("span", { class: "badge " + KIND[level] }, t("guard.level." + level));
  return levelBadge(level);
}

export function guardScreen() {
  const p = page({ title: t("nav.guard"), sub: t("guard.sub"), icon: "eye" });
  p.el.classList.add("np-page");
  const body = h("div", { class: "np-stack" });
  p.body.append(body);
  const subs = [];

  async function render() {
    const g = await api.get("/guard");
    const rules = g.rules || [];
    const incidents = g.incidents || [];
    const incCard = h("div", { class: "card pad0" },
      h("div", { class: "log-bar card-pad np-card-head" }, h("h2", { class: "grow" }, t("guard.incidents"), incidents.length ? h("span", { class: "np-quiet" }, String(incidents.length)) : null)));
    if (!incidents.length) {
      incCard.append(h("p", { class: "np-none card-pad" }, icon("check", { size: 16 }), h("span", null, h("strong", null, t("guard.noIncidents")), " ", t("guard.noIncidentsHint"))));
    } else {
      incCard.append(h("ul", { class: "np-list" }, ...incidents.map((inc, i) => h("li", { class: "np-inc" },
        h("span", { class: "np-attn-mark " + (KIND[inc.level] || "info") }, icon(inc.level === "intrusion" ? "ban" : "alert", { size: 16 })),
        h("div", { class: "np-inc-main" },
          h("div", { class: "np-inc-title" },
            inc.domain ? h("a", { href: `#/sites/${enc(inc.domain)}` }, inc.domain) : h("strong", null, "—"),
            h("code", null, inc.rule || ""), guardBadge(inc.level)),
          inc.where ? h("div", { class: "np-inc-where mono" }, inc.where) : null,
          inc.action ? h("div", { class: "np-inc-action" }, inc.action) : null),
        h("div", { class: "np-inc-side" },
          h("span", { class: "np-quiet", title: fmtTime(inc.at) }, fmtAgo(inc.at, t)),
          dismissBtn(inc.id ?? String(i)))))));
    }
    const groups = ["intrusion", "suspicious"].map((lv) => [lv, rules.filter((r) => r.level === lv)]);
    const other = rules.filter((r) => !KIND[r.level]);
    const ruleList = (list) => h("ul", { class: "np-rules" }, ...list.map((r) => h("li", null, h("code", null, r.key), h("span", null, r.description || ""))));
    const rulesCard = h("div", { class: "card" }, h("h2", null, t("guard.rules"), h("span", { class: "np-quiet" }, String(rules.length))));
    if (!rules.length) rulesCard.append(h("p", { class: "np-none" }, t("guard.noRules")));
    for (const [lv, list] of groups) {
      if (!list.length) continue;
      rulesCard.append(h("section", { class: "np-rule-group " + KIND[lv] },
        h("h3", null, guardBadge(lv), h("span", null, t("guard.group." + lv))),
        h("p", { class: "np-lead" }, t("guard.groupHint." + lv)),
        ruleList(list)));
    }
    if (other.length) rulesCard.append(h("section", { class: "np-rule-group" }, ruleList(other)));
    clear(body).append(incCard, rulesCard);
    return null;
  }

  function dismissBtn(id) {
    const b = h("button", { type: "button", class: "btn sm" }, icon("check"), t("guard.dismiss"));
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
