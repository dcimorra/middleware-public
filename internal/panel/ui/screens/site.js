// One site: header with status, tabs, each tab its own module. The header
// re-renders from the store so a restart pressed here shows "handing
// over" within the tick that does it, from the same event the list sees.

import { h, clear, append, shortRev, fmtTime } from "../lib/dom.js";
import { t } from "../lib/i18n.js";
import { api, apiURL, enc } from "../lib/api.js";
import { onEvent } from "../lib/stream.js";
import { store } from "../lib/store.js";
import { navigate } from "../lib/router.js";
import { page, kv, table, cell, empty, loading, errorBox, notice } from "../components/page.js";
import { icon } from "../lib/icons.js";

import { modeBadge, aliveStatus, siteMode } from "../components/badge.js";
import { tabs } from "../components/tabs.js";
import { open, confirm, field, checkbox } from "../components/dialog.js";
import { toast, toastError } from "../components/toast.js";
import { jsonEditor } from "../components/editor.js";
import { movesCard } from "../components/moves.js";
import { logsTab } from "./site-logs.js";
import { filesTab } from "./site-files.js";
import { chatTab } from "./site-chat.js";
import { accessTab } from "./site-access.js";
import { securityTab } from "./site-security.js";
import { activityTab } from "./site-activity.js";
import { cacheTab } from "./site-cache.js";
import { doorCard } from "./site-door.js";

const TABS = ["overview", "activity", "manifest", "logs", "files", "chat", "access", "security", "cache"];
const TAB_ICON = { overview: "gauge", activity: "activity", manifest: "code", logs: "list", files: "folder", chat: "chat", access: "lock", security: "shield", cache: "box" };

export function siteScreen({ domain, tab }) {
  const active = TABS.includes(tab) ? tab : "overview";
  const base = `#/sites/${enc(domain)}`;
  const statusLine = h("div", { class: "actions" });
  const p = page({
    title: domain,
    crumbs: [h("a", { href: "#/sites" }, t("nav.sites")), " / ", domain],
    actions: statusLine,
  });
  p.body.append(tabs(TABS.map((k) => ({ key: k, href: `${base}/${k}`, label: t("site.tab." + k), icon: TAB_ICON[k] })), active));
  const body = h("div");
  p.body.append(body);
  const subs = [];
  let view = null;

  function renderStatus() {
    const s = store.site(domain);
    clear(statusLine);
    if (!s) return;
    statusLine.append(modeBadge(s), aliveStatus(s));
    if (siteMode(s) !== "unmanaged") statusLine.append(h("a", { class: "btn sm", href: `https://${domain}`, target: "_blank", rel: "noopener" }, icon("external"), t("site.open")));
  }

  function mountTab() {
    if (view && view.destroy) view.destroy();
    clear(body);
    const s = store.site(domain);
    if (!s) return;
    switch (active) {
      case "manifest": view = manifestTab(domain); break;
      case "logs": view = logsTab(domain); break;
      case "files": view = filesTab(domain); break;
      case "chat": view = chatTab(domain); break;
      case "access": view = accessTab(domain); break;
      case "security": view = securityTab(domain); break;
      case "activity": view = activityTab(domain); break;
      case "cache": view = cacheTab(domain); break;
      default: view = overviewTab(domain);
    }
    body.append(view.el);
  }

  subs.push(store.subscribe("site:" + domain, renderStatus));
  (async () => {
    if (!store.site(domain)) {
      body.append(loading());
      try {
        store.putSite(await api.get(`/sites/${enc(domain)}`));
      } catch (e) {
        clear(body).append(errorBox(e));
        return;
      }
    }
    renderStatus();
    mountTab();
  })();

  return { el: p.el, destroy() { subs.forEach((u) => u()); if (view && view.destroy) view.destroy(); } };
}

// ---------- overview ----------

function overviewTab(domain) {
  const el = h("div");
  const subs = [];
  const envBox = h("div");
  // Built once: the overview re-renders on every store change, and the
  // door's form must not lose what is being typed into it.
  const door = doorCard(domain);
  // The names the house published for this site, from GET /publish:
  // [name, record] pairs, refreshed on publish.* events.
  let published = [];
  // The other nodes of this one's network, online: where the site can be
  // moved. Empty without a network, and the button is not shown.
  let otherNodes = [];
  // The site's moves, as their operations, kept across the redraws a
  // moving site causes.
  const moves = movesCard(domain);
  subs.push(() => moves.destroy());
  (async () => {
    try {
      const info = await api.get("/nodes");
      otherNodes = (info.members || []).filter((x) => x.id !== info.self.id && x.online && !x.removed);
      if (otherNodes.length) render();
    } catch { /* no network: nothing to move to */ }
  })();
  async function loadPublished() {
    try {
      const recs = await api.get("/publish");
      published = Object.entries(recs || {}).filter(([, r]) => r.site === domain).sort(([a], [b]) => a.localeCompare(b));
    } catch { published = []; }
    render();
  }
  subs.push(onEvent("publish.", () => loadPublished()));

  // The names the site answers to: its own, the www of an apex (published
  // and certified, and sent to the apex), and the extra domains, added and
  // taken away here. Each one is published like the site's own name; one
  // the zone already points elsewhere waits for a yes in the list above.
  function domainsCard(m) {
    const aliases = [...(m.aliases || [])];
    const apex = domain.split(".").length === 2;
    const list = h("div", { class: "stack-sm" });
    const save = async (next, www) => {
      const patch = { aliases: next.length ? next : null };
      if (www !== undefined) patch.www = www ? null : false;
      try {
        await api.patch(`/sites/${enc(domain)}/manifest`, patch);
        toast(t("site.domainsSaved"), "ok");
      } catch (e) { toastError(e); }
    };
    const rows = [h("div", null, h("code", null, domain), " ", h("span", { class: "badge" }, t("site.domainMain")))];
    if (apex) {
      const www = checkbox(t("site.wwwToo", { name: "www." + domain }), { checked: m.www !== false });
      www.input.addEventListener("change", () => save(aliases, www.input.checked));
      rows.push(www.el);
    }
    for (const a of aliases) {
      const x = h("button", { type: "button", class: "btn sm ghost", "aria-label": t("site.domainRemove", { name: a }) }, "×");
      x.addEventListener("click", async () => {
        if (!(await confirm({ title: t("site.domainRemoveQ", { name: a }), body: t("site.domainRemoveBody", { name: a }), confirmLabel: t("site.domainRemoveBtn"), danger: true }))) return;
        save(aliases.filter((y) => y !== a));
      });
      rows.push(h("div", { class: "row" }, h("code", { class: "grow" }, a), x));
    }
    list.append(...rows);
    const input = h("input", { type: "text", placeholder: "otro-dominio.com", autocomplete: "off", autocapitalize: "off", spellcheck: "false" });
    const add = h("button", { type: "submit", class: "btn" }, icon("plus"), t("site.domainAdd"));
    const form = h("form", { class: "row", novalidate: true }, input, add);
    form.addEventListener("submit", (e) => {
      e.preventDefault();
      const v = input.value.trim().toLowerCase().replace(/^https?:\/\//, "").replace(/\/.*$/, "");
      if (!v || v === domain || aliases.includes(v)) return;
      save([...aliases, v]);
    });
    return h("div", { class: "card" }, h("h2", null, t("site.domains")), h("p", { class: "small muted" }, t("site.domainsHint")), list, form);
  }

  // Out of its container, onto the machine as a normal process: the same
  // command in the same folder (the container mounts it at the same path),
  // and the docker section kept for going back. A command that needs Node
  // gets one on the machine, installed if it has none.
  async function setRuntime(m, runtime) {
    const out = runtime === "process";
    if (!(await confirm({ title: t("site.uncontainQ", { domain }), body: t("site.uncontainBody"), confirmLabel: t("site.uncontain") }))) return;
    try {
      await api.patch(`/sites/${enc(domain)}/manifest`, { run: { ...m.run, runtime } });
      toast(t(out ? "site.uncontained" : "site.contained"), "ok");
    } catch (e) { toastError(e); }
  }

  // Into a container, or back into the one it had: the house works it out
  // and tries it beside the one that serves before anything is written;
  // when it cannot, the agent picked here is asked in a new conversation,
  // and the chat is where this goes next. Back into a kept container goes
  // the same way, tried first, so the button never leaves a site retrying.
  async function containSite() {
    let agents = [];
    try { agents = ((await api.get("/agents")).providers || []).filter((a) => a.available); } catch { agents = []; }
    const sel = h("select", null, h("option", { value: "" }, t("site.containNoAgent")), ...agents.map((a) => h("option", { value: a.provider }, a.provider)));
    if (agents.length) sel.value = agents[0].provider;
    const go = await open({
      title: t("site.containizeQ", { domain }),
      body: [h("p", { class: "dim" }, t("site.containizeBody")), field(t("site.containAgent"), sel)],
      buttons: [{ label: t("common.cancel"), value: null }, { label: t("site.containize"), primary: true, value: "go" }],
    });
    if (go !== "go") return;
    try {
      await api.post(`/sites/${enc(domain)}/containerize`, sel.value ? { provider: sel.value } : {});
    } catch (e) { toastError(e); return; }
    const box = h("div", { class: "notice info" }, t("site.containStep.planning"));
    el.prepend(box);
    const off = onEvent("containerize.", (ev) => {
      if (ev.subject !== domain) return;
      const step = ev.type.slice("containerize.".length);
      box.textContent = t("site.containStep." + step) + (ev.message ? " — " + ev.message : "");
      if (step === "trying") return;
      off();
      box.className = "notice " + ({ done: "ok", asked: "warn" }[step] || "bad");
      if (step === "done") toast(t("site.contained"), "ok");
      if (step === "asked") {
        box.append(" ", h("a", { href: `#/sites/${enc(domain)}/chat` }, t("site.containOpenChat")));
      }
    });
    subs.push(off);
  }

  // Moving the site to another node: chosen, said (a short stop), and then
  // followed step by step from the events until it runs there.
  async function moveSite() {
    const sel = h("select", null, ...otherNodes.map((x) => h("option", { value: x.id }, x.name)));
    const node = await open({
      title: t("site.moveQ", { domain }),
      body: [h("p", { class: "dim" }, t("site.moveBody")), field(t("site.moveTo"), sel)],
      buttons: [{ label: t("common.cancel"), value: null }, { label: t("site.move"), primary: true, value: "go" }],
    });
    if (node !== "go") return;
    try {
      await api.post(`/sites/${enc(domain)}/move`, { node: sel.value });
    } catch (e) { toastError(e); return; }
    toast(t("site.moveStarted"), "ok");
    moves.reload();
  }

  // The site's redirects: an old address sent to a new one with a 3xx,
  // before the site is served. In order: the first that matches wins.
  function redirectsCard(m) {
    const rules = [...(m.redirects || [])];
    const save = async (next) => {
      try {
        await api.patch(`/sites/${enc(domain)}/manifest`, { redirects: next.length ? next : null });
        toast(t("site.redirectsSaved"), "ok");
      } catch (e) { toastError(e); }
    };
    const list = h("div", { class: "stack-sm" });
    if (!rules.length) list.append(h("p", { class: "small muted" }, t("site.redirectsNone")));
    rules.forEach((r, i) => {
      const x = h("button", { type: "button", class: "btn sm ghost", "aria-label": t("site.redirectRemove") }, "×");
      x.addEventListener("click", () => save(rules.filter((_, j) => j !== i)));
      list.append(h("div", { class: "row redirect-row" },
        h("code", { class: "grow" }, r.from), h("span", { class: "muted" }, "→"), h("code", { class: "grow" }, r.to),
        h("span", { class: "badge" }, String(r.code || 301)), x));
    });
    const from = h("input", { type: "text", placeholder: "/viejo, /blog/*, " + domain + "/*", autocomplete: "off", spellcheck: "false" });
    const to = h("input", { type: "text", placeholder: "https://nuevo.example.com/*", autocomplete: "off", spellcheck: "false" });
    const code = h("select", null, ...[301, 302, 307, 308, 303].map((c) => h("option", { value: String(c) }, t("site.redirectCode." + c))));
    const add = h("button", { type: "submit", class: "btn" }, icon("plus"), t("site.redirectAdd"));
    const form = h("form", { novalidate: true, class: "stack-sm" },
      h("div", { class: "form-row" }, field(t("site.redirectFrom"), from), field(t("site.redirectTo"), to)),
      h("div", { class: "row" }, code, add));
    form.addEventListener("submit", (e) => {
      e.preventDefault();
      const f = from.value.trim(), d = to.value.trim();
      if (!f || !d) return;
      const c = Number(code.value);
      save([...rules, c === 301 ? { from: f, to: d } : { from: f, to: d, code: c }]);
    });
    return h("div", { class: "card" }, h("h2", null, t("site.redirects")), h("p", { class: "small muted" }, t("site.redirectsHint")), list, form);
  }

  // A name the zone already points somewhere else: said, and taken only
  // with an explicit yes, because the record may be somebody's shop or mail.
  function conflictEl(name, r) {
    const take = h("button", { type: "button", class: "btn sm danger" }, t("publish.takeover"));
    take.addEventListener("click", async () => {
      if (!(await confirm({ title: t("publish.takeoverQ", { name }), body: t("publish.takeoverBody", { name, found: r.found, site: domain }), confirmLabel: t("publish.takeover"), danger: true }))) return;
      try {
        await api.post("/publish/takeover", { names: [name] });
        toast(t("publish.takenOver", { name }), "ok");
        loadPublished();
      } catch (e) { toastError(e); }
    });
    return h("div", { class: "small" }, h("span", { class: "dim" }, t("publish.conflictSays", { found: r.found })), " ", take);
  }

  function render() {
    const s = store.site(domain);
    clear(el);
    el.append(moves.el);
    if (!s) { el.append(empty(t("site.gone"), t("site.goneHint"))); return; }
    const mode = siteMode(s);
    const m = s.manifest || {};
    const unmanaged = mode === "unmanaged";

    // Actions. Each one is a verb on this site, and the destructive ones
    // confirm with the domain in the question.
    const actions = h("div", { class: "actions" });
    if (!unmanaged) {
      if (mode === "run" || mode === "upstream") actions.append(btn(t("site.restart"), "refresh", () => act("restart", `/sites/${enc(domain)}/restart`, t("site.restarted"))));
      if (m.disabled) actions.append(btn(t("site.start"), "play", () => act("start", `/sites/${enc(domain)}/start`, t("site.started")), "primary"));
      else actions.append(btn(t("site.stop"), "stop", async () => {
        if (await confirm({ title: t("site.stopQ", { domain }), body: t("site.stopBody"), confirmLabel: t("site.stop"), danger: true })) {
          await act("stop", `/sites/${enc(domain)}/stop`, t("site.stopped"));
        }
      }));
      if (m.hold && m.hold.enabled) actions.append(btn(t("site.release"), "unlock", () => hold(false)));
      else actions.append(btn(t("site.hold"), "pause", () => hold(true)));
      if (m.deploy && m.deploy.github) actions.append(btn(t("site.deploy"), "upload", () => act("deploy", `/sites/${enc(domain)}/deploy`, t("site.deployStarted"))));
    } else {
      actions.append(btn(t("site.adopt"), "plus", adopt, "primary"));
    }
    if (!unmanaged && mode !== "project") {
      actions.append(btn(t("site.publish"), "upload", publish));
      if (published.length) actions.append(btn(t("site.unpublish"), "door", unpublish));
    }
    if (!unmanaged) {
      // A plain link: the browser downloads with the session cookie, and a
      // site of gigabytes streams instead of passing through memory.
      actions.append(h("a", { class: "btn", href: apiURL(`/sites/${enc(domain)}/download.zip`), download: `${domain}.zip`, title: t("site.downloadHint") }, icon("download"), t("site.download")));
      actions.append(btn(t(mode === "project" ? "site.giveDomain" : "site.rename"), "edit", rename));
      actions.append(btn(t("site.exec"), "terminal", runCommand));
      if (otherNodes.length) actions.append(btn(t("site.move"), "upload", moveSite));
      // In a container, or out of one with its container kept to go back to.
      if (m.run && m.run.runtime === "docker") actions.append(btn(t("site.uncontain"), "box", () => setRuntime(m, "process")));
      else if (mode === "run") actions.append(btn(t(m.run && m.run.docker ? "site.contain" : "site.containize"), "box", containSite));
    }
    actions.append(btn(t("site.delete"), "trash", del, "danger"));

    const pairs = [
      [t("site.mode"), modeBadge(s)],
      [t("site.status"), aliveStatus(s)],
      [t("site.port"), s.port ? h("code", null, String(s.port)) : null],
      [t("site.revision"), s.revision ? h("code", { title: s.revision }, shortRev(s.revision)) : null],
      [t("site.error"), s.error ? h("span", { class: "level-error" }, s.error) : null],
      [t("site.missing"), (s.missing || []).length ? h("div", { class: "stack-sm" }, ...s.missing.map((x) => h("span", { class: "level-warn" }, x))) : null],
      [t("site.handing"), s.handing ? t("common.yes") : null],
      [t("site.runtime"), m.run && m.run.runtime ? m.run.runtime : null],
      [t("site.upstream"), m.upstream || null],
      [t("site.staticRoot"), m.static ? (m.static.root || "public") : null],
      [t("site.holdTitle"), m.hold && m.hold.enabled ? (m.hold.title || "—") : null],
      [t("site.picture"), mode === "project" || unmanaged ? null : sitePicture()],
      [t("site.published"), published.length ? h("div", { class: "stack-sm" }, ...published.map(([name, r]) => h("div", null,
        h("code", null, name), " ",
        h("span", { class: "badge " + ({ published: "ok", failed: "bad", manual: "warn", retiring: "warn", conflict: "bad" }[r.state] || ""), title: r.error || "" }, t("publish.state." + r.state)),
        " ", h("span", { class: "small muted" }, t("inbound.door." + r.door)),
        r.state === "conflict" ? conflictEl(name, r) : r.error ? h("div", { class: "small dim" }, r.error) : null,
      ))) : (mode === "project" ? null : h("span", { class: "muted" }, t("site.notPublished")))],
    ];

    append(el, [
      h("div", { class: "card" }, h("h2", null, t("site.tab.overview")), kv(pairs), h("hr"), actions),
      unmanaged ? notice("warn", t("site.unmanagedNotice")) : null,
      unmanaged || mode === "project" ? null : domainsCard(m),
      unmanaged || mode === "project" ? null : redirectsCard(m),
      workersCard(s),
      unmanaged ? null : door,
      unmanaged ? null : h("div", { class: "card" }, h("h2", null, t("site.env")), h("p", { class: "small muted" }, t("site.envHint")), envBox),
    ]);
  }

  function workersCard(s) {
    const m = s.manifest || {};
    const names = Object.keys(m.workers || {});
    if (names.length === 0) return null;
    const rows = names.map((name) => {
      const w = (s.workers && s.workers[name]) || {};
      const def = m.workers[name] || {};
      return h("tr", null,
        cell(h("code", null, name), null, "primary"),
        cell(w.alive ? h("span", { class: "status" }, h("span", { class: "dot ok" }), t("status.up")) : h("span", { class: "status", title: w.error || "" }, h("span", { class: "dot bad" }), t("status.down")), t("site.status")),
        cell(def.port || w.port ? String(def.port || w.port) : "", t("site.port"), "num mono"),
        cell(def.command ? h("code", null, def.command.join(" ")) : "", t("site.command"), "mono"),
        cell(w.error ? h("span", { class: "level-error small" }, w.error) : "", t("site.error")),
        cell(workerActions(name, def, w), null, "right nowrap"),
      );
    });
    return h("div", { class: "card pad0" }, h("div", { class: "log-bar card-pad" }, h("h2", null, t("site.workers"))), table([
      { label: t("site.worker") }, { label: t("site.status") }, { label: t("site.port"), class: "num" }, { label: t("site.command") }, { label: t("site.error") }, { label: "" },
    ], rows, { responsive: true }).el);
  }

  // Restart is for now; stop and start are written to the manifest
  // (workers.<name>.disabled), so they survive a restart of the house.
  function workerActions(name, def, w) {
    const box = h("div", { class: "actions" });
    const path = `/sites/${enc(domain)}/workers/${enc(name)}`;
    if (def.disabled) {
      box.append(btn(t("site.workerStart"), "play", () => act("start", path + "/start", t("site.workerStarted", { name })), "sm"));
      return box;
    }
    if (w.alive) box.append(btn(t("site.restart"), "refresh", () => act("restart", path + "/restart", t("site.workerRestarted", { name })), "sm"));
    box.append(btn(t("site.stop"), "stop", async () => {
      if (await confirm({ title: t("site.workerStopQ", { name }), body: t("site.workerStopBody"), confirmLabel: t("site.stop"), danger: true })) {
        await act("stop", path + "/stop", t("site.workerStopped", { name }));
      }
    }, "sm"));
    return box;
  }

  // One command where the site runs, answered in the same dialog. Not a
  // terminal: no input, a minute by default; anything longer is a task.
  async function runCommand() {
    const cmd = h("input", { type: "text", class: "mono", placeholder: "npm ls --depth=0", autocomplete: "off", spellcheck: "false" });
    const timeout = h("select", null, ...["30s", "1m", "5m", "10m"].map((d) => h("option", { value: d }, d)));
    timeout.value = "1m";
    const out = h("pre", { class: "log exec-out", hidden: true });
    const status = h("p", { class: "small" });
    await open({
      title: t("site.execQ", { domain }),
      wide: true,
      body: [h("p", { class: "dim small" }, t("site.execBody")), h("div", { class: "form-row" }, field(t("site.execCommand"), cmd), field(t("site.execTimeout"), timeout)), status, out],
      buttons: [{ label: t("common.close"), value: null }, { label: t("site.execRun"), submit: true, primary: true }],
      onSubmit: async () => {
        if (!cmd.value.trim()) return false;
        status.textContent = t("site.execRunning");
        out.hidden = true;
        const r = await api.post(`/sites/${enc(domain)}/exec`, { command: cmd.value, timeout: timeout.value });
        out.textContent = r.output || t("site.execNoOutput");
        out.hidden = false;
        status.textContent = (r.timed_out ? t("site.execTimedOut") : t("site.execExit", { code: r.exit_code })) +
          " · " + r.seconds.toFixed(1) + " s" + (r.truncated ? " · " + t("site.execTruncated") : "");
        status.className = "small " + (r.exit_code === 0 && !r.timed_out ? "level-ok" : "level-error");
        return false; // stay open: the answer is the point
      },
    });
  }

  async function loadEnv() {
    try {
      const r = await api.get(`/sites/${enc(domain)}/env`);
      const resolved = (r && r.resolved) || {};
      const keys = Object.keys(resolved).sort();
      clear(envBox);
      if (keys.length === 0) { envBox.append(h("p", { class: "muted small" }, t("site.envEmpty"))); return; }
      envBox.append(table([{ label: t("site.envKey") }, { label: t("site.envValue") }],
        keys.map((k) => h("tr", null, cell(h("code", null, k)), cell(h("code", null, String(resolved[k]))))), {}).el);
    } catch (e) {
      clear(envBox).append(h("p", { class: "muted small" }, e.message));
    }
  }

  function btn(label, ico, fn, cls) {
    const b = h("button", { type: "button", class: "btn " + (cls || "") }, icon(ico), label);
    b.addEventListener("click", async () => {
      b.classList.add("busy");
      try { await fn(); } catch (e) { toastError(e); } finally { b.classList.remove("busy"); }
    });
    return b;
  }

  async function act(_what, path, done) {
    const r = await api.post(path);
    if (r && r.domain) store.putSite(r);
    toast(done, "ok");
  }

  async function hold(enabled) {
    const s = store.site(domain);
    const cur = (s.manifest && s.manifest.hold) || {};
    if (!enabled) {
      const r = await api.post(`/sites/${enc(domain)}/hold`, { enabled: false });
      if (r && r.domain) store.putSite(r);
      toast(t("site.released"), "ok");
      return;
    }
    const title = h("input", { type: "text", value: cur.title || "" });
    const message = h("textarea", { value: cur.message || "" });
    const mode = h("select", null, ...["maintenance", "404", "410", "500"].map((k) => h("option", { value: k }, t("site.holdMode." + k))));
    mode.value = cur.mode || "maintenance";
    // A local time in the picker, sent as RFC 3339: the page shows it and
    // it goes out as Retry-After.
    const until = h("input", { type: "datetime-local", value: cur.until ? new Date(cur.until).toISOString().slice(0, 16) : "" });
    const ok = await open({
      title: t("site.holdQ", { domain }),
      body: [h("p", { class: "dim" }, t("site.holdBody")), field(t("site.holdModeLabel"), mode), field(t("site.holdTitle"), title), field(t("site.holdMessage"), message), field(t("site.holdUntil"), until, t("site.holdUntilHint"))],
      buttons: [{ label: t("common.cancel"), value: null }, { label: t("site.hold"), submit: true, primary: true }],
      onSubmit: async () => {
        const body = { enabled: true, title: title.value, message: message.value, mode: mode.value === "maintenance" ? "" : mode.value };
        if (until.value) body.until = new Date(until.value).toISOString().replace(/\.\d{3}Z$/, "Z");
        const r = await api.post(`/sites/${enc(domain)}/hold`, body);
        if (r && r.domain) store.putSite(r);
      },
    });
    if (ok) toast(t("site.heldNow"), "ok");
  }

  // The site as a visitor sees it, taken through a door that skips its
  // password; a picture that is not there yet just does not show.
  function sitePicture() {
    const img = h("img", { class: "site-shot", alt: t("site.pictureAlt", { domain }), src: apiURL(`/sites/${enc(domain)}/shot.png?t=${Math.floor(Date.now() / 60000)}`), loading: "lazy" });
    const box = h("div", { class: "stack-sm" }, img);
    img.addEventListener("error", () => { img.hidden = true; });
    const again = h("button", { type: "button", class: "btn sm ghost" }, icon("refresh"), t("site.pictureAgain"));
    again.addEventListener("click", async () => {
      try { await api.post(`/sites/${enc(domain)}/shot`); toast(t("site.pictureTaking"), "ok"); } catch (e) { toastError(e); }
    });
    box.append(again);
    return box;
  }
  subs.push(onEvent("shot.taken", (ev) => {
    if (ev.subject !== domain) return;
    const img = el.querySelector("img.site-shot");
    if (img) { img.hidden = false; img.src = apiURL(`/sites/${enc(domain)}/shot.png?t=${Date.now()}`); }
  }));

  async function publish() {
    try {
      const r = await api.post(`/sites/${enc(domain)}/publish`, {});
      const failed = (r.lines || []).filter((l) => l.status === "fail");
      toast(failed.length ? failed.map((l) => l.text).join("; ") : t("site.publishedNow"), failed.length ? "bad" : "ok");
      // Published with known critical vulnerabilities: said at the moment
      // the site starts being reachable.
      if (r.vulnerable) toast(r.vulnerable, "bad");
    } catch (e) { toastError(e); }
    loadPublished();
  }

  async function unpublish() {
    if (!(await confirm({ title: t("site.unpublishQ", { domain }), body: t("site.unpublishBody"), confirmLabel: t("site.unpublish"), danger: true }))) return;
    try {
      await api.post(`/sites/${enc(domain)}/unpublish`, {});
      toast(t("site.unpublishedNow"), "ok");
    } catch (e) { toastError(e); }
    loadPublished();
  }

  async function rename() {
    // A project (no dot) is given a domain the same way, and published on
    // it unless the box is cleared.
    const project = !domain.includes(".");
    const to = h("input", { type: "text", value: project ? "" : domain, placeholder: project ? "example.com" : "", autocomplete: "off", autocapitalize: "off", spellcheck: "false" });
    const pub = checkbox(t("site.renamePublish"), { checked: true });
    const made = await open({
      title: t(project ? "site.giveDomainQ" : "site.renameQ", { domain }),
      body: [h("p", { class: "dim" }, t(project ? "site.giveDomainBody" : "site.renameBody")), field(t(project ? "site.giveDomainTo" : "site.renameTo"), to), project ? pub.el : null],
      buttons: [{ label: t("common.cancel"), value: null }, { label: t(project ? "site.giveDomain" : "site.rename"), submit: true, primary: true }],
      onSubmit: async () => {
        const r = await api.post(`/sites/${enc(domain)}/rename`, { to: to.value.trim().toLowerCase(), publish: project && pub.input.checked });
        store.dropSite(domain);
        if (r && r.domain) store.putSite(r);
        return r.domain;
      },
    });
    if (made) {
      toast(t("site.renamed", { domain: made }), "ok");
      navigate(`/sites/${encodeURIComponent(made)}`);
    }
  }

  async function del() {
    const archive = checkbox(t("site.archive"), { checked: true });
    const yes = await confirm({
      title: t("site.deleteQ", { domain }),
      body: t("site.deleteBody"),
      confirmLabel: t("site.delete"),
      danger: true,
      extra: h("div", { class: "field" }, archive.el, h("div", { class: "hint" }, t("site.archiveHint"))),
    });
    if (!yes) return;
    try {
      await api.del(`/sites/${enc(domain)}` + (archive.input.checked ? "?archive=true" : ""));
      store.dropSite(domain);
      toast(archive.input.checked ? t("site.archived", { domain }) : t("site.deleted", { domain }), "ok");
      navigate("/sites");
    } catch (e) { toastError(e); }
  }

  async function adopt() {
    const template = h("select", null, ...["static", "node", "go", "docker"].map((k) => h("option", { value: k }, t("template." + k))));
    const made = await open({
      title: t("site.adoptQ", { domain }),
      body: [h("p", { class: "dim" }, t("site.adoptBody")), field(t("sites.template"), template)],
      buttons: [{ label: t("common.cancel"), value: null }, { label: t("site.adopt"), submit: true, primary: true }],
      onSubmit: async () => {
        const site = await api.post("/sites", { domain, template: template.value });
        store.putSite(site);
      },
    });
    if (made) toast(t("site.adopted", { domain }), "ok");
  }

  subs.push(store.subscribe("site:" + domain, render));
  render();
  if (siteMode(store.site(domain) || {}) !== "unmanaged") { loadEnv(); loadPublished(); }
  return { el, destroy() { subs.forEach((u) => u()); } };
}

// ---------- manifest ----------

function manifestTab(domain) {
  const s = store.site(domain);
  const el = h("div", { class: "card" });
  const editor = jsonEditor({
    value: s.manifest || { version: 2 },
    onSave: async (value) => {
      const updated = await api.put(`/sites/${enc(domain)}/manifest`, value);
      if (updated && updated.domain) {
        loaded = JSON.stringify(updated.manifest || {}); // our own save is not a change underneath
        store.putSite(updated);
      }
      toast(t("manifest.saved"), "ok");
    },
  });
  const note = h("p", { class: "small muted" }, t("manifest.hint"));
  const stamp = h("p", { class: "small muted" });
  el.append(h("h2", null, t("site.tab.manifest")), note, editor.el, stamp);
  // A manifest edited by an agent or by hand while this tab is open shows
  // a note instead of silently replacing what the operator is typing. The
  // site object refreshes on every instance event, so only a manifest
  // that really differs from the one loaded counts as changed.
  let loaded = JSON.stringify(s.manifest || {});
  const unsub = store.subscribe("site:" + domain, () => {
    const site = store.site(domain);
    if (!site) return;
    const now = JSON.stringify(site.manifest || {});
    if (now === loaded) return;
    loaded = now;
    if (!editor.dirty) { editor.setValue(site.manifest || {}); stamp.textContent = ""; return; }
    stamp.textContent = t("manifest.changedUnderneath", { at: fmtTime(new Date().toISOString()) });
  });
  el.append(historyCard(domain, editor));
  return { el, destroy: unsub };
}

// The versions the engine kept, whoever wrote them. Looking at one puts
// nothing in the editor; restoring writes it back through the same
// validation as a save, and becomes the newest version itself.
function historyCard(domain, editor) {
  const box = h("div");
  const wrap = h("div", null, h("hr"), h("h3", null, t("manifest.history")), h("p", { class: "small muted" }, t("manifest.historyHint")), box);
  async function load() {
    try {
      const versions = await api.get(`/sites/${enc(domain)}/manifest/history`);
      clear(box);
      if (!versions.length) { box.append(h("p", { class: "small muted" }, t("manifest.historyNone"))); return; }
      box.append(table([{ label: t("manifest.historyAt") }, { label: t("manifest.historySize"), class: "num" }, { label: "" }],
        versions.map((v, i) => h("tr", null,
          cell(h("span", { title: fmtTime(v.at) }, fmtTime(v.at)), t("manifest.historyAt")),
          cell(String(v.bytes) + " B", t("manifest.historySize"), "num"),
          cell(i === 0 ? h("span", { class: "badge ok" }, t("manifest.historyCurrent")) : versionActions(v), null, "right nowrap"),
        )), { responsive: true }).el);
    } catch (e) { clear(box).append(errorBox(e)); }
  }
  function versionActions(v) {
    const view = h("button", { type: "button", class: "btn sm" }, icon("eye"), t("manifest.historyView"));
    view.addEventListener("click", async () => {
      try {
        const text = await api.text(`/sites/${enc(domain)}/manifest/history/${enc(v.id)}`);
        const restore = await open({
          title: t("manifest.historyOf", { at: fmtTime(v.at) }), wide: true,
          body: h("pre", { class: "log" }, text),
          buttons: [{ label: t("common.close"), value: false }, { label: t("manifest.historyRestore"), value: true, primary: true }],
        });
        if (restore) await doRestore(v);
      } catch (e) { toastError(e); }
    });
    const back = h("button", { type: "button", class: "btn sm" }, icon("history"), t("manifest.historyRestore"));
    back.addEventListener("click", () => doRestore(v));
    return h("div", { class: "actions" }, view, back);
  }
  async function doRestore(v) {
    if (!(await confirm({ title: t("manifest.historyRestoreQ", { at: fmtTime(v.at) }), body: t("manifest.historyRestoreBody"), confirmLabel: t("manifest.historyRestore") }))) return;
    try {
      const updated = await api.post(`/sites/${enc(domain)}/manifest/history/${enc(v.id)}/restore`);
      if (updated && updated.domain) { store.putSite(updated); editor.setValue(updated.manifest || {}); }
      toast(t("manifest.historyRestored"), "ok");
      setTimeout(load, 3500); // the engine keeps it on its next tick
    } catch (e) { toastError(e); }
  }
  load();
  return wrap;
}
