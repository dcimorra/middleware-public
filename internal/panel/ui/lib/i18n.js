// The string catalog. Every user-visible string in the panel is a t() call:
// a missing key shows as the key itself, loudly, instead of a blank, and
// a Go test greps the sources so a missing key fails the build rather
// than the operator. Other languages are another JSON file next to en.json.

let strings = {};
let lang = "en";

export const LANGUAGES = ["en", "es"];

// Each language by its own name, which is how someone looking for theirs
// recognises it.
export const LANGUAGE_NAMES = { en: "English", es: "Español" };

export async function loadStrings(code) {
  const want = LANGUAGES.includes(code) ? code : "en";
  const res = await fetch(`/assets/i18n/${want}.json`, { cache: "no-cache" });
  if (!res.ok) throw new Error(`catalog ${want}: ${res.status}`);
  strings = await res.json();
  lang = want;
  document.documentElement.lang = want;
  return want;
}

export function currentLang() { return lang; }

export function t(key, vars) {
  let s = strings[key];
  if (s === undefined) {
    console.warn("i18n: missing", key);
    s = key;
  }
  if (vars) {
    for (const [k, v] of Object.entries(vars)) {
      s = s.split(`{${k}}`).join(String(v));
    }
  }
  return s;
}

export function has(key) { return key in strings; }
