// Tabs as links: each tab is a hash route of its own, so a tab survives a
// reload and can be sent to someone. aria-current marks the live one.
//
// On a phone the row is wider than the screen and scrolls sideways, which
// nothing said: the last tabs were simply not there for whoever did not
// think to swipe. The row now fades out on the side that has more, and
// opens scrolled to the live tab, which on a site's tenth tab was
// otherwise off screen on arrival.

import { h } from "../lib/dom.js";
import { icon } from "../lib/icons.js";

export function tabs(items, active) {
  const nav = h("nav", { class: "tabs", role: "tablist" });
  for (const it of items) {
    nav.append(h("a", { href: it.href, role: "tab", "aria-selected": it.key === active ? "true" : "false", "aria-current": it.key === active ? "page" : null }, it.icon ? icon(it.icon) : null, it.label));
  }
  const edges = () => {
    const max = nav.scrollWidth - nav.clientWidth;
    nav.classList.toggle("more-left", nav.scrollLeft > 2);
    nav.classList.toggle("more-right", max - nav.scrollLeft > 2);
  };
  nav.addEventListener("scroll", edges, { passive: true });
  // The first observation is the row's first layout, the moment its width
  // is known; a screen appends it some time after this returns.
  let first = true;
  if (typeof ResizeObserver === "function") {
    new ResizeObserver(() => {
      if (first) {
        first = false;
        const on = nav.querySelector("[aria-current=page]");
        // Only when the live tab is out of sight: scrolling a visible one
        // to the middle hid the first tabs for nothing.
        if (on && (on.offsetLeft < nav.scrollLeft || on.offsetLeft + on.offsetWidth > nav.scrollLeft + nav.clientWidth)) {
          nav.scrollLeft = on.offsetLeft - (nav.clientWidth - on.offsetWidth) / 2;
        }
      }
      edges();
    }).observe(nav);
  }
  return nav;
}
