// What moves left on this node, seen and collected: receptions that never
// finished, earlier copies a commit set aside, sites placed here that were
// not taken out. Listed by the server (GET /relocate/leftovers), removed
// only as listed, after the operator ticks and confirms.

import { icon } from "../lib/icons.js";
import { h, clear, fmtBytes, fmtAgo } from "../lib/dom.js";
import { t } from "../lib/i18n.js";
import { api } from "../lib/api.js";
import { confirm } from "../components/dialog.js";
import { toast, toastError } from "../components/toast.js";

export function leftoversCard() {
  const el = h("div", { class: "card" });
  const list = h("div", { class: "stack-sm" });
  const cleanBtn = h("button", { type: "button", class: "btn danger" }, icon("trash"), t("leftovers.clean"));
  el.append(h("h2", null, t("leftovers.title")), h("p", { class: "small muted" }, t("leftovers.hint")), list, h("div", { class: "row" }, cleanBtn));
  let items = [];

  function selected() { return [...list.querySelectorAll("input[type=checkbox]:checked")].map((c) => c.value); }
  function paint() {
    clear(list);
    cleanBtn.hidden = !items.length;
    if (!items.length) { list.append(h("p", { class: "small muted" }, t("leftovers.none"))); return; }
    const total = items.reduce((n, l) => n + l.bytes, 0);
    list.append(h("p", { class: "small" }, t("leftovers.total", { n: items.length, size: fmtBytes(total) })));
    for (const l of items) {
      const box = h("input", { type: "checkbox", value: l.path, checked: true });
      list.append(h("label", { class: "leftover" }, box,
        h("span", { class: "badge" }, t("leftovers.kind." + l.kind)),
        h("span", { class: "grow" }, h("strong", null, l.domain), " ", h("code", { class: "small" }, l.path)),
        h("span", { class: "small muted mono" }, fmtBytes(l.bytes) + " · " + fmtAgo(l.modified, t))));
    }
  }
  async function load() {
    // null when there is nothing: Go sends an empty list as null.
    try { items = (await api.get("/relocate/leftovers")) || []; } catch { items = []; }
    paint();
  }
  cleanBtn.addEventListener("click", async () => {
    const paths = selected();
    if (!paths.length) return;
    if (!(await confirm({ title: t("leftovers.cleanQ", { n: paths.length }), body: t("leftovers.cleanBody"), confirmLabel: t("leftovers.clean"), danger: true }))) return;
    try {
      const r = await api.post("/relocate/leftovers/clean", { paths });
      const failed = Object.entries(r.results || {}).filter(([, v]) => v !== "removed");
      if (failed.length) toast(failed.map(([p, v]) => p + ": " + v).join("\n"), "bad");
      else toast(t("leftovers.cleaned", { n: paths.length }), "ok");
    } catch (e) { toastError(e); }
    load();
  });
  load();
  return { el, destroy() {} };
}
