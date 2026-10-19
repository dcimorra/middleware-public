// Theme follows prefers-color-scheme unless the operator picked one. The
// choice is per browser (localStorage), not a setting on the server: a
// dark laptop and a light phone are both right.

const KEY = "mw.theme";
const LANG = "mw.lang";

export function getTheme() {
  try { return localStorage.getItem(KEY) || "auto"; } catch { return "auto"; }
}

export function setTheme(v) {
  try {
    if (v === "auto") localStorage.removeItem(KEY);
    else localStorage.setItem(KEY, v);
  } catch { /* private window: the choice lasts the session */ }
  applyTheme(v);
}

export function applyTheme(v = getTheme()) {
  const root = document.documentElement;
  if (v === "light" || v === "dark") root.dataset.theme = v;
  else delete root.dataset.theme;
}

export function getLang() {
  try { return localStorage.getItem(LANG) || (navigator.language || "en").slice(0, 2); } catch { return "en"; }
}

export function setLang(v) {
  try { localStorage.setItem(LANG, v); } catch { /* same */ }
}
