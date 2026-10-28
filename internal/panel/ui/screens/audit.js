// Dependencies: what npm's advisory database says about each site's
// packages. Critical and high are listed, the rest counted; nothing here
// changes a site's dependencies, which belong to its project.
//
// The verdict comes first, in one line (all clean, or how many sites
// have something serious), and a site with nothing found says so in its
// row: an empty cell there read as "not checked".

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
  const runAll = h("button", { type: "button", class: "btn primary" }, icon("refresh"), t("audit.runAll"));
  const p = page({ title: t("nav.audit"), sub: t("audit.sub"), icon: "alert", actions: runAll });
  p.el.classList.add("np-page");
  const verdict = h("div");
  const body = h("div", { class: "card pad0" });
  p.body.append(verdict, body);
  runAll.addEventListener("click", async () => {
    runAll.classList.add("busy");
    try { await api.post("/audit/run"); toast(t("audit.running"), "ok"); } catch (e) { toastError(e); }
    finally { runAll.classList.remove("busy"); }
  });

  async function render() {
    const list = await api.get("/audit");
    clear(verdict);
    if (!list.length) {
      const e = empty(t("audit.none"), t("audit.noneHint"), null, "alert");
      e.classList.add("np-empty");
      clear(body).append(e);
      return null;
    }
    list.sort((a, b) => (b.counts?.critical || 0) - (a.counts?.critical || 0) || (b.counts?.high || 0) - (a.counts?.high || 0) || a.domain.localeCompare(b.domain));
    const rows = list.map((r) => {
      const again = h("button", { type: "button", class: "btn sm ghost" }, icon("refresh"), t("audit.check"));
      again.addEventListener("click", async () => {
        again.classList.add("busy");
        try { await api.post(`/sites/${enc(r.domain)}/audit`); load(body, render); } catch (e) { toastError(e); } finally { again.classList.remove("busy"); }
      });
      const found = LEVELS.filter((l) => r.counts && r.counts[l]);
      const counts = r.skipped || r.error
        ? h("span", { class: r.error ? "level-error small" : "muted small" }, r.error || r.skipped)
        : found.length ? h("div", { class: "actions" }, ...found.map((l) =>
          h("span", { class: "badge " + (l === "critical" ? "bad" : l === "high" ? "warn" : "") }, `${r.counts[l]} ${t("audit.level." + l)}`)))
          : h("span", { class: "status np-clean" }, icon("check", { size: 14 }), t("audit.nothingFound"));
      const details = (r.critical || []).length + (r.high || []).length;
      const show = h("button", { type: "button", class: "btn sm" }, t("audit.details"));
      show.hidden = !details;
      show.addEventListener("click", () => open({
        title: t("audit.detailsOf", { domain: r.domain }), wide: true,
        body: h("ul", { class: "np-advisories" }, ...[...r.critical, ...r.high].map((a) => h("li", null,
          h("span", { class: "badge " + (a.severity === "critical" ? "bad" : "warn") }, t("audit.level." + a.severity)),
          h("div", null, h("code", null, a.name + " " + a.range), a.title ? h("div", null, a.title) : null,
            a.url ? h("a", { href: a.url, target: "_blank", rel: "noopener" }, t("audit.advisory")) : null)))),
      }));
      return h("tr", null,
        cell(h("a", { class: "cell-link", href: `#/sites/${enc(r.domain)}` }, r.domain), null, "primary"),
        cell(counts, t("audit.found")),
        cell(h("span", { title: fmtTime(r.at) }, fmtAgo(r.at, t)), t("audit.at")),
        cell(h("div", { class: "np-row-actions" }, show, again), null, "right nowrap"));
    });
    const serious = list.filter((r) => r.counts && (r.counts.critical || r.counts.high)).length;
    const clean = list.every((r) => !r.error && !r.skipped && !LEVELS.some((l) => r.counts && r.counts[l]));
    if (serious) {
      verdict.append(h("div", { class: "np-verdict bad" }, h("span", { class: "np-attn-mark bad" }, icon("alert", { size: 16 })), h("span", null, t("audit.serious", { n: serious }))));
    } else if (clean) {
      verdict.append(h("div", { class: "np-calm" }, h("span", { class: "np-calm-mark" }, icon("check", { size: 18 })), h("div", null, h("strong", null, t("audit.allClean")))));
    }
    clear(body).append(table([{ label: t("audit.site") }, { label: t("audit.found") }, { label: t("audit.at") }, { label: "" }], rows, { responsive: true }).el);
    return null;
  }
  load(body, render);
  const unsub = onEvent("deps.", () => load(body, render));
  return { el: p.el, destroy() { unsub(); } };
}
