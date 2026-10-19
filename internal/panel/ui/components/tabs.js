// Tabs as links: each tab is a hash route of its own, so a tab survives a
// reload and can be sent to someone. aria-current marks the live one.

import { h } from "../lib/dom.js";

export function tabs(items, active) {
  const nav = h("nav", { class: "tabs", role: "tablist" });
  for (const it of items) {
    nav.append(h("a", { href: it.href, role: "tab", "aria-selected": it.key === active ? "true" : "false", "aria-current": it.key === active ? "page" : null }, it.label));
  }
  return nav;
}
