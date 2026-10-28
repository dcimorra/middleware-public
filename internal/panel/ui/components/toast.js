import { h } from "../lib/dom.js";
import { icon } from "../lib/icons.js";

const box = () => document.getElementById("toasts");

// A toast leaves with a short fade rather than vanishing mid-sentence, and
// a tap sends it away at once: on a phone it sits over the screen, and the
// operator who has read it should not have to wait four seconds to see
// what is under it.
export function toast(message, kind = "", ms = 4000) {
  const ico = kind === "bad" ? "alert" : kind === "ok" ? "check" : "info";
  const el = h("div", { class: "toast " + kind, role: kind === "bad" ? "alert" : "status" }, icon(ico), h("span", null, message));
  let gone = false;
  const leave = () => {
    if (gone) return;
    gone = true;
    el.classList.add("leaving");
    setTimeout(() => el.remove(), 180);
  };
  el.addEventListener("click", leave);
  box().append(el);
  setTimeout(leave, ms);
  return el;
}

export function toastError(e) {
  toast(e && e.message ? e.message : String(e), "bad", 6000);
}
