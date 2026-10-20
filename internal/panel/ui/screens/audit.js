// Dependencies: what npm's advisory database says about each site's
// packages. Critical and high are listed, the rest counted; nothing here
// changes a site's dependencies, which belong to its project.

import { h, clear, fmtTime, fmtAgo } from "../lib/dom.js";
import { t } from "../lib/i18n.js";
import { api, enc } from "../lib/api.js";
import { onEvent } from "../lib/stream.js";
import { page, load, table, cell, empty } from "../components/page.js";
import { open } from "../components/dialog.js";
import { toast, toastError } from "../components/toast.js";
import { icon } from "../lib/icons.js";

const LEVELS = ["critical", "high", "moderate", "low"];

export function auditScreen() {
  const runAll = h("button", { type: "button", class: "btn" }, icon("refresh"), t("audit.runAll"));
  const p = page({ title: t("nav.audit"), sub: t("audit.sub"), icon: "alert", actions: runAll });
  const body = h("div", { class: "card pad0" });
  p.body.append(body);
  runAll.addEventListener("click", async () => {
    try { await api.post("/audit/run"); toast(t("audit.running"), "ok"); } catch (e) { toastError(e); }
  });

  async function render() {
    const list = await api.get("/audit");
    if (!list.length) { clear(body).append(empty(t("audit.none"), t("audit.noneHint"), null, "alert")); return null; }
    list.sort((a, b) => (b.counts?.critical || 0) - (a.counts?.critical || 0) || (b.counts?.high || 0) - (a.counts?.high || 0) || a.domain.localeCompare(b.domain));
    const rows = list.map((r) => {
      const again = h("button", { type: "button", class: "btn sm" }, t("audit.check"));
      again.addEventListener("click", async () => {
        again.classList.add("busy");
        try { await api.post(`/sites/${enc(r.domain)}/audit`); load(body, render); } catch (e) { toastError(e); } finally { again.classList.remove("busy"); }
      });
      const counts = r.skipped || r.error
        ? h("span", { class: r.error ? "level-error small" : "muted small" }, r.error || r.skipped)
        : h("div", { class: "actions" }, ...LEVELS.filter((l) => r.counts && r.counts[l]).map((l) =>
          h("span", { class: "badge " + (l === "critical" ? "bad" : l === "high" ? "warn" : "") }, `${r.counts[l]} ${t("audit.level." + l)}`)));
      const details = (r.critical || []).length + (r.high || []).length;
      const show = h("button", { type: "button", class: "btn sm ghost" }, t("audit.details"));
      show.hidden = !details;
      show.addEventListener("click", () => open({
        title: t("audit.detailsOf", { domain: r.domain }), wide: true,
        body: h("div", { class: "stack-sm" }, ...[...r.critical, ...r.high].map((a) => h("div", null,
          h("span", { class: "badge " + (a.severity === "critical" ? "bad" : "warn") }, t("audit.level." + a.severity)), " ",
          h("code", null, a.name + " " + a.range), " ", a.title || "", " ",
          a.url ? h("a", { href: a.url, target: "_blank", rel: "noopener" }, t("audit.advisory")) : null))),
      }));
      return h("tr", null,
        cell(h("a", { href: `#/sites/${enc(r.domain)}` }, r.domain), null, "primary"),
        cell(counts, t("audit.found")),
        cell(h("span", { title: fmtTime(r.at) }, fmtAgo(r.at, t)), t("audit.at")),
        cell(h("div", { class: "actions" }, show, again), null, "right nowrap"));
    });
    const clean = list.every((r) => !r.error && !r.skipped && !LEVELS.some((l) => r.counts && r.counts[l]));
    clear(body).append(table([{ label: t("audit.site") }, { label: t("audit.found") }, { label: t("audit.at") }, { label: "" }], rows, { responsive: true }).el);
    if (clean) body.append(h("p", { class: "small muted card-pad" }, t("audit.allClean")));
    return null;
  }
  load(body, render);
  const unsub = onEvent("deps.", () => load(body, render));
  return { el: p.el, destroy() { unsub(); } };
}
