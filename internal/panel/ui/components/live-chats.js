// Who is talking, in the bar, from any screen: the conversations at work
// and the ones waiting for an answer, on every node of the network, a
// click away. As in V1: a turn can last twenty minutes, and the only way
// to know it was alive was to remember which site it was left in; a
// conversation that ended asking something looked finished and its
// question waited inside a site nobody was going to open. What the button
// says is whose turn it is: someone waiting, if anyone is.

import { h, clear, fmtAgo } from "../lib/dom.js";
import { t } from "../lib/i18n.js";
import { api } from "../lib/api.js";
import { store } from "../lib/store.js";
import { navigate } from "../lib/router.js";
import { setCurrentNode, currentNode } from "../lib/node.js";
import { onEvent } from "../lib/stream.js";

export function liveChats({ onCount } = {}) {
  const count = h("span", { class: "live-count" });
  const btn = h("button", { type: "button", class: "live-btn", "aria-haspopup": "true", "aria-expanded": "false" }, h("span", { class: "live-dot" }), count);
  const drop = h("div", { class: "live-drop", role: "menu", hidden: true });
  const el = h("div", { class: "live-chats", hidden: true }, btn, drop);
  let rows = [];

  function close() { drop.hidden = true; btn.setAttribute("aria-expanded", "false"); }
  btn.addEventListener("click", (e) => {
    e.stopPropagation();
    drop.hidden = !drop.hidden;
    btn.setAttribute("aria-expanded", String(!drop.hidden));
  });
  document.addEventListener("click", (e) => { if (!el.contains(e.target)) close(); });
  document.addEventListener("keydown", (e) => { if (e.key === "Escape") close(); });

  function open(r) {
    close();
    // The conversation itself, not the site's last one: the chat screen
    // reads this once when it opens.
    store.set("openChat", { domain: r.domain, id: r.id || "" });
    if ((currentNode() || "") !== r.node) setCurrentNode(r.node);
    navigate(r.domain === "middleware" ? "/house" : `/sites/${encodeURIComponent(r.domain)}/chat`);
  }

  function paint() {
    const asking = rows.filter((r) => r.asking).length;
    const talking = rows.filter((r) => !r.asking).length;
    if (onCount) onCount(asking, talking);
    el.hidden = rows.length === 0;
    if (!rows.length) { close(); return; }
    el.dataset.state = asking ? "asking" : "talking";
    clear(count).append(
      h("span", { class: "long" }, asking ? t("live.waiting", { n: asking }) : t("live.talking", { n: talking })),
      h("span", { class: "short" }, String(asking || talking)));
    btn.title = asking ? t("live.waiting", { n: asking }) : t("live.talking", { n: talking });
    const nodes = new Set(rows.map((r) => r.node));
    clear(drop).append(...rows.map((r) => {
      const item = h("button", { type: "button", class: "live-item" + (r.asking ? " asking" : ""), role: "menuitem" },
        h("span", { class: r.asking ? "live-mark asking" : "live-mark talking" }),
        h("span", { class: "live-main" },
          h("b", null, r.domain === "middleware" ? t("nav.house") : r.domain),
          h("small", null, (nodes.size > 1 ? r.nodeName + " · " : "") + (r.title || t("chat.untitled")))),
        h("span", { class: "live-side" }, r.asking ? t("live.asks") : t("live.talkingSince", { ago: fmtAgo(r.updated, t) })));
      item.addEventListener("click", () => open(r));
      return item;
    }));
  }

  // Every node of the network, each asked on its own: one that does not
  // answer leaves its chats out, not the others'.
  async function load() {
    const n = store.get("nodes");
    const targets = [{ id: "", name: (n && n.self && n.self.name) || "" }, ...(((n && n.members) || []).filter((m) => m.online !== false).map((m) => ({ id: m.id, name: m.name })))];
    const answers = await Promise.all(targets.map(async (x) => {
      try { return { x, w: await api.at(x.id).get("/agents/waiting") }; } catch { return { x, w: null }; }
    }));
    const next = [];
    for (const { x, w } of answers) {
      if (!w) continue;
      for (const c of w.asking || []) next.push({ ...c, asking: true, node: x.id, nodeName: x.name });
      for (const c of w.running || []) next.push({ ...c, asking: false, node: x.id, nodeName: x.name });
    }
    // Waiting first: that is what someone has to do.
    next.sort((a, b) => (b.asking - a.asking) || (new Date(b.updated) - new Date(a.updated)));
    rows = next;
    paint();
  }

  let timer = null;
  const soon = () => { clearTimeout(timer); timer = setTimeout(load, 400); };
  onEvent("agent.", (ev) => { if (/^agent\.(done|error|question|started)$/.test(ev.type)) soon(); });
  store.subscribe("nodes", soon);
  setInterval(load, 20000);
  return { el, load };
}
