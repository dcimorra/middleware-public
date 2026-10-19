// A per-second sparkline as inline SVG. Attributes only, no style: the
// CSP would drop an inline style and the line would vanish silently.

import { svg, h } from "../lib/dom.js";
import { t } from "../lib/i18n.js";

export function sparkline({ width = 600, height = 72, label } = {}) {
  const W = width, H = height, pad = 2;
  const el = svg("svg", { class: "spark", viewBox: `0 0 ${W} ${H}`, preserveAspectRatio: "none", role: "img", "aria-label": label || "" });
  const base = svg("line", { class: "base", x1: 0, x2: W, y1: H - pad, y2: H - pad });
  const area = svg("path", { class: "area", d: "" });
  const line = svg("path", { class: "line", d: "" });
  el.append(base, area, line);
  const peak = h("span", { class: "peak" });
  const wrap = h("div", { class: "spark-wrap" }, el, peak);

  function set(values) {
    const v = (values || []).map((x) => Number(x) || 0);
    if (v.length < 2) { area.setAttribute("d", ""); line.setAttribute("d", ""); peak.textContent = ""; return; }
    const max = Math.max(1, ...v);
    const step = W / (v.length - 1);
    const y = (x) => H - pad - (x / max) * (H - 2 * pad);
    let d = "";
    v.forEach((x, i) => { d += (i === 0 ? "M" : "L") + (i * step).toFixed(1) + " " + y(x).toFixed(1) + " "; });
    line.setAttribute("d", d.trim());
    area.setAttribute("d", `${d.trim()} L${W} ${H - pad} L0 ${H - pad} Z`);
    peak.textContent = t("spark.peak", { n: max });
    el.setAttribute("aria-label", t("spark.label", { now: v[v.length - 1], max }));
  }
  return { el: wrap, set };
}
