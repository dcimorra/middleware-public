// Every node at a glance: whether it answers, how its machine is doing,
// which of its sites are in trouble, which conversations are at work or
// waiting, and what went wrong lately. One request to this node, which
// asks the others in parallel; read every ten seconds while open. The
// links between nodes are made and undone here too.

import { h, clear, svg, fmtBytes, fmtAgo, copy } from "../lib/dom.js";
import { t } from "../lib/i18n.js";
import { api } from "../lib/api.js";
import { store } from "../lib/store.js";
import { navigate } from "../lib/router.js";
import { currentNode, setCurrentNode } from "../lib/node.js";
import { page, notice, loading, errorBox } from "../components/page.js";
import { open, field, confirm } from "../components/dialog.js";
import { toast, toastError } from "../components/toast.js";
import { icon } from "../lib/icons.js";

const pct = (v) => `${Math.round(Number(v || 0))}%`;

// go opens something on a node: the panel moves to that machine first.
function go(node, path) {
  setCurrentNode(node.self ? "" : node.id);
  navigate(path);
}

// spark draws the hour per minute: processor and memory, 0 to 100.
function spark(minutes) {
  const W = 300, H = 48, n = minutes.length;
  const s = svg("svg", { class: "dash-spark", viewBox: `0 0 ${W} ${H}`, preserveAspectRatio: "none", role: "img", "aria-label": t("dash.sparkLabel") });
  if (n < 2) return s;
  const line = (k, cls) => {
    let d = "";
    minutes.forEach((m, i) => { d += (i ? "L" : "M") + ((i / (n - 1)) * W).toFixed(1) + " " + (H - (Math.min(100, Math.max(0, m[k])) / 100) * (H - 2) - 1).toFixed(1) + " "; });
    return svg("path", { class: cls, d });
  };
  s.append(line(2, "mem"), line(1, "cpu"));
  return s;
}

function meter(label, value, extra) {
  const v = Math.min(100, Math.max(0, Number(value || 0)));
  const level = v >= 90 ? "bad" : v >= 75 ? "warn" : "ok";
  const bar = h("div", { class: "dash-bar-fill " + level });
  bar.style.width = v.toFixed(0) + "%";
  return h("div", { class: "dash-meter" },
    h("div", { class: "dash-meter-head" }, h("span", null, label), h("strong", { class: "mono" }, pct(v))),
    h("div", { class: "dash-bar", role: "meter", "aria-valuenow": v.toFixed(0), "aria-valuemin": "0", "aria-valuemax": "100", "aria-label": label }, bar),
    extra ? h("div", { class: "small muted" }, extra) : null);
}

function stat(value, label, kind) {
  return h("div", { class: "dash-stat " + (kind || "") }, h("strong", null, String(value)), h("span", null, label));
}

function nodeCard(node, actions) {
  const s = node.summary || {};
  const head = h("div", { class: "dash-node-head" },
    h("span", { class: "dot " + (node.ok ? "ok" : "bad") }),
    h("h2", null, node.name),
    node.self ? h("span", { class: "badge info" }, t("dash.thisNode")) : null,
    (!node.self && currentNode() === node.id) || (node.self && !currentNode()) ? h("span", { class: "badge accent" }, t("dash.viewing")) : null,
    h("div", { class: "grow" }),
    ...actions);
  const card = h("article", { class: "card dash-node" + (node.ok ? "" : " down") }, head);
  const meta = [];
  if (s.hostname) meta.push(s.hostname);
  if (s.version) meta.push(s.version);
  if (s.started) meta.push(t("dash.up", { ago: fmtAgo(s.started, t) }));
  if (node.latency_ms) meta.push(`${node.latency_ms} ms`);
  if (node.url) meta.push(node.url);
  card.append(h("p", { class: "small muted dash-meta" }, meta.join(" · ")));
  if (!node.ok) {
    card.append(notice("bad", h("strong", null, t("dash.unreachable")), " ", h("span", { class: "small" }, node.error || "")));
    return card;
  }
  const v = s.vitals;
  if (v && v.now) {
    const n = v.now;
    card.append(h("div", { class: "dash-meters" },
      meter(t("vitals.cpu"), n.cpu, t("dash.cores", { n: n.cores || 0, load: Number(n.load1 || 0).toFixed(2) })),
      meter(t("vitals.memory"), n.mem_pct, `${fmtBytes(n.mem_used)} / ${fmtBytes(n.mem_total)}`),
      meter(t("vitals.disk"), n.disk_pct, `${fmtBytes(n.disk_used)} / ${fmtBytes(n.disk_total)}`)));
    if ((v.minutes || []).length > 1) {
      card.append(h("div", { class: "dash-spark-wrap" }, spark(v.minutes),
        h("div", { class: "small muted dash-legend" }, h("span", { class: "cpu" }, t("vitals.cpu")), h("span", { class: "mem" }, t("vitals.memory")), h("span", null, t("dash.lastHour")))));
    }
    if (v.open_episode) {
      const e = v.open_episode;
      const who = (e.blamed || []).map((b) => b.site || b.cmd).join(", ");
      card.append(notice("warn", h("strong", null, t("dash.strain", { reason: t("vitals.reason." + e.reason) })), who ? " " + t("dash.strainWho", { who }) : ""));
    }
  }
  const sc = s.sites;
  if (sc) {
    const chats = s.chats || { asking: [], working: 0 };
    card.append(h("div", { class: "dash-stats" },
      stat(sc.serving, t("dash.serving"), "ok"),
      stat(sc.trouble.length, t("dash.trouble"), sc.trouble.length ? "bad" : ""),
      stat(sc.stopped, t("dash.stopped")),
      stat(sc.projects, t("dash.projects")),
      stat(chats.working || 0, t("dash.working"), chats.working ? "accent" : ""),
      stat((chats.asking || []).length, t("dash.asking"), (chats.asking || []).length ? "warn" : "")));
    if (sc.trouble.length) {
      card.append(h("ul", { class: "dash-list" }, ...sc.trouble.map((x) => h("li", null,
        h("span", { class: "dot bad" }),
        h("a", { href: "#", onClick: (e) => { e.preventDefault(); go(node, `/sites/${encodeURIComponent(x.domain)}`); } }, x.domain),
        h("span", { class: "small muted" }, t("dash.why." + x.why))))));
    }
    if ((chats.asking || []).length) {
      card.append(h("ul", { class: "dash-list" }, ...chats.asking.map((c) => h("li", null,
        icon("chat", { size: 14 }),
        h("a", { href: "#", onClick: (e) => { e.preventDefault(); go(node, c.domain && c.domain !== "middleware" ? `/sites/${encodeURIComponent(c.domain)}/chat` : "/house"); } }, c.name || c.domain),
        h("span", { class: "small muted" }, t("dash.waitsForYou"))))));
    }
  }
  const up = s.update;
  if (up && up.available) card.append(notice("", t("dash.update", { v: up.latest })));
  const evs = (s.events || []).slice(0, 4);
  if (evs.length) {
    card.append(h("details", { class: "dash-events" },
      h("summary", { class: "small" }, t("dash.recentTrouble", { n: (s.events || []).length })),
      h("ul", { class: "dash-list small" }, ...evs.map((e) => h("li", null,
        h("span", { class: "dot " + (e.level === "error" ? "bad" : "warn") }),
        h("span", { class: "grow" }, e.subject ? h("strong", null, e.subject + " ") : null, e.message || e.type),
        h("span", { class: "muted", title: e.time }, fmtAgo(e.time, t)))))));
  }
  return card;
}

export function dashboardScreen() {
  const linkBtn = h("button", { type: "button", class: "btn" }, icon("link"), t("dash.link"));
  const inviteBtn = h("button", { type: "button", class: "btn primary" }, icon("plus"), t("dash.invite"));
  const p = page({ title: t("nav.dashboard"), icon: "layout", sub: t("dash.sub"), actions: [linkBtn, inviteBtn] });
  const totals = h("div", { class: "dash-totals" });
  const grid = h("div", { class: "dash-grid" }, loading());
  const help = h("div", { class: "card" }, h("h2", null, t("dash.howTitle")), h("p", { class: "small muted" }, t("dash.how1")), h("p", { class: "small muted" }, t("dash.how2")));
  p.body.append(totals, grid, help);
  let cards = [];
  let alive = true;

  function paint() {
    clear(totals);
    const ok = cards.filter((c) => c.ok);
    const sum = (f) => ok.reduce((n, c) => n + (f(c.summary || {}) || 0), 0);
    totals.append(
      stat(`${ok.length}/${cards.length}`, t("dash.nodesUp"), ok.length < cards.length ? "bad" : "ok"),
      stat(sum((s) => s.sites && s.sites.sites), t("dash.sites")),
      stat(sum((s) => s.sites && s.sites.trouble.length), t("dash.trouble"), sum((s) => s.sites && s.sites.trouble.length) ? "bad" : ""),
      stat(sum((s) => s.chats && s.chats.working), t("dash.working")),
      stat(sum((s) => s.chats && (s.chats.asking || []).length), t("dash.asking"), sum((s) => s.chats && (s.chats.asking || []).length) ? "warn" : ""));
    clear(grid);
    for (const c of cards) {
      const acts = [h("button", { type: "button", class: "btn sm", title: t("dash.openHint"), onClick: () => go(c, "/sites") }, icon("external"), t("dash.open"))];
      if (c.self) acts.push(h("button", { type: "button", class: "btn sm", onClick: () => rename(c) }, icon("edit"), t("dash.rename")));
      else acts.push(h("button", { type: "button", class: "btn sm danger", onClick: () => unlink(c) }, t("dash.unlink")));
      grid.append(nodeCard(c, acts));
    }
  }

  async function load() {
    try {
      cards = await api.get("/nodes/overview", { node: "" });
      paint();
    } catch (e) {
      if (!cards.length) clear(grid).append(errorBox(e));
    }
  }

  async function refreshNodes() {
    try { store.set("nodes", await api.get("/nodes", { node: "" })); } catch { /* the selector keeps what it had */ }
    await load();
  }

  async function rename(c) {
    const name = h("input", { type: "text", value: c.name, maxlength: "40", required: true });
    const done = await open({
      title: t("dash.renameTitle"),
      body: [field(t("dash.name"), name, t("dash.nameHint"))],
      buttons: [{ label: t("common.cancel"), value: null }, { label: t("common.save"), submit: true, primary: true }],
      onSubmit: async () => { await api.put("/nodes/self", { name: name.value }, { node: "" }); return true; },
    });
    if (done) refreshNodes();
  }

  async function unlink(c) {
    const ok = await confirm({ title: t("dash.unlinkQ", { name: c.name }), body: t("dash.unlinkBody"), confirmLabel: t("dash.unlink"), danger: true });
    if (!ok) return;
    try {
      const r = await api.del(`/nodes/${encodeURIComponent(c.id)}`, { node: "" });
      toast(r.remote_error ? t("dash.unlinkedLocal", { error: r.remote_error }) : t("dash.unlinked", { name: c.name }), r.remote_error ? "warn" : "ok");
      if (currentNode() === c.id) setCurrentNode("");
      refreshNodes();
    } catch (e) { toastError(e); }
  }

  inviteBtn.addEventListener("click", async () => {
    const url = h("input", { type: "url", value: location.origin, autocomplete: "off", spellcheck: "false" });
    const out = h("div", { class: "stack-sm" });
    await open({
      title: t("dash.inviteTitle"),
      wide: true,
      body: [h("p", { class: "dim" }, t("dash.inviteBody")), field(t("dash.selfURL"), url, t("dash.selfURLHint")), out],
      buttons: [{ label: t("common.close"), value: null }, { label: t("dash.makeCode"), submit: true, primary: true }],
      onSubmit: async () => {
        const r = await api.post("/nodes/invite", { url: url.value }, { node: "" });
        const box = h("textarea", { class: "mono code-box", rows: "4", readonly: true }, r.code);
        const cp = h("button", { type: "button", class: "btn sm" }, icon("copy"), t("common.copy"));
        cp.addEventListener("click", async () => { if (await copy(r.code)) toast(t("common.copied"), "ok"); });
        clear(out).append(box, h("div", { class: "row" }, cp, h("span", { class: "small muted" }, t("dash.codeExpires", { at: new Date(r.expires).toLocaleTimeString() }))));
        box.select();
        return false;
      },
    });
  });

  linkBtn.addEventListener("click", async () => {
    const code = h("textarea", { class: "mono code-box", rows: "4", placeholder: "mwnode_…", spellcheck: "false", required: true });
    const url = h("input", { type: "url", value: location.origin, autocomplete: "off", spellcheck: "false" });
    const made = await open({
      title: t("dash.linkTitle"),
      wide: true,
      body: [h("p", { class: "dim" }, t("dash.linkBody")), field(t("dash.code"), code), field(t("dash.selfURL"), url, t("dash.selfURLHint"))],
      buttons: [{ label: t("common.cancel"), value: null }, { label: t("dash.link"), submit: true, primary: true }],
      onSubmit: async (_f, showError) => {
        if (!code.value.trim()) { showError(t("dash.codeNeeded")); return false; }
        return api.post("/nodes", { code: code.value.trim(), url: url.value }, { node: "" });
      },
    });
    if (made) { toast(t("dash.linked", { name: made.name }), "ok"); refreshNodes(); }
  });

  load();
  const timer = setInterval(() => { if (alive && !document.hidden) load(); }, 10000);
  return { el: p.el, destroy() { alive = false; clearInterval(timer); } };
}
