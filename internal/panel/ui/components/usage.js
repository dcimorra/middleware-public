// A subscription's usage windows: a bar each, how much is spent and when
// it starts again, and where the figure came from. The house screen shows
// every subscription with it, a module's card its own, and a chat a short
// line for the agent it is talking to.

import { h, fmtAgo } from "../lib/dom.js";
import { t } from "../lib/i18n.js";

const NAMED = new Set(["5h", "7d", "7d_opus", "7d_sonnet", "7d_extra"]);
// The windows a subscription always has: shown even before anything says
// their figure, so a missing one reads "unknown" and not "not there".
const ALWAYS = { claude: ["5h", "7d"], openai: ["5h", "7d"] };

// A window the house has a word for is said in it; one it has not
// (Antigravity's model groups) by the vendor's own name.
function windowLabel(w) {
  return NAMED.has(w.name) ? t("usage.window." + w.name) : w.label || w.name;
}

function shortLabel(w) {
  return NAMED.has(w.name) ? t("usage.short." + w.name) : w.label || w.name;
}

// fillOf is the coloured part of a meter and the figure beside it.
function fillOf(w) {
  const known = typeof w.used === "number" && w.used >= 0;
  const pct = known ? Math.max(0, Math.min(100, w.used)) : 0;
  // The width through CSSOM: the panel's CSP refuses style attributes.
  const fill = h("div", { class: "fill" + (w.status === "rejected" || pct >= 90 ? " bad" : pct >= 70 || w.status === "allowed_warning" ? " warn" : "") });
  fill.style.width = `${w.status === "rejected" && !known ? 100 : pct}%`;
  const figure = known ? `${pct.toFixed(0)}%` : w.status === "rejected" ? t("usage.reached") : t("usage.unknown");
  return { fill, figure, pct };
}

function bar(w) {
  const label = windowLabel(w);
  const { fill, figure, pct } = fillOf(w);
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
    const ws = [...u.windows];
    (ALWAYS[st.provider] || []).forEach((name, i) => { if (!ws.some((w) => w.name === name)) ws.splice(i, 0, { name, used: -1 }); });
    out.append(...ws.map(bar));
    out.append(h("p", { class: "small muted" }, t(u.source === "turn" ? "usage.fromTurn" : "usage.fromVendor", { when: fmtAgo(u.at, t) }), u.plan ? " · " + t("usage.plan", { plan: u.plan }) : ""));
  } else {
    out.append(h("p", { class: "small muted" }, t(st.supported === false ? "usage.unsupported" : "usage.none")));
  }
  if (st.error) out.append(h("p", { class: "small muted" }, t("usage.vendorSaid", { error: st.error })));
  return out;
}

// usageLine is a chat's: every window of the agent it talks to, short, a
// thin bar each and the figure, the reset in the tooltip. Empty (and
// hidden) when nothing is known of that agent's limits.
export function usageLine(st) {
  const u = st && st.usage;
  const out = h("div", { class: "chat-usage small", role: "status", "aria-label": t("usage.title") });
  if (!u || !u.windows || !u.windows.length) { out.hidden = true; return out; }
  for (const w of u.windows) {
    const { fill, figure, pct } = fillOf(w);
    const title = windowLabel(w) + (w.resets_at ? " · " + t("usage.resets", { when: fmtAgo(w.resets_at, t), at: new Date(w.resets_at).toLocaleString() }) : "");
    out.append(h("span", { class: "chat-usage-w", title },
      h("span", { class: "muted" }, shortLabel(w)),
      h("span", { class: "meter", role: "meter", "aria-valuenow": String(pct), "aria-valuemin": "0", "aria-valuemax": "100", "aria-label": windowLabel(w) }, fill),
      h("strong", null, figure)));
  }
  out.append(h("span", { class: "muted chat-usage-src" }, t(u.source === "turn" ? "usage.fromTurn" : "usage.fromVendor", { when: fmtAgo(u.at, t) })));
  return out;
}
