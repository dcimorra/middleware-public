import { h } from "../lib/dom.js";
import { icon } from "../lib/icons.js";

const box = () => document.getElementById("toasts");

export function toast(message, kind = "", ms = 4000) {
  const ico = kind === "bad" ? "alert" : kind === "ok" ? "check" : "info";
  const el = h("div", { class: "toast " + kind, role: kind === "bad" ? "alert" : "status" }, icon(ico), h("span", null, message));
  box().append(el);
  setTimeout(() => el.remove(), ms);
  return el;
}

export function toastError(e) {
  toast(e && e.message ? e.message : String(e), "bad", 6000);
}
