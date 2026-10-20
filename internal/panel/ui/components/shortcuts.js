// Keyboard shortcuts. "/" focuses the search box of the screen, "?" shows
// this list, "g" then a letter jumps to a section. Nothing fires while
// the operator is typing in a field, which is the bug every shortcut
// scheme ships once.

import { h } from "../lib/dom.js";
import { t } from "../lib/i18n.js";
import { open } from "./dialog.js";
import { navigate } from "../lib/router.js";

export const GOTO = {
  s: "/sites", i: "/inbound", w: "/waf", m: "/modules", t: "/tasks", h: "/house", e: "/events", g: "/guard", c: "/settings", y: "/system",
};

let pendingG = false;
let pendingTimer = null;

export function installShortcuts() {
  document.addEventListener("keydown", (e) => {
    const tag = (e.target && e.target.tagName) || "";
    const typing = tag === "INPUT" || tag === "TEXTAREA" || tag === "SELECT" || (e.target && e.target.isContentEditable);
    if (e.metaKey || e.ctrlKey || e.altKey) return;
    if (typing) {
      if (e.key === "Escape" && tag !== "TEXTAREA") e.target.blur();
      return;
    }
    if (document.querySelector("dialog[open]")) return;
    if (pendingG) {
      pendingG = false;
      clearTimeout(pendingTimer);
      const to = GOTO[e.key.toLowerCase()];
      if (to) { e.preventDefault(); navigate(to); }
      return;
    }
    if (e.key === "/") {
      const box = document.querySelector("#main input[type=search]");
      if (box) { e.preventDefault(); box.focus(); box.select(); }
      else { e.preventDefault(); navigate("/sites?focus=search"); }
    } else if (e.key === "?") {
      e.preventDefault();
      showShortcuts();
    } else if (e.key === "g") {
      pendingG = true;
      pendingTimer = setTimeout(() => { pendingG = false; }, 1500);
    } else if (e.key === "n") {
      const btn = document.querySelector("#main [data-shortcut=new]");
      if (btn) { e.preventDefault(); btn.click(); }
    }
  });
}

export function showShortcuts() {
  const rows = [
    ["/", t("keys.search")],
    ["?", t("keys.help")],
    ["n", t("keys.new")],
    ["Esc", t("keys.escape")],
    ["g s", t("nav.sites")],
    ["g i", t("nav.inbound")],
    ["g w", t("nav.waf")],
    ["g m", t("nav.modules")],
    ["g t", t("nav.tasks")],
    ["g e", t("nav.events")],
    ["g g", t("nav.guard")],
    ["g c", t("nav.settings")],
    ["g y", t("nav.system")],
  ];
  const dl = h("dl", { class: "shortcuts" });
  for (const [k, label] of rows) {
    dl.append(h("dt", null, ...k.split(" ").map((x, i) => [i ? " " : null, h("kbd", null, x)])), h("dd", null, label));
  }
  return open({ title: t("keys.title"), body: dl });
}
