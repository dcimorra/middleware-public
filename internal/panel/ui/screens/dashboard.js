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
import { open, field, confirm, toggle } from "../components/dialog.js";
import { toast, toastError } from "../components/toast.js";
import { icon } from "../lib/icons.js";
import { movesCard } from "../components/moves.js";

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

function nodeCard(node, actions, member, myVersion) {
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
  if (node.ip) meta.push(node.ip);
  if (node.latency_ms) meta.push(`${node.latency_ms} ms`);
  card.append(h("p", { class: "small muted dash-meta" }, meta.join(" · ")));
  if (member) {
    const hs = member.handshake && !member.handshake.startsWith("0001") ? member.handshake : "";
    card.append(h("p", { class: "small dash-tunnel" }, h("span", { class: "dot " + (member.online ? "ok" : "bad") }),
      hs ? t("dash.tunnelSeen", { ago: fmtAgo(hs, t) }) : t("dash.tunnelNever"),
      member.endpoint ? h("span", { class: "muted" }, " · " + member.endpoint) : h("span", { class: "muted" }, " · " + t("dash.behindNAT"))));
  }
  if (s.version && myVersion && s.version !== myVersion) card.append(notice("warn", t("dash.otherVersion", { v: s.version, mine: myVersion })));
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
  const joinBtn = h("button", { type: "button", class: "btn" }, icon("link"), t("dash.join"));
  const inviteBtn = h("button", { type: "button", class: "btn primary" }, icon("plus"), t("dash.invite"));
  const p = page({ title: t("nav.dashboard"), icon: "layout", sub: t("dash.sub"), actions: [joinBtn, inviteBtn] });
  const banner = h("div");
  const totals = h("div", { class: "dash-totals" });
  const grid = h("div", { class: "dash-grid" }, loading());
  const help = h("div", { class: "card" }, h("h2", null, t("dash.howTitle")), h("p", { class: "small muted" }, t("dash.how1")), h("p", { class: "small muted" }, t("dash.how2")), h("p", { class: "small muted" }, t("dash.how3")));
  // The network's settings: one decision for every node.
  const netCard = h("div", { class: "card", hidden: true });
  const moves = movesCard("");
  p.body.append(banner, totals, grid, moves.el, netCard, help);
  let cards = [];
  let info = null;
  let alive = true;

  function paint() {
    clear(banner);
    if (info && !info.network) {
      banner.append(notice("", h("strong", null, t("dash.alone")), " ", t("dash.aloneHint")));
    } else if (info && info.error) {
      banner.append(notice("bad", h("strong", null, t("dash.meshError")), " ", info.error));
    }
    joinBtn.hidden = Boolean(info && info.network);
    paintNet();
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
    const byID = new Map(((info && info.members) || []).map((m) => [m.id, m]));
    const mine = cards[0] && cards[0].summary && cards[0].summary.version;
    for (const c of cards) {
      const acts = [h("button", { type: "button", class: "btn sm", title: t("dash.openHint"), onClick: () => go(c, "/sites") }, icon("external"), t("dash.open"))];
      if (c.self) {
        acts.push(h("button", { type: "button", class: "btn sm", onClick: () => rename(c) }, icon("edit"), t("dash.rename")));
        if (info && info.network) acts.push(h("button", { type: "button", class: "btn sm danger", onClick: leave }, t("dash.leave")));
      } else {
        acts.push(h("button", { type: "button", class: "btn sm danger", onClick: () => expel(c) }, t("dash.expel")));
      }
      grid.append(nodeCard(c, acts, byID.get(c.id), mine));
    }
  }

  function paintNet() {
    netCard.hidden = !(info && info.network);
    if (netCard.hidden) return;
    const on = Boolean(info.settings && info.settings.panel_vpn_only);
    const sw = toggle(t("dash.panelVPNOnly"), { checked: on });
    sw.input.addEventListener("change", async () => {
      const want = sw.input.checked;
      if (want && !(await confirm({ title: t("dash.panelVPNOnlyQ"), body: t("dash.panelVPNOnlyBody"), confirmLabel: t("dash.panelVPNOnlyGo") }))) { sw.input.checked = false; return; }
      try {
        await api.put("/nodes/settings", { panel_vpn_only: want }, { node: "" });
        toast(t(want ? "dash.panelVPNOnlyOn" : "dash.panelVPNOnlyOff"), "ok");
        load();
      } catch (e) { sw.input.checked = !want; toastError(e); }
    });
    clear(netCard).append(
      h("h2", null, t("dash.netTitle")),
      h("div", { class: "field" }, sw.el, h("p", { class: "small muted" }, t("dash.panelVPNOnlyHint"))),
      h("p", { class: "small muted" }, t("dash.relayHint")));
  }

  async function load() {
    try {
      [cards, info] = await Promise.all([api.get("/nodes/overview", { node: "" }), api.get("/nodes", { node: "" })]);
      store.set("nodes", info);
      paint();
    } catch (e) {
      if (!cards.length) clear(grid).append(errorBox(e));
    }
  }

  async function rename(c) {
    const name = h("input", { type: "text", value: c.name, maxlength: "40", required: true });
    const done = await open({
      title: t("dash.renameTitle"),
      body: [field(t("dash.name"), name, t("dash.nameHint"))],
      buttons: [{ label: t("common.cancel"), value: null }, { label: t("common.save"), submit: true, primary: true }],
      onSubmit: async () => { await api.put("/nodes/self", { name: name.value }, { node: "" }); return true; },
    });
    if (done) load();
  }

  async function expel(c) {
    const ok = await confirm({ title: t("dash.expelQ", { name: c.name }), body: t("dash.expelBody"), confirmLabel: t("dash.expel"), danger: true });
    if (!ok) return;
    try {
      await api.del(`/nodes/${encodeURIComponent(c.id)}`, { node: "" });
      toast(t("dash.expelled", { name: c.name }), "ok");
      if (currentNode() === c.id) setCurrentNode("");
      load();
    } catch (e) { toastError(e); }
  }

  async function leave() {
    const ok = await confirm({ title: t("dash.leaveQ"), body: t("dash.leaveBody"), confirmLabel: t("dash.leave"), danger: true });
    if (!ok) return;
    try {
      await api.post("/nodes/leave", {}, { node: "" });
      setCurrentNode("");
      toast(t("dash.left"), "ok");
      load();
    } catch (e) { toastError(e); }
  }

  inviteBtn.addEventListener("click", async () => {
    const creating = !(info && info.network);
    const endpoint = h("input", { type: "text", value: (info && info.suggested_endpoint) || "", placeholder: "203.0.113.10:51821", autocomplete: "off", spellcheck: "false" });
    const out = h("div", { class: "stack-sm" });
    await open({
      title: t(creating ? "dash.createTitle" : "dash.inviteTitle"),
      wide: true,
      body: [
        h("p", { class: "dim" }, t(creating ? "dash.createBody" : "dash.inviteBody")),
        creating ? field(t("dash.endpoint"), endpoint, t("dash.endpointHint")) : null,
        out,
      ],
      buttons: [{ label: t("common.close"), value: null }, { label: t("dash.makeCode"), submit: true, primary: true }],
      onSubmit: async () => {
        const r = await api.post("/nodes/invite", { endpoint: creating ? endpoint.value : "" }, { node: "" });
        const box = h("textarea", { class: "mono code-box", rows: "4", readonly: true }, r.code);
        const cp = h("button", { type: "button", class: "btn sm" }, icon("copy"), t("common.copy"));
        cp.addEventListener("click", async () => { if (await copy(r.code)) toast(t("common.copied"), "ok"); });
        clear(out).append(box, h("div", { class: "row" }, cp, h("span", { class: "small muted" }, t("dash.codeExpires", { at: new Date(r.expires).toLocaleTimeString() }))));
        box.select();
        load();
        return false;
      },
    });
  });

  joinBtn.addEventListener("click", async () => {
    const code = h("textarea", { class: "mono code-box", rows: "4", placeholder: "mwmesh_…", spellcheck: "false", required: true });
    const endpoint = h("input", { type: "text", value: (info && info.suggested_endpoint) || "", placeholder: "203.0.113.11:51821", autocomplete: "off", spellcheck: "false" });
    const made = await open({
      title: t("dash.joinTitle"),
      wide: true,
      body: [h("p", { class: "dim" }, t("dash.joinBody")), field(t("dash.code"), code), field(t("dash.endpoint"), endpoint, t("dash.joinEndpointHint"))],
      buttons: [{ label: t("common.cancel"), value: null }, { label: t("dash.join"), submit: true, primary: true }],
      onSubmit: async (_f, showError) => {
        if (!code.value.trim()) { showError(t("dash.codeNeeded")); return false; }
        return api.post("/nodes/join", { code: code.value.trim(), endpoint: endpoint.value.trim() }, { node: "" });
      },
    });
    if (made) { toast(t("dash.joined", { name: made.inviter }), "ok"); load(); }
  });

  load();
  const timer = setInterval(() => { if (alive && !document.hidden) load(); }, 10000);
  return { el: p.el, destroy() { alive = false; clearInterval(timer); moves.destroy(); } };
}
