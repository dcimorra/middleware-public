// The frame every screen shares: heading, actions, crumbs, and the three
// states a list can be in (loading, failed, empty) so no screen invents
// its own spinner or its own "nothing here".

import { h, clear } from "../lib/dom.js";
import { t } from "../lib/i18n.js";
import { icon } from "../lib/icons.js";

export function page({ title, sub, actions, crumbs, icon: ico }) {
  const head = h("div", { class: "page-head" });
  if (crumbs) head.append(h("div", { class: "crumbs" }, crumbs));
  // The title and the line under it go together, the actions beside them:
  // with the line last in a wrapping row it fell under the buttons on a
  // phone, and read as the caption of the buttons rather than the page.
  head.append(h("div", { class: "page-title" },
    h("h1", null, ico ? icon(ico) : null, h("span", null, title)),
    sub ? h("p", { class: "sub" }, sub) : null));
  if (actions) head.append(h("div", { class: "actions" }, actions));
  const body = h("div", { class: "page-body" });
  const el = h("section", { class: "page" }, head, body);
  return { el, head, body };
}

// loading is a few soft bars in the shape of what is coming, shown only
// after a moment: most answers arrive before it would appear, and a word
// flashing for a tenth of a second read as the screen flickering. The
// words stay for screen readers.
export function loading() {
  return h("div", { class: "loading", role: "status", "aria-live": "polite" },
    h("span", { class: "sr-only" }, t("common.loading")),
    h("span", { class: "skel", "aria-hidden": "true" }),
    h("span", { class: "skel", "aria-hidden": "true" }),
    h("span", { class: "skel short", "aria-hidden": "true" }));
}

// errorBox says what failed and, when the caller can, offers to try again:
// a list that did not load was otherwise a dead end until a full reload.
export function errorBox(e, retry) {
  const msg = e && e.message ? e.message : String(e);
  const box = h("div", { class: "error-box", role: "alert" }, icon("alert", { size: 18 }), h("span", { class: "grow" }, msg));
  if (retry) {
    box.append(h("button", { type: "button", class: "btn sm", onClick: retry }, icon("refresh"), t("common.retry")));
  }
  return box;
}

// empty is a quiet invitation rather than a blank: an icon in a soft
// circle, what is (not) here in one line, the hint, and the next action.
export function empty(title, hint, action, ico = "box") {
  return h("div", { class: "empty" },
    h("span", { class: "empty-ico", "aria-hidden": "true" }, icon(ico)),
    h("strong", null, title),
    hint ? h("div", { class: "empty-hint" }, hint) : null,
    action ? h("div", { class: "empty-action" }, action) : null);
}

export function notice(kind, ...content) {
  const ico = kind === "warn" ? "alert" : kind === "bad" ? "ban" : kind === "ok" ? "check" : "info";
  return h("div", { class: "notice " + (kind || "") }, icon(ico, { size: 18 }), h("div", { class: "grow" }, ...content));
}

// search wraps an input with the magnifier.
export function search(input) {
  return h("div", { class: "search" }, icon("search"), input);
}

// load runs fn and swaps the container between loading, error and the
// result; returning a function from fn makes it the re-render, so a
// screen can call reload() when the stream says something changed.
//
// The container keeps the height it had while the new content comes: a
// reload on a stream event emptied it for a moment, the page under it
// jumped up and the scroll was lost with it.
export function load(container, fn) {
  const spinner = loading();
  const before = container.offsetHeight;
  if (before) container.style.minHeight = before + "px";
  clear(container).append(spinner);
  return Promise.resolve()
    .then(fn)
    .then((node) => {
      // A screen that fills the container itself returns nothing; only the
      // spinner goes then, or the screen's own work would be wiped.
      if (node) clear(container).append(node);
      else spinner.remove();
    })
    .catch((e) => { clear(container).append(errorBox(e, () => load(container, fn))); })
    .finally(() => { container.style.minHeight = ""; });
}

export function table(cols, rows, opts = {}) {
  const thead = h("thead", null, h("tr", null, ...cols.map((c) => h("th", { class: c.class || "" }, c.label))));
  const tbody = h("tbody");
  for (const r of rows) {
    // A cell that says only "—" is a placeholder: a column needs it to
    // stay a column, a phone card does not, where its label made a line
    // of nothing ("Certificate —").
    for (const td of r.children || []) if (td.dataset && td.dataset.label && td.textContent.trim() === "—") td.classList.add("blank");
    tbody.append(r);
  }
  const tbl = h("table", { class: "table " + (opts.responsive ? "responsive" : "") }, thead, tbody);
  return { el: h("div", { class: "table-wrap" }, tbl), tbody };
}

export function cell(content, label, cls) {
  const td = h("td", { class: cls || "" }, content);
  if (label) td.dataset.label = label;
  return td;
}

export function kv(pairs) {
  const dl = h("dl", { class: "kv" });
  for (const [k, v] of pairs) {
    if (v === undefined || v === null || v === "") continue;
    dl.append(h("dt", null, k), h("dd", null, v));
  }
  return dl;
}
