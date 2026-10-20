// System: the process itself. Numbers refresh on middleware.* events and
// every 10s while open, because load and memory do not emit events and a
// stale number on this screen is what an operator comes here to check.

import { icon } from "../lib/icons.js";
import { h, clear, append, fmtBytes, fmtDuration } from "../lib/dom.js";
import { t } from "../lib/i18n.js";
import { api, enc } from "../lib/api.js";
import { onEvent } from "../lib/stream.js";
import { page, load, table, cell, empty, kv } from "../components/page.js";
import { confirm } from "../components/dialog.js";
import { toast, toastError } from "../components/toast.js";

export function systemScreen() {
  const restartBtn = h("button", { type: "button", class: "btn danger" }, icon("power"), t("system.restart"));
  const p = page({ title: t("nav.system"), icon: "cpu", actions: restartBtn });
  const stats = h("div", { class: "stats" });
  const info = h("div", { class: "card" });
  const procs = h("div", { class: "card pad0" });
  const portsCard = h("div", { class: "card" });
  p.body.append(stats, info, procs, portsCard);
  const subs = [];
  let timer = null;

  function stat(label, value, mono) {
    return h("div", { class: "stat" }, h("div", { class: "label" }, label), h("div", { class: "value " + (mono ? "mono" : "") }, value));
  }
  function pct(part, total) {
    if (!part || !total) return "";
    return ` (${Math.round((part / total) * 100)}%)`;
  }

  async function render() {
    const s = await api.get("/system");
    const mem = s.memory || {};
    const disk = s.disk || {};
    // load is {"1m","5m","15m"} from the API; older builds sent an array.
    const l = s.load;
    const load_ = Array.isArray(l) ? l.map((x) => Number(x).toFixed(2)).join(" ")
      : l && typeof l === "object" ? ["1m", "5m", "15m"].map((k) => Number(l[k] ?? 0).toFixed(2)).join(" ")
      : (l ?? "");
    // uptime arrives already formatted ("31s", "3h12m"); a number is seconds.
    const uptime = typeof s.uptime === "number" ? fmtDuration(s.uptime) : (s.uptime || (s.started ? fmtDuration((Date.now() - new Date(s.started)) / 1000) : ""));
    const memUsed = mem.used ?? (mem.total && mem.free ? mem.total - mem.free : undefined);
    const diskUsed = disk.used ?? (disk.total && disk.free ? disk.total - disk.free : undefined);
    clear(stats).append(
      stat(t("system.version"), s.version || "", true),
      stat(t("system.uptime"), uptime),
      stat(t("system.load"), load_, true),
      stat(t("system.memory"), memUsed !== undefined ? fmtBytes(memUsed) + " / " + fmtBytes(mem.total) + pct(memUsed, mem.total) : fmtBytes(mem.total)),
      stat(t("system.disk"), diskUsed !== undefined ? fmtBytes(diskUsed) + " / " + fmtBytes(disk.total) + pct(diskUsed, disk.total) : fmtBytes(disk.total)),
    );
    clear(info).append(h("h2", null, t("system.process")), kv([
      [t("system.root"), h("code", null, s.root || "")],
      [t("system.listen"), h("code", null, s.listen || "")],
      [t("settings.panelDomain"), s.panel_domain || h("span", { class: "muted" }, t("system.loopbackOnly"))],
      [t("system.go"), s.go || ""],
    ]));
    return null;
  }

  async function renderProcs() {
    const list = await api.get("/system/processes");
    const rows = (list || []).map((pr) => h("tr", null,
      cell(String(pr.pid), t("system.col.pid"), "mono num"),
      cell(pr.domain ? h("a", { href: `#/sites/${enc(pr.domain)}` }, pr.domain) : "—", null, "primary"),
      cell(pr.kind || "", t("system.col.kind")),
      cell(fmtBytes(pr.rss), t("system.col.rss"), "num mono"),
      cell(pr.cpu !== undefined ? Number(pr.cpu).toFixed(1) + "%" : "", t("system.col.cpu"), "num mono"),
      cell(pr.started ? fmtDuration((Date.now() - new Date(pr.started)) / 1000) : "", t("system.col.age"), "num"),
    ));
    clear(procs).append(
      h("div", { class: "log-bar card-pad" }, h("h2", null, t("system.processes")), h("span", { class: "muted small" }, t("system.processCount", { n: rows.length }))),
      rows.length ? table([{ label: t("system.col.pid"), class: "num" }, { label: t("system.col.domain") }, { label: t("system.col.kind") }, { label: t("system.col.rss"), class: "num" }, { label: t("system.col.cpu"), class: "num" }, { label: t("system.col.age"), class: "num" }], rows, { responsive: true }).el
        : empty(t("system.noProcesses"), t("system.noProcessesHint")),
    );
    return null;
  }

  async function renderPorts() {
    const pt = await api.get("/system/ports");
    const used = Object.entries(pt.in_use || {}).sort((a, b) => Number(a[0]) - Number(b[0]));
    append(clear(portsCard), [
      h("h2", null, t("system.ports")),
      h("p", { class: "small muted" }, t("system.portsRange", { from: pt.from, to: pt.to, n: used.length })),
      used.length ? h("div", { class: "inline-list" }, ...used.map(([port, domain]) => h("span", { class: "badge" }, h("code", null, port), " ", domain))) : null,
    ]);
    return null;
  }

  restartBtn.addEventListener("click", async () => {
    if (!(await confirm({ title: t("system.restartQ"), body: t("system.restartBody"), confirmLabel: t("system.restart"), danger: true }))) return;
    try { await api.post("/system/restart"); toast(t("system.restarting"), "ok"); } catch (e) { toastError(e); }
  });

  const all = () => { load(info, render); load(procs, renderProcs); load(portsCard, renderPorts); };
  all();
  timer = setInterval(() => { render().catch(() => {}); renderProcs().catch(() => {}); }, 10000);
  subs.push(onEvent("middleware.", all));
  subs.push(onEvent("instance.", () => { renderProcs().catch(() => {}); renderPorts().catch(() => {}); }));
  return { el: p.el, destroy() { clearInterval(timer); subs.forEach((u) => u()); } };
}
