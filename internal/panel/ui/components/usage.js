// A subscription's usage windows: a bar each, how much is spent and when
// it starts again, and where the figure came from. The house screen shows
// every subscription with it, and a module's card its own.

import { h, fmtAgo } from "../lib/dom.js";
import { t } from "../lib/i18n.js";

const NAMED = new Set(["5h", "7d", "7d_opus", "7d_sonnet"]);

function windowLabel(name) {
  return NAMED.has(name) ? t("usage.window." + name) : name;
}

function bar(w) {
  const label = windowLabel(w.name);
  const known = typeof w.used === "number" && w.used >= 0;
  const pct = known ? Math.max(0, Math.min(100, w.used)) : 0;
  // The width through CSSOM: the panel's CSP refuses style attributes.
  const fill = h("div", { class: "fill" + (w.status === "rejected" || pct >= 90 ? " bad" : pct >= 70 || w.status === "allowed_warning" ? " warn" : "") });
  fill.style.width = `${w.status === "rejected" && !known ? 100 : pct}%`;
  const figure = known ? `${pct.toFixed(0)}%` : w.status === "rejected" ? t("usage.reached") : t("usage.unknown");
  return h("div", { class: "usage-row" },
    h("div", { class: "row" }, h("span", { class: "grow" }, label), h("strong", null, figure)),
    h("div", { class: "meter", role: "meter", "aria-valuenow": String(pct), "aria-valuemin": "0", "aria-valuemax": "100", "aria-label": label }, fill),
    w.resets_at ? h("div", { class: "small muted" }, t("usage.resets", { when: fmtAgo(w.resets_at, t), at: new Date(w.resets_at).toLocaleString() })) : null);
}

// usageBlock renders one entry of /agents/usage (or a module's /usage).
export function usageBlock(st) {
  const u = st.usage;
  const out = h("div", { class: "usage" });
  if (u && u.windows && u.windows.length) {
    out.append(...u.windows.map(bar));
    out.append(h("p", { class: "small muted" }, t(u.source === "turn" ? "usage.fromTurn" : "usage.fromVendor", { when: fmtAgo(u.at, t) }), u.plan ? " · " + t("usage.plan", { plan: u.plan }) : ""));
  } else {
    out.append(h("p", { class: "small muted" }, t(st.supported === false ? "usage.unsupported" : "usage.none")));
  }
  if (st.error) out.append(h("p", { class: "small muted" }, t("usage.vendorSaid", { error: st.error })));
  return out;
}
