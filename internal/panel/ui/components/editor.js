// A JSON editor that is a textarea. Line numbers are CSS counters on a
// gutter of empty spans, one per line, which is all a manifest needs; a
// syntax tree would be a dependency and the CSP would fight a worker. The
// browser's JSON.parse gives the error position, which is turned into a
// line so the gutter can mark it.

import { h } from "../lib/dom.js";
import { t } from "../lib/i18n.js";

export function jsonEditor({ value, onSave, saveLabel, rows, readOnly }) {
  const ta = h("textarea", { spellcheck: "false", autocapitalize: "off", autocomplete: "off", "aria-label": t("editor.label"), readOnly: Boolean(readOnly) });
  const gutter = h("div", { class: "gutter", "aria-hidden": "true" });
  const box = h("div", { class: "editor" }, gutter, ta);
  const msg = h("span", { class: "msg", role: "status" });
  const saveBtn = h("button", { type: "button", class: "btn primary", hidden: Boolean(readOnly) }, saveLabel || t("common.save"));
  const formatBtn = h("button", { type: "button", class: "btn", hidden: Boolean(readOnly) }, t("editor.format"));
  const bar = h("div", { class: "editor-bar" }, saveBtn, formatBtn, msg);
  const el = h("div", null, box, bar);
  let badLine = -1;
  let dirty = false;

  function lines() {
    const n = ta.value.split("\n").length;
    while (gutter.childElementCount < n) gutter.append(h("span"));
    while (gutter.childElementCount > n) gutter.lastElementChild.remove();
    for (let i = 0; i < gutter.childElementCount; i++) {
      gutter.children[i].classList.toggle("bad", i === badLine);
    }
  }
  // setValue keeps the status line: a save answers with the new object, and
  // "Saved" wiped by the refresh it causes looks like nothing happened.
  function setValue(v) {
    ta.value = typeof v === "string" ? v : JSON.stringify(v, null, 2);
    badLine = -1;
    dirty = false;
    lines();
  }
  function say(text, kind) {
    msg.textContent = text;
    msg.className = "msg " + (kind || "");
  }
  function parse() {
    try {
      const v = JSON.parse(ta.value);
      badLine = -1;
      lines();
      ta.removeAttribute("aria-invalid");
      return { ok: true, value: v };
    } catch (e) {
      const m = /position (\d+)/.exec(e.message);
      if (m) badLine = ta.value.slice(0, Number(m[1])).split("\n").length - 1;
      else {
        const ln = /line (\d+)/.exec(e.message);
        badLine = ln ? Number(ln[1]) - 1 : -1;
      }
      lines();
      ta.setAttribute("aria-invalid", "true");
      say(t("editor.invalid", { msg: e.message, line: badLine >= 0 ? badLine + 1 : "?" }), "bad");
      return { ok: false, error: e };
    }
  }

  ta.addEventListener("input", () => { dirty = true; lines(); });
  ta.addEventListener("scroll", () => { gutter.scrollTop = ta.scrollTop; });
  ta.addEventListener("keydown", (e) => {
    if (e.key === "Tab" && !e.shiftKey && !readOnly) {
      // Tab inserts two spaces inside the editor; Escape then Tab leaves it,
      // which is the pattern screen readers already teach for code fields.
      if (ta.dataset.escaped === "1") { ta.dataset.escaped = ""; return; }
      e.preventDefault();
      const s = ta.selectionStart;
      ta.setRangeText("  ", s, ta.selectionEnd, "end");
      lines();
    } else if (e.key === "Escape") {
      ta.dataset.escaped = "1";
    } else if ((e.metaKey || e.ctrlKey) && e.key === "s" && !readOnly) {
      e.preventDefault();
      saveBtn.click();
    }
  });
  formatBtn.addEventListener("click", () => {
    const r = parse();
    if (r.ok) { ta.value = JSON.stringify(r.value, null, 2); lines(); say(t("editor.formatted"), "ok"); }
  });
  saveBtn.addEventListener("click", async () => {
    const r = parse();
    if (!r.ok) return;
    if (!onSave) return;
    saveBtn.classList.add("busy");
    try {
      await onSave(r.value, ta.value);
      dirty = false;
      say(t("editor.saved"), "ok");
    } catch (e) {
      say(e.message || String(e), "bad");
    } finally {
      saveBtn.classList.remove("busy");
    }
  });

  if (rows) ta.rows = rows;
  setValue(value === undefined ? "" : value);
  return { el, textarea: ta, setValue, parse, say, get dirty() { return dirty; }, get value() { return ta.value; } };
}
