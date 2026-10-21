// Who called at this site: countries, robots, browsers and the User-Agents
// no table knows, heaviest first. It is what a security rule is written
// from: closing a country or a crawler without it meant typing from memory.

import { h, clear, fmtAgo } from "../lib/dom.js";
import { t } from "../lib/i18n.js";
import { api, enc } from "../lib/api.js";
import { table, cell, empty, notice } from "../components/page.js";
import { confirm } from "../components/dialog.js";
import { toastError } from "../components/toast.js";
import { icon } from "../lib/icons.js";

export function seenCard(domain) {
  const el = h("div", { class: "card" });
  const path = `/sites/${enc(domain)}/security/seen`;
  async function load() {
    let tb;
    try { tb = await api.get(path); } catch (e) { clear(el).append(h("h2", null, t("seen.title")), notice("bad", e.message)); return; }
    const forget = h("button", { type: "button", class: "btn sm ghost" }, icon("trash"), t("seen.forget"));
    forget.addEventListener("click", async () => {
      if (!(await confirm({ title: t("seen.forgetQ"), body: t("seen.forgetBody"), confirmLabel: t("seen.forget") }))) return;
      try { await api.post(path + "/forget"); load(); } catch (e) { toastError(e); }
    });
    clear(el).append(
      h("div", { class: "log-bar" }, h("h2", { class: "grow" }, t("seen.title")), forget),
      h("p", { class: "small muted" }, t("seen.hint", { n: tb.total || 0, since: tb.since ? new Date(tb.since).toLocaleString() : "—" })));
    if (!tb.total) { el.append(empty(t("seen.none"), "")); return; }
    const section = (key, rows, extraLabel) => {
      if (!rows || !rows.length) return null;
      const top = rows.slice(0, 15);
      return h("div", null, h("h3", null, t("seen." + key)),
        table([{ label: t("seen.what") }, ...(extraLabel ? [{ label: extraLabel }] : []), { label: t("seen.calls"), class: "num" }, { label: t("seen.last") }],
          top.map((e) => h("tr", null,
            cell(key === "agents" ? h("code", { class: "small" }, e.name || e.key) : (e.name || e.key || "—"), null, "primary"),
            ...(extraLabel ? [cell(e.extra || "", extraLabel, "small")] : []),
            cell(String(e.count), t("seen.calls"), "num mono"),
            cell(fmtAgo(e.last, t), t("seen.last")))), { responsive: true }).el,
        rows.length > 15 ? h("p", { class: "small muted" }, t("seen.more", { n: rows.length - 15 })) : null);
    };
    el.append(h("div", { class: "grid-2" },
      section("countries", tb.countries),
      section("browsers", tb.browsers),
      section("bots", tb.bots, t("seen.family")),
      section("agents", tb.agents)));
  }
  load();
  return el;
}
