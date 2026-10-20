// One site: header with status, tabs, each tab its own module. The header
// re-renders from the store so a restart pressed here shows "handing
// over" within the tick that does it, from the same event the list sees.

import { h, clear, append, shortRev, fmtTime } from "../lib/dom.js";
import { t } from "../lib/i18n.js";
import { api, enc } from "../lib/api.js";
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
import { logsTab } from "./site-logs.js";
import { filesTab } from "./site-files.js";
import { chatTab } from "./site-chat.js";
import { accessTab } from "./site-access.js";

const TABS = ["overview", "manifest", "logs", "files", "chat", "access"];
const TAB_ICON = { overview: "gauge", manifest: "code", logs: "list", files: "folder", chat: "chat", access: "lock" };

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
  // The names the house published for this site, from GET /publish:
  // [name, record] pairs, refreshed on publish.* events.
  let published = [];
  async function loadPublished() {
    try {
      const recs = await api.get("/publish");
      published = Object.entries(recs || {}).filter(([, r]) => r.site === domain).sort(([a], [b]) => a.localeCompare(b));
    } catch { published = []; }
    render();
  }
  subs.push(onEvent("publish.", () => loadPublished()));

  function render() {
    const s = store.site(domain);
    clear(el);
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
    actions.append(btn(t("site.delete"), "trash", del, "danger"));

    const pairs = [
      [t("site.mode"), modeBadge(s)],
      [t("site.status"), aliveStatus(s)],
      [t("site.port"), s.port ? h("code", null, String(s.port)) : null],
      [t("site.revision"), s.revision ? h("code", { title: s.revision }, shortRev(s.revision)) : null],
      [t("site.error"), s.error ? h("span", { class: "level-error" }, s.error) : null],
      [t("site.handing"), s.handing ? t("common.yes") : null],
      [t("site.runtime"), m.run && m.run.runtime ? m.run.runtime : null],
      [t("site.upstream"), m.upstream || null],
      [t("site.staticRoot"), m.static ? (m.static.root || "public") : null],
      [t("site.holdTitle"), m.hold && m.hold.enabled ? (m.hold.title || "—") : null],
      [t("site.app"), m.app || null],
      [t("site.published"), published.length ? h("div", { class: "stack-sm" }, ...published.map(([name, r]) => h("div", null,
        h("code", null, name), " ",
        h("span", { class: "badge " + ({ published: "ok", failed: "bad", manual: "warn", retiring: "warn" }[r.state] || ""), title: r.error || "" }, t("publish.state." + r.state)),
        " ", h("span", { class: "small muted" }, t("inbound.door." + r.door)),
        r.error ? h("div", { class: "small dim" }, r.error) : null,
      ))) : (mode === "project" ? null : h("span", { class: "muted" }, t("site.notPublished")))],
    ];

    append(el, [
      h("div", { class: "card" }, h("h2", null, t("site.tab.overview")), kv(pairs), h("hr"), actions),
      unmanaged ? notice("warn", t("site.unmanagedNotice")) : null,
      workersCard(s),
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
      );
    });
    return h("div", { class: "card pad0" }, h("div", { class: "log-bar card-pad" }, h("h2", null, t("site.workers"))), table([
      { label: t("site.worker") }, { label: t("site.status") }, { label: t("site.port"), class: "num" }, { label: t("site.command") }, { label: t("site.error") },
    ], rows, { responsive: true }).el);
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
    const ok = await open({
      title: t("site.holdQ", { domain }),
      body: [h("p", { class: "dim" }, t("site.holdBody")), field(t("site.holdTitle"), title), field(t("site.holdMessage"), message)],
      buttons: [{ label: t("common.cancel"), value: null }, { label: t("site.hold"), submit: true, primary: true }],
      onSubmit: async () => {
        const r = await api.post(`/sites/${enc(domain)}/hold`, { enabled: true, title: title.value, message: message.value });
        if (r && r.domain) store.putSite(r);
      },
    });
    if (ok) toast(t("site.heldNow"), "ok");
  }

  async function publish() {
    try {
      const r = await api.post(`/sites/${enc(domain)}/publish`, {});
      const failed = (r.lines || []).filter((l) => l.status === "fail");
      toast(failed.length ? failed.map((l) => l.text).join("; ") : t("site.publishedNow"), failed.length ? "bad" : "ok");
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
  return { el, destroy: unsub };
}
