// The frame every screen shares: heading, actions, crumbs, and the three
// states a list can be in (loading, failed, empty) so no screen invents
// its own spinner or its own "nothing here".

import { h, clear } from "../lib/dom.js";
import { t } from "../lib/i18n.js";
import { icon } from "../lib/icons.js";

export function page({ title, sub, actions, crumbs, icon: ico }) {
  const head = h("div", { class: "page-head" });
  if (crumbs) head.append(h("div", { class: "crumbs" }, crumbs));
  head.append(h("h1", null, ico ? icon(ico) : null, h("span", null, title)));
  if (actions) head.append(h("div", { class: "actions" }, actions));
  if (sub) head.append(h("p", { class: "sub" }, sub));
  const body = h("div", { class: "page-body" });
  const el = h("section", { class: "page" }, head, body);
  return { el, head, body };
}

export function loading() {
  return h("div", { class: "loading", role: "status" }, t("common.loading"));
}

export function errorBox(e) {
  return h("div", { class: "error-box", role: "alert" }, e && e.message ? e.message : String(e));
}

export function empty(title, hint, action, ico = "box") {
  return h("div", { class: "empty" }, icon(ico), h("strong", null, title), hint ? h("div", null, hint) : null, action || null);
}

// notice is a boxed line with the icon its kind implies; `kind` is "",
// "warn", "bad" or "ok".
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
export function load(container, fn) {
  const spinner = loading();
  clear(container).append(spinner);
  return Promise.resolve()
    .then(fn)
    .then((node) => {
      // A screen that fills the container itself returns nothing; only the
      // spinner goes then, or the screen's own work would be wiped.
      if (node) clear(container).append(node);
      else spinner.remove();
    })
    .catch((e) => { clear(container).append(errorBox(e)); });
}

export function table(cols, rows, opts = {}) {
  const thead = h("thead", null, h("tr", null, ...cols.map((c) => h("th", { class: c.class || "" }, c.label))));
  const tbody = h("tbody");
  for (const r of rows) tbody.append(r);
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
