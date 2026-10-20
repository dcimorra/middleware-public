// Events: the audit trail, live. The first page comes from GET /events,
// the rest from the stream; both land in the store so a row is a row
// whichever way it arrived. Filters are in memory and the type prefix is
// also sent to the API so the first page is the right slice.

import { icon } from "../lib/icons.js";
import { h, clear, fmtClock, fmtTime } from "../lib/dom.js";
import { t } from "../lib/i18n.js";
import { api, qs } from "../lib/api.js";
import { store } from "../lib/store.js";
import { page, table, cell, empty, errorBox } from "../components/page.js";
import { levelBadge } from "../components/badge.js";

export function eventsScreen({ query }) {
  const typeBox = h("input", { type: "search", placeholder: t("events.typeFilter"), "aria-label": t("events.typeFilter"), value: query.get("type") || "", autocomplete: "off" });
  const subjectBox = h("input", { type: "text", placeholder: t("events.subjectFilter"), "aria-label": t("events.subjectFilter"), value: query.get("subject") || "", autocomplete: "off", list: "subjects" });
  const level = h("select", { "aria-label": t("events.level") }, h("option", { value: "" }, t("events.allLevels")), h("option", { value: "warn" }, t("level.warn")), h("option", { value: "error" }, t("level.error")));
  const datalist = h("datalist", { id: "subjects" });
  const pause = h("button", { type: "button", class: "btn sm", "aria-pressed": "false" }, icon("pause"), t("events.pause"));
  const p = page({ title: t("nav.events"), sub: t("events.sub"), icon: "activity" });
  const listBox = h("div", { class: "card pad0" });
  const drawer = h("aside", { class: "card drawer", hidden: true });
  const wrap = h("div", { class: "events" }, listBox, drawer);
  p.body.append(h("div", { class: "filters" }, h("div", { class: "search" }, typeBox), subjectBox, datalist, level, pause), wrap);
  const subs = [];
  let paused = false;
  let selected = null;

  function matches(ev) {
    const tp = typeBox.value.trim();
    const sj = subjectBox.value.trim();
    if (tp && !ev.type.startsWith(tp)) return false;
    if (sj && ev.subject !== sj) return false;
    if (level.value && (ev.level || "") !== level.value) return false;
    return true;
  }

  function render() {
    if (paused) return;
    const all = store.get("events");
    const list = all.filter(matches).slice(-500).reverse();
    clear(listBox);
    if (list.length === 0) { listBox.append(empty(t("events.emptyTitle"), t("events.emptyHint"))); return; }
    const rows = list.map((ev) => {
      const tr = h("tr", { class: "event-row row-link level-" + (ev.level || "info"), tabindex: "0" },
        cell(h("span", { title: fmtTime(ev.time) }, fmtClock(ev.time)), null, "nowrap mono"),
        cell(ev.type, t("events.col.type"), "type"),
        cell(ev.subject ? h("a", { href: `#/sites/${encodeURIComponent(ev.subject)}` }, ev.subject) : "", t("events.col.subject")),
        cell(ev.actor || "", t("events.col.actor"), "dim"),
        cell(h("span", { class: "truncate" }, ev.message || ""), null, "primary"),
      );
      const show = () => { selected = ev; showDrawer(ev); };
      tr.addEventListener("click", (e) => { if (!e.target.closest("a")) show(); });
      tr.addEventListener("keydown", (e) => { if (e.key === "Enter" || e.key === " ") { e.preventDefault(); show(); } });
      return tr;
    });
    listBox.append(table([{ label: t("events.col.time") }, { label: t("events.col.type") }, { label: t("events.col.subject") }, { label: t("events.col.actor") }, { label: t("events.col.message") }], rows, { responsive: true }).el);
    const subjects = new Set(all.map((e) => e.subject).filter(Boolean));
    clear(datalist).append(...[...subjects].sort().map((s) => h("option", { value: s })));
  }

  function showDrawer(ev) {
    clear(drawer);
    drawer.hidden = false;
    wrap.classList.add("with-drawer");
    const close = h("button", { type: "button", class: "btn sm ghost" }, t("common.close"));
    close.addEventListener("click", () => { drawer.hidden = true; wrap.classList.remove("with-drawer"); selected = null; });
    drawer.append(
      h("div", { class: "log-bar" }, h("h2", { class: "grow" }, ev.type), levelBadge(ev.level), close),
      h("dl", { class: "kv" },
        h("dt", null, t("events.col.time")), h("dd", null, fmtTime(ev.time)),
        h("dt", null, t("events.col.subject")), h("dd", null, ev.subject || "—"),
        h("dt", null, t("events.col.actor")), h("dd", null, ev.actor || "—"),
        h("dt", null, t("events.col.message")), h("dd", null, ev.message || "—"),
      ),
      h("h3", null, t("events.data")),
      h("pre", { class: "log" }, JSON.stringify(ev.data || {}, null, 2)),
    );
  }

  for (const el of [typeBox, subjectBox, level]) el.addEventListener("input", render);
  pause.addEventListener("click", () => {
    paused = !paused;
    pause.setAttribute("aria-pressed", String(paused));
    pause.textContent = paused ? t("events.resume") : t("events.pause");
    if (!paused) render();
  });

  subs.push(store.subscribe("events", render));
  (async () => {
    try {
      const list = await api.get("/events" + qs({ limit: 200, type: typeBox.value.trim() || undefined }));
      // Merge with what the stream already delivered rather than replace
      // it: an event that arrived between the page load and this answer
      // would otherwise vanish for a moment and come back.
      const have = new Set(store.get("events").map((e) => e.time + e.type + e.subject));
      const merged = [...(list || []).filter((e) => !have.has(e.time + e.type + e.subject)), ...store.get("events")]
        .sort((a, b) => new Date(a.time) - new Date(b.time));
      store.seedEvents(merged);
    } catch (e) {
      clear(listBox).append(errorBox(e));
    }
  })();
  render();
  return { el: p.el, destroy() { subs.forEach((u) => u()); } };
}
