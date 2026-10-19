// DOM building without innerHTML. Every string that reaches the page goes
// through textContent, so a domain, a log line or an event message can
// never run as markup; the CSP forbids inline anyway, and this keeps the
// escaping in one place instead of at every call site.

export function h(tag, attrs, ...children) {
  const el = document.createElement(tag);
  if (attrs && typeof attrs === "object" && !(attrs instanceof Node) && !Array.isArray(attrs)) {
    for (const [k, v] of Object.entries(attrs)) {
      if (v === null || v === undefined || v === false) continue;
      if (k === "class") el.className = v;
      else if (k === "dataset") Object.assign(el.dataset, v);
      else if (k.startsWith("on") && typeof v === "function") el.addEventListener(k.slice(2).toLowerCase(), v);
      else if (k === "value" && "value" in el) el.value = v;
      else if (k === "checked" || k === "disabled" || k === "selected" || k === "open" || k === "hidden" || k === "readOnly" || k === "required" || k === "autofocus") el[k] = Boolean(v);
      else el.setAttribute(k, v === true ? "" : String(v));
    }
  } else if (attrs !== null && attrs !== undefined) {
    children.unshift(attrs);
  }
  append(el, children);
  return el;
}

export function append(el, children) {
  for (const c of children.flat(Infinity)) {
    // An empty string is skipped so a cell with nothing in it stays :empty
    // and the phone layout can hide its label.
    if (c === null || c === undefined || c === false || c === "") continue;
    el.append(c instanceof Node ? c : document.createTextNode(String(c)));
  }
  return el;
}

export function clear(el) {
  while (el.firstChild) el.removeChild(el.firstChild);
  return el;
}

export function replace(el, ...children) {
  clear(el);
  return append(el, children);
}

// svg builds inside the SVG namespace; h() would create unknown HTML
// elements that render as nothing.
export function svg(tag, attrs, ...children) {
  const el = document.createElementNS("http://www.w3.org/2000/svg", tag);
  for (const [k, v] of Object.entries(attrs || {})) {
    if (v === null || v === undefined) continue;
    el.setAttribute(k, String(v));
  }
  append(el, children);
  return el;
}

export function on(el, type, fn, opts) {
  el.addEventListener(type, fn, opts);
  return () => el.removeEventListener(type, fn, opts);
}

export function fmtBytes(n) {
  if (n === null || n === undefined || isNaN(n)) return "";
  const units = ["B", "KB", "MB", "GB", "TB"];
  let i = 0;
  let v = Number(n);
  while (v >= 1024 && i < units.length - 1) { v /= 1024; i++; }
  return (i === 0 ? v : v.toFixed(v < 10 ? 1 : 0)) + " " + units[i];
}

export function fmtTime(iso) {
  if (!iso) return "";
  const d = new Date(iso);
  if (isNaN(d)) return String(iso);
  return d.toLocaleString(undefined, { dateStyle: "medium", timeStyle: "short" });
}

export function fmtClock(iso) {
  if (!iso) return "";
  const d = new Date(iso);
  if (isNaN(d)) return String(iso);
  return d.toLocaleTimeString(undefined, { hour: "2-digit", minute: "2-digit", second: "2-digit" });
}

// Relative times read faster in a list than timestamps, and the exact one
// is one hover away in the title attribute the callers set.
export function fmtAgo(iso, t) {
  if (!iso) return "";
  const d = new Date(iso);
  if (isNaN(d)) return String(iso);
  const s = Math.round((Date.now() - d.getTime()) / 1000);
  const abs = Math.abs(s);
  const future = s < 0;
  let out;
  if (abs < 45) out = t("time.now");
  else if (abs < 3600) out = t("time.minutes", { n: Math.round(abs / 60) });
  else if (abs < 86400) out = t("time.hours", { n: Math.round(abs / 3600) });
  else out = t("time.days", { n: Math.round(abs / 86400) });
  if (abs < 45) return out;
  return future ? t("time.in", { v: out }) : t("time.ago", { v: out });
}

export function fmtDuration(seconds) {
  if (seconds === null || seconds === undefined || isNaN(seconds)) return "";
  let s = Math.floor(Number(seconds));
  const d = Math.floor(s / 86400); s -= d * 86400;
  const hh = Math.floor(s / 3600); s -= hh * 3600;
  const mm = Math.floor(s / 60); s -= mm * 60;
  if (d > 0) return `${d}d ${hh}h`;
  if (hh > 0) return `${hh}h ${mm}m`;
  if (mm > 0) return `${mm}m ${s}s`;
  return `${s}s`;
}

export function shortRev(rev) {
  return rev ? String(rev).slice(0, 8) : "";
}

// debounce keeps a burst of stream events from re-rendering a table a
// hundred times a second; the last call wins after the pause.
export function debounce(fn, ms) {
  let timer = null;
  return (...args) => {
    clearTimeout(timer);
    timer = setTimeout(() => { timer = null; fn(...args); }, ms);
  };
}

export async function copy(text) {
  try {
    await navigator.clipboard.writeText(text);
    return true;
  } catch {
    return false;
  }
}

export function busy(btn, fn) {
  return async (...args) => {
    if (btn.classList.contains("busy")) return;
    btn.classList.add("busy");
    btn.setAttribute("aria-busy", "true");
    try {
      return await fn(...args);
    } finally {
      btn.classList.remove("busy");
      btn.removeAttribute("aria-busy");
    }
  };
}
