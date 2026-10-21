// Names that reached this house and belong to no site: a tunnel rule that
// outlived its site, a DNS record pointing here, a scanner trying a Host.
// The door and the samples are what tell one from the other.

import { h, clear, fmtAgo } from "../lib/dom.js";
import { t } from "../lib/i18n.js";
import { api, enc } from "../lib/api.js";
import { table, cell, empty, notice } from "../components/page.js";
import { toastError } from "../components/toast.js";
import { icon } from "../lib/icons.js";

export function unroutedCard() {
  const el = h("div", { class: "card pad0" });
  async function load() {
    let list;
    try { list = await api.get("/unrouted"); } catch (e) { clear(el).append(h("div", { class: "card-pad" }, h("h2", null, t("unrouted.title")), notice("bad", e.message))); return; }
    const refresh = h("button", { type: "button", class: "btn sm ghost", "aria-label": t("common.refresh") }, icon("refresh"));
    refresh.addEventListener("click", load);
    clear(el).append(h("div", { class: "log-bar card-pad" }, h("h2", { class: "grow" }, t("unrouted.title")), refresh),
      h("p", { class: "small muted card-pad" }, t("unrouted.hint")));
    if (!list.length) { el.append(empty(t("unrouted.none"), t("unrouted.noneHint"))); return; }
    const rows = list.map((k) => {
      const forget = h("button", { type: "button", class: "btn sm ghost", title: t("unrouted.forget") }, icon("x"));
      forget.addEventListener("click", async () => { try { await api.del(`/unrouted/${enc(k.host)}`); load(); } catch (e) { toastError(e); } });
      const doors = Object.entries(k.doors || {}).map(([d, n]) => `${t("inbound.door." + d)} ${n}`).join(" · ");
      const samples = h("details", null, h("summary", { class: "small" }, t("unrouted.samples", { n: (k.samples || []).length })),
        h("div", { class: "stack-sm small mono" }, ...(k.samples || []).slice().reverse().map((s) => h("div", null,
          `${new Date(s.when).toLocaleTimeString()} ${s.method} ${s.path}  ${s.ip || ""}`,
          s.proxy ? h("span", { class: "badge warn" }, t("unrouted.proxy")) : null,
          s.tls && s.sni && s.sni !== k.host ? h("span", { class: "badge warn" }, `SNI ${s.sni}`) : null,
          s.agent ? h("div", { class: "dim" }, s.agent) : null))));
      return h("tr", null,
        cell(h("div", null, h("code", null, k.host), samples), null, "primary"),
        cell(String(k.count), t("unrouted.calls"), "num mono"),
        cell(doors || "—", t("unrouted.doors"), "small"),
        cell(fmtAgo(k.last, t), t("unrouted.last")),
        cell(forget, null, "right"));
    });
    el.append(table([{ label: t("unrouted.name") }, { label: t("unrouted.calls"), class: "num" }, { label: t("unrouted.doors") }, { label: t("unrouted.last") }, { label: "" }], rows, { responsive: true }).el);
  }
  load();
  return el;
}
