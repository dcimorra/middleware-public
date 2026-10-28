// Names that reached this house and belong to no site: a tunnel rule that
// outlived its site, a DNS record pointing here, a scanner trying a Host.
// The door and the samples are what tell one from the other.
//
// Usually there are none, and the card then is one quiet line: it was a
// full empty state with a box drawn in it, the tallest thing on the
// Inbound screen, for nothing to see.

import { h, clear, fmtAgo } from "../lib/dom.js";
import { t } from "../lib/i18n.js";
import { api, enc } from "../lib/api.js";
import { table, cell, notice } from "../components/page.js";
import { toastError } from "../components/toast.js";
import { icon } from "../lib/icons.js";

export function unroutedCard() {
  const el = h("div", { class: "card pad0 np-unrouted" });
  async function load() {
    let list;
    try { list = await api.get("/unrouted"); } catch (e) { clear(el).append(h("div", { class: "card-pad" }, h("h2", null, t("unrouted.title")), notice("bad", e.message))); return; }
    const refresh = h("button", { type: "button", class: "icon-btn sm", "aria-label": t("common.refresh"), title: t("common.refresh") }, icon("refresh"));
    refresh.addEventListener("click", load);
    clear(el).append(h("div", { class: "log-bar card-pad np-card-head" },
      h("h2", { class: "grow" }, t("unrouted.title"), list.length ? h("span", { class: "np-quiet" }, String(list.length)) : null), refresh));
    if (!list.length) {
      el.append(h("p", { class: "np-none card-pad" }, icon("check", { size: 16 }), h("span", null, t("unrouted.noneHint"))));
      return;
    }
    el.append(h("p", { class: "np-lead card-pad" }, t("unrouted.hint")));
    const rows = list.map((k) => {
      const forget = h("button", { type: "button", class: "icon-btn sm", title: t("unrouted.forget"), "aria-label": t("unrouted.forget") }, icon("x"));
      forget.addEventListener("click", async () => { try { await api.del(`/unrouted/${enc(k.host)}`); load(); } catch (e) { toastError(e); } });
      const doors = Object.entries(k.doors || {}).map(([d, n]) => `${t("inbound.door." + d)} ${n}`).join(" · ");
      const samples = h("details", { class: "np-more" }, h("summary", null, t("unrouted.samples", { n: (k.samples || []).length })),
        h("div", { class: "stack-sm small mono np-samples" }, ...(k.samples || []).slice().reverse().map((s) => h("div", null,
          `${new Date(s.when).toLocaleTimeString()} ${s.method} ${s.path}  ${s.ip || ""}`,
          s.proxy ? h("span", { class: "badge warn" }, t("unrouted.proxy")) : null,
          s.tls && s.sni && s.sni !== k.host ? h("span", { class: "badge warn" }, `SNI ${s.sni}`) : null,
          s.agent ? h("div", { class: "dim" }, s.agent) : null))));
      return h("tr", null,
        cell(h("div", null, h("code", null, k.host), samples), null, "primary"),
        cell(String(k.count), t("unrouted.calls"), "num mono"),
        cell(doors || "—", t("unrouted.doors"), "small"),
        cell(h("span", { title: new Date(k.last).toLocaleString() }, fmtAgo(k.last, t)), t("unrouted.last")),
        cell(forget, null, "right"));
    });
    el.append(table([{ label: t("unrouted.name") }, { label: t("unrouted.calls"), class: "num" }, { label: t("unrouted.doors") }, { label: t("unrouted.last") }, { label: "" }], rows, { responsive: true }).el);
  }
  load();
  return el;
}
