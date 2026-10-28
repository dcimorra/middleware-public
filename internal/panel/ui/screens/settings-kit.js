// Pieces the configuration screens (Settings, Modules, System) share and
// the panel's components do not have: a card head, a save button that
// says beside it what happened, and a fold for the fields few people
// change. Kept next to these screens rather than in components/ until a
// screen elsewhere wants them as well.

import { h, clear } from "../lib/dom.js";
import { t } from "../lib/i18n.js";
import { icon } from "../lib/icons.js";

// The head of a card: its name, one quiet line on what it is for, and
// whatever acts on the whole card on the right. The hint is capped to a
// reading width: across a 1,100px card it was a line nobody finished.
export function cardHead(title, hint, ...actions) {
  return h("div", { class: "set-head" },
    h("div", { class: "set-head-text" }, h("h2", null, title), hint ? hintLine(hint) : null),
    actions.length ? h("div", { class: "set-head-actions" }, ...actions) : null);
}

// A hint of a paragraph (the house's AI for automatic work explains six
// lines of what it does) shows two lines and the rest on demand: read
// once, it is in the way every time after.
function hintLine(text) {
  const p = h("p", { class: "set-hint" }, text);
  if (text.length < 220) return p;
  p.classList.add("clamped");
  const more = h("button", { type: "button", class: "set-hint-more", "aria-expanded": "false" }, t("ops.more"));
  more.addEventListener("click", () => {
    const open = p.classList.toggle("clamped") === false;
    more.setAttribute("aria-expanded", String(open));
    more.textContent = open ? t("ops.less") : t("ops.more");
  });
  return h("div", null, p, more);
}

// saveBar is the save button with its state beside it: nothing at first,
// "unsaved changes" once a field under the watched element moves, "saved
// at 20:31" once the server took it. A toast in the corner of a long
// page said "saved" without saying which of fifteen cards it meant, and
// nothing at all said a card had changes still to save.
export function saveBar(button, watch) {
  const state = h("span", { class: "save-state", role: "status", "aria-live": "polite" });
  const el = h("div", { class: "form-actions save-bar" }, button, state);
  const show = (kind, ...content) => {
    state.dataset.state = kind;
    clear(state).append(...content);
  };
  const bar = {
    el,
    dirty() { show("dirty", h("span", { class: "save-dot", "aria-hidden": "true" }), t("ops.unsaved")); },
    // extra is what the operator should know about this save in
    // particular ("some take effect at the next restart").
    saved(extra) {
      const when = new Date().toLocaleTimeString(undefined, { hour: "2-digit", minute: "2-digit" });
      show("saved", icon("check", { size: 14 }), t("ops.savedAt", { time: when }) + (extra ? " · " + extra : ""));
    },
    reset() { show("", ""); },
    watch(node) {
      node.addEventListener("input", () => bar.dirty());
      node.addEventListener("change", () => bar.dirty());
      return bar;
    },
  };
  if (watch) bar.watch(watch);
  return bar;
}

// A switch that saves the moment it moves says so on its own line, as
// saveBar does for a form.
export function savedNote() {
  const el = h("span", { class: "save-state", role: "status", "aria-live": "polite" });
  return {
    el,
    saved() {
      const when = new Date().toLocaleTimeString(undefined, { hour: "2-digit", minute: "2-digit" });
      el.dataset.state = "saved";
      clear(el).append(icon("check", { size: 14 }), t("ops.savedAt", { time: when }));
    },
  };
}

// quietEmpty is "nothing here yet" inside a card: one line with a small
// icon. The page-sized empty state (48px of padding, a 40px icon) made a
// card with no tokens the tallest thing on the screen.
export function quietEmpty(ico, title, hint) {
  return h("div", { class: "set-empty" }, icon(ico, { size: 18 }),
    h("div", null, h("strong", null, title), hint ? h("span", null, hint) : null));
}

// fold keeps the fields that are right for nearly everyone out of the
// way: a quiet line with what they hold now, opened on demand. `open`
// unfolds it from the start, for a fold whose fields say something the
// operator should see (a deny list that is not empty).
export function fold(label, note, children, open) {
  return h("details", { class: "set-more", open: Boolean(open) },
    h("summary", null, h("span", { class: "set-more-label" }, label), note ? h("span", { class: "set-more-note" }, note) : null),
    h("div", { class: "set-more-body" }, ...children));
}
