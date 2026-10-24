// The moves of sites from this node to others, as their operations: each
// step with its state, how long it took and, when it failed, why; and the
// way back when there was one. Read from GET /moves, which keeps them on
// disk, and redrawn on every relocate.* event, so a move goes on in the
// background and is read here whenever, also once the site has left this
// node and its own page is gone.

import { h, clear, fmtAgo, fmtDuration, fmtTime, debounce } from "../lib/dom.js";
import { t } from "../lib/i18n.js";
import { api, enc } from "../lib/api.js";
import { onEvent } from "../lib/stream.js";

const BADGE = { running: "info", ok: "ok", failed: "bad", warn: "warn" };

const secs = (a, b) => (new Date(b || Date.now()) - new Date(a)) / 1000;

function jobState(j) {
  if (!j.done) return "running";
  if (j.error) return "failed";
  return (j.steps || []).some((s) => s.state === "warn") ? "warn" : "ok";
}

function stepRow(j, s) {
  return h("li", { class: "move-step " + s.state },
    h("span", { class: "badge " + (BADGE[s.state] || "") }, t("moves.state." + s.state)),
    h("div", { class: "grow" },
      h("div", null, t("site.moveStep." + s.step, { node: j.node })),
      s.message ? h("div", { class: "small dim" }, s.message) : null,
      s.error ? h("div", { class: "small level-error" }, s.error) : null),
    h("span", { class: "small muted mono", title: fmtTime(s.started) }, fmtDuration(secs(s.started, s.ended))));
}

function jobEl(j, open) {
  const state = jobState(j);
  const head = h("summary", { class: "row" },
    h("span", { class: "badge " + BADGE[state] }, t("moves.state." + state)),
    h("strong", null, t("moves.title1", { domain: j.domain, node: j.node })),
    h("span", { class: "grow" }),
    h("span", { class: "small muted", title: fmtTime(j.started) }, fmtAgo(j.started, t) + " · " + fmtDuration(secs(j.started, j.ended))));
  const body = h("div", { class: "stack-sm" },
    h("ol", { class: "move-steps" }, ...(j.steps || []).map((s) => stepRow(j, s))),
    j.error ? h("div", { class: "notice bad small" }, j.error) : null,
    j.report && j.report.down && state !== "failed" ? h("p", { class: "small muted" }, t("moves.down", { down: j.report.down })) : null);
  return h("details", { class: "move-job", open: open || state === "running" }, head, body);
}

// movesCard is the card for every move (domain empty) or one site's. It
// hides itself while there is none.
export function movesCard(domain) {
  const list = h("div", { class: "stack-sm" });
  const el = h("div", { class: "card", hidden: true }, h("h2", null, t(domain ? "moves.siteTitle" : "moves.title")), h("p", { class: "small muted" }, t("moves.hint")), list);
  let seen = false;
  async function load() {
    let jobs = [];
    try { jobs = await api.get("/moves" + (domain ? "?domain=" + enc(domain) : "")); } catch { jobs = []; }
    if (!domain) jobs = jobs.slice(0, 10);
    el.hidden = !jobs.length;
    // The newest open the first time; after that, what the operator opened stays.
    const opened = new Set([...list.querySelectorAll("details[open]")].map((d) => d.dataset.id));
    clear(list).append(...jobs.map((j, i) => {
      const d = jobEl(j, seen ? opened.has(j.id) : i === 0);
      d.dataset.id = j.id;
      return d;
    }));
    seen = true;
  }
  const reload = debounce(load, 300);
  const off = onEvent("relocate.", (ev) => { if (!domain || ev.subject === domain) reload(); });
  // A running step's clock moves without events.
  const timer = setInterval(() => { if (list.querySelector(".badge.info")) load(); }, 5000);
  load();
  return { el, reload: load, destroy() { off(); clearInterval(timer); } };
}
