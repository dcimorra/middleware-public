import { h } from "../lib/dom.js";

const box = () => document.getElementById("toasts");

export function toast(message, kind = "", ms = 4000) {
  const el = h("div", { class: "toast " + kind, role: kind === "bad" ? "alert" : "status" }, message);
  box().append(el);
  setTimeout(() => el.remove(), ms);
  return el;
}

export function toastError(e) {
  toast(e && e.message ? e.message : String(e), "bad", 6000);
}
