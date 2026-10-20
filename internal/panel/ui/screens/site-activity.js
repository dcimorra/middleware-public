// Activity for one site: its traffic over the last minute, live, and the
// guard's incidents about it. Traffic is polled every two seconds while
// the tab is open and only then: it is a minute of counters in memory,
// cheap to ask for and pointless to stream to a closed tab.

import { h, clear, fmtTime, fmtAgo, fmtBytes } from "../lib/dom.js";
import { t } from "../lib/i18n.js";
import { api, enc } from "../lib/api.js";
import { onEvent } from "../lib/stream.js";
import { kv, table, cell, empty, errorBox } from "../components/page.js";
import { levelBadge } from "../components/badge.js";
import { sparkline } from "../components/sparkline.js";
import { toast, toastError } from "../components/toast.js";
import { icon } from "../lib/icons.js";

export function activityTab(domain) {
  const spark = sparkline({ label: t("activity.sparkLabel") });
  const numbers = h("div");
  const trafficErr = h("div");
  const incBox = h("div");
  const cpu = sparkline({ label: t("activity.cpu") });
  const mem = sparkline({ label: t("activity.memory") });
  const resBox = h("div");
  const el = h("div", { class: "cards" },
    h("div", { class: "card" }, h("h2", null, t("activity.traffic")), h("p", { class: "small muted" }, t("activity.trafficHint")), spark.el, numbers, trafficErr),
    h("div", { class: "card" }, h("h2", null, t("activity.resources")), h("p", { class: "small muted" }, t("activity.resourcesHint")),
      h("h3", null, t("activity.cpu")), cpu.el, h("h3", null, t("activity.memory")), mem.el, resBox),
    h("div", { class: "card pad0" }, h("div", { class: "log-bar card-pad" }, h("h2", null, t("activity.incidents"))), incBox),
  );

  async function poll() {
    try {
      const snap = await api.get("/waf/traffic");
      const d = (snap.by_domain || {})[domain];
      clear(trafficErr);
      if (!d) {
        spark.set(new Array(60).fill(0));
        clear(numbers).append(h("p", { class: "small muted" }, t("activity.quiet")));
        return;
      }
      spark.set(d.seconds || []);
      const statuses = Object.entries(d.by_status || {}).sort().map(([k, v]) => `${k}: ${v}`).join(" · ");
      clear(numbers).append(kv([
        [t("activity.requests"), String(d.requests)],
        [t("activity.perSecond"), d.per_second.toFixed(2)],
        [t("activity.bytes"), fmtBytes(d.bytes)],
        [t("activity.latency"), t("activity.latencyValue", { p50: d.p50_ms, p95: d.p95_ms })],
        [t("activity.statuses"), statuses],
      ]));
    } catch (e) {
      clear(trafficErr).append(errorBox(e));
    }
  }

  // A minute a sample: loaded once and on the minute, not every poll.
  async function loadResources() {
    try {
      const r = await api.get(`/sites/${enc(domain)}/resources`);
      const samples = r.samples || [];
      cpu.set(samples.map((x) => Math.round(x.cpu)));
      mem.set(samples.map((x) => Math.round(x.rss / 1048576)));
      const last = samples[samples.length - 1];
      clear(resBox).append(
        last ? h("p", { class: "small" }, t("activity.resourcesNow", { cpu: last.cpu, mem: fmtBytes(last.rss) })) : h("p", { class: "small muted" }, t("activity.resourcesNone")),
        ...(r.episodes || []).slice(-5).reverse().map((ep) => h("div", { class: "small " + (ep.end ? "" : "level-warn") },
          t(ep.end ? "activity.episode" : "activity.episodeOpen", {
            kind: t("activity.kind." + ep.kind), at: fmtTime(ep.start),
            peak: ep.kind === "cpu" ? Math.round(ep.peak) + "%" : fmtBytes(ep.peak),
            minutes: ep.end ? Math.round((new Date(ep.end) - new Date(ep.start)) / 60000) : 0,
          }))));
    } catch (e) {
      clear(resBox).append(h("p", { class: "small muted" }, e.message));
    }
  }

  async function loadIncidents() {
    try {
      const g = await api.get("/guard");
      const mine = (g.incidents || []).filter((i) => i.domain === domain);
      if (!mine.length) { clear(incBox).append(empty(t("activity.noIncidents"), t("activity.noIncidentsHint"), null, "eye")); return; }
      const rows = mine.map((inc) => h("tr", { class: "level-" + (inc.level || "info") },
        cell(h("span", { title: fmtTime(inc.at) }, fmtAgo(inc.at, t)), t("guard.col.when"), "nowrap"),
        cell(h("code", null, inc.rule || ""), t("guard.col.rule")),
        cell(levelBadge(inc.level), t("guard.col.level")),
        cell(inc.where || "", t("guard.col.where"), "mono"),
        cell(inc.action || "", t("guard.col.action")),
        cell(dismissBtn(inc.id), null, "right"),
      ));
      clear(incBox).append(table([{ label: t("guard.col.when") }, { label: t("guard.col.rule") }, { label: t("guard.col.level") }, { label: t("guard.col.where") }, { label: t("guard.col.action") }, { label: "" }], rows, { responsive: true }).el);
    } catch (e) {
      clear(incBox).append(errorBox(e));
    }
  }

  function dismissBtn(id) {
    const b = h("button", { type: "button", class: "btn sm" }, icon("check"), t("guard.dismiss"));
    b.addEventListener("click", async () => {
      b.classList.add("busy");
      try { await api.post(`/guard/incidents/${enc(id)}/dismiss`); toast(t("guard.dismissed"), "ok"); loadIncidents(); }
      catch (e) { toastError(e); } finally { b.classList.remove("busy"); }
    });
    return b;
  }

  poll();
  loadIncidents();
  loadResources();
  const timer = setInterval(() => { if (!document.hidden) poll(); }, 2000);
  const resTimer = setInterval(() => { if (!document.hidden) loadResources(); }, 60000);
  const unsub = onEvent("guard.", () => loadIncidents());
  return { el, destroy() { clearInterval(timer); clearInterval(resTimer); unsub(); } };
}
