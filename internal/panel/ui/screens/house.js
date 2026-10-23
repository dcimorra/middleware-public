// The house's own conversation, and every agent turn at work around it.
// What the agents cost is the LLM usage screen's.

import { h, clear, fmtAgo } from "../lib/dom.js";
import { onEvent } from "../lib/stream.js";
import { confirm } from "../components/dialog.js";
import { toast, toastError } from "../components/toast.js";
import { t } from "../lib/i18n.js";
import { api } from "../lib/api.js";
import { page, table, cell } from "../components/page.js";
import { chatTab } from "./site-chat.js";

export function houseScreen() {
  const p = page({ title: t("nav.house"), sub: t("house.sub"), icon: "sparkles" });
  const chat = chatTab("middleware");
  // Every agent turn at work in the house, and every conversation waiting
  // for the operator: the one place to see, and stop, what the agents of
  // every site are doing.
  const live = h("div", { class: "card pad0" });
  p.body.append(live, chat.el);
  // The paths of a conversation, by its name: the house's, a site's
  // first, or another of a site's.
  const convPath = (name) => {
    const [domain, id] = name.split("#");
    if (domain === "middleware") return { api: "/chat/middleware", href: "#/house" };
    return id ? { api: `/sites/${encodeURIComponent(domain)}/chats/${encodeURIComponent(id)}`, href: `#/sites/${encodeURIComponent(domain)}/chat` }
      : { api: `/sites/${encodeURIComponent(domain)}/chat`, href: `#/sites/${encodeURIComponent(domain)}/chat` };
  };
  async function loadLive() {
    let cap, w;
    try { [cap, w] = await Promise.all([api.get("/agents/capacity"), api.get("/agents/waiting")]); } catch (e) { clear(live).append(h("p", { class: "card-pad small muted" }, e.message)); return; }
    const rows = (cap.running || []).map((r) => {
      const c = convPath(r.domain);
      const stop = h("button", { type: "button", class: "btn sm danger" }, t("chat.stop"));
      stop.addEventListener("click", async () => {
        if (!(await confirm({ title: t("house.stopQ", { name: r.domain }), body: t("house.stopBody"), confirmLabel: t("chat.stop"), danger: true }))) return;
        try { await api.post(c.api + "/stop"); toast(t("chat.stopping"), "ok"); setTimeout(loadLive, 1500); } catch (e) { toastError(e); }
      });
      return h("tr", null,
        cell(h("a", { href: c.href }, r.domain), null, "primary"),
        cell(r.provider, t("house.col.provider")),
        cell(fmtAgo(r.started, t), t("house.col.since")),
        cell(stop, null, "right"));
    });
    const asking = (w.asking || []).map((a) => h("li", null, h("a", { href: convPath(a.name).href }, a.name)));
    clear(live).append(...[
      h("div", { class: "log-bar card-pad" }, h("h2", { class: "grow" }, t("house.live")), h("span", { class: "small muted" }, t("house.liveCount", { used: cap.used, max: cap.max }))),
      rows.length ? table([{ label: t("house.col.conversation") }, { label: t("house.col.provider") }, { label: t("house.col.since") }, { label: "" }], rows, { responsive: true }).el
        : h("p", { class: "card-pad small muted" }, t("house.noneRunning")),
      asking.length ? h("div", { class: "card-pad" }, h("h3", null, t("house.waiting", { n: asking.length })), h("ul", null, ...asking)) : null].filter(Boolean));
  }
  loadLive();
  const subs = [onEvent("agent.", (ev) => { if (/^agent\.(started|done|error|question)$/.test(ev.type)) loadLive(); })];

  return { el: p.el, destroy() { chat.destroy(); subs.forEach((u) => u()); } };
}
