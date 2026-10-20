// Home: every site on the machine, live. Rows are re-rendered from the
// store whenever the stream touches a site, so a handover shows as it
// happens; the search box filters in memory because the list is already
// here and a round trip would only add a delay.

import { h, clear, shortRev, fmtAgo, debounce } from "../lib/dom.js";
import { t } from "../lib/i18n.js";
import { api } from "../lib/api.js";
import { store } from "../lib/store.js";
import { navigate } from "../lib/router.js";
import { page, empty, errorBox, loading, table, cell, search as searchBox } from "../components/page.js";
import { icon } from "../lib/icons.js";

import { modeBadge, aliveStatus, siteMode } from "../components/badge.js";
import { open, field, checkbox } from "../components/dialog.js";
import { toast, toastError } from "../components/toast.js";

export function sitesScreen({ query }) {
  const search = h("input", { type: "search", placeholder: t("sites.search"), "aria-label": t("sites.search"), autocomplete: "off" });
  const newBtn = h("button", { type: "button", class: "btn primary", "data-shortcut": "new" }, icon("plus"), t("sites.new"));
  const archivedBtn = h("button", { type: "button", class: "btn" }, icon("download"), t("sites.archived"));
  const p = page({ title: t("nav.sites"), icon: "globe", actions: [searchBox(search), archivedBtn, newBtn] });
  const container = h("div", { class: "card pad0" });
  const count = h("p", { class: "small muted", role: "status" });
  p.body.append(container, count);

  let filter = "";
  const subs = [];

  function render() {
    const all = store.siteList();
    if (!store.get("sitesLoaded")) { clear(container).append(loading()); return; }
    const q = filter.trim().toLowerCase();
    const groupOf = (s) => (s.manifest && s.manifest.group) || "";
    const list = (q ? all.filter((s) => s.domain.toLowerCase().includes(q) || siteMode(s).includes(q) || groupOf(s).toLowerCase().includes(q)) : all)
      // Grouped sites first, by group, then the rest; by domain inside.
      .slice().sort((a, b) => (groupOf(a) === "") - (groupOf(b) === "") || groupOf(a).localeCompare(groupOf(b)) || a.domain.localeCompare(b.domain));
    clear(container);
    if (all.length === 0) {
      container.append(empty(t("sites.emptyTitle"), t("sites.emptyHint"), h("button", { type: "button", class: "btn primary", onClick: newSite }, icon("plus"), t("sites.new")), "globe"));
      count.textContent = "";
      return;
    }
    if (list.length === 0) {
      container.append(empty(t("sites.noMatch"), t("sites.noMatchHint", { q: filter }), null, "search"));
      count.textContent = "";
      return;
    }
    const rows = [];
    let current = null;
    const grouped = list.some((s) => groupOf(s));
    for (const s of list) {
      if (grouped && groupOf(s) !== current) {
        current = groupOf(s);
        rows.push(h("tr", { class: "group-row" }, h("th", { colspan: "6", scope: "colgroup" }, current || t("sites.ungrouped"))));
      }
      rows.push(row(s));
    }
    container.append(table([
      { label: t("sites.col.domain") },
      { label: t("sites.col.mode") },
      { label: t("sites.col.status") },
      { label: t("sites.col.port"), class: "num" },
      { label: t("sites.col.revision") },
      { label: t("sites.col.lastEvent") },
    ], rows, { responsive: true }).el);
    count.textContent = t("sites.count", { n: list.length, total: all.length });
  }

  function row(s) {
    const href = `#/sites/${encodeURIComponent(s.domain)}`;
    const last = store.lastEventFor(s.domain);
    const tr = h("tr", { class: "row-link", tabindex: "-1" },
      cell(h("a", { class: "cell-link", href }, s.domain), null, "primary"),
      cell(modeBadge(s), t("sites.col.mode")),
      cell(aliveStatus(s), t("sites.col.status")),
      cell(s.port ? String(s.port) : "", t("sites.col.port"), "num mono"),
      cell(shortRev(s.revision), t("sites.col.revision"), "mono"),
      cell(last ? h("span", { class: "small dim", title: last.time }, h("span", { class: "mono" }, last.type), " ", last.message || "", " · ", fmtAgo(last.time, t)) : h("span", { class: "small muted" }, "—"), t("sites.col.lastEvent")),
    );
    tr.addEventListener("click", (e) => {
      if (e.target.closest("a")) return;
      navigate(`/sites/${encodeURIComponent(s.domain)}`);
    });
    return tr;
  }

  async function newSite() {
    const domain = h("input", { type: "text", placeholder: "example.com", autocomplete: "off", autocapitalize: "off", spellcheck: "false", required: true });
    const template = h("select", null,
      ...["static", "node", "go", "docker", "project"].map((k) => h("option", { value: k }, t("template." + k))),
    );
    // Publishing is on by default: a site is something people reach. A
    // name without a dot is a project and has nothing to publish.
    const publish = checkbox(t("sites.publish"), { checked: true });
    const isProject = () => !domain.value.includes(".");
    const syncPublish = () => {
      publish.input.disabled = isProject();
      if (isProject()) template.value = "project";
      else if (template.value === "project") template.value = "static";
    };
    domain.addEventListener("input", syncPublish);
    let published = false;
    const made = await open({
      title: t("sites.newTitle"),
      body: [
        field(t("sites.domain"), domain, t("sites.domainHint")),
        field(t("sites.template"), template, t("sites.templateHint")),
        h("div", { class: "field" }, publish.el, h("div", { class: "hint" }, t("sites.publishHint"))),
      ],
      buttons: [{ label: t("common.cancel"), value: null }, { label: t("common.create"), submit: true, primary: true }],
      onSubmit: async (_form, showError) => {
        const d = domain.value.trim().toLowerCase();
        if (!/^[a-z0-9-]+(\.[a-z0-9-]+)*$/.test(d)) { showError(t("sites.badDomain")); return false; }
        const body = { domain: d, template: template.value };
        if (!isProject()) body.publish = publish.input.checked;
        const site = await api.post("/sites", body);
        published = Boolean(site.publishing);
        store.putSite(site);
        return site.domain;
      },
    });
    if (made) {
      toast(t(published ? "sites.createdPublishing" : "sites.created", { domain: made }), "ok");
      navigate(`/sites/${encodeURIComponent(made)}`);
    }
  }

  newBtn.addEventListener("click", newSite);
  // Archived sites: one file each under data/archive, faithful (data and
  // dependencies included), restored to sites/ as they were.
  archivedBtn.addEventListener("click", async () => {
    const list = h("div", { class: "stack-sm" });
    const fill = async () => {
      clear(list);
      const items = await api.get("/archive");
      if (!items.length) { list.append(h("p", { class: "muted" }, t("sites.noArchived"))); return; }
      for (const a of items) {
        const restore = h("button", { type: "button", class: "btn sm" }, t("sites.restore"));
        restore.addEventListener("click", async () => {
          restore.classList.add("busy");
          try { const s = await api.post(`/archive/${encodeURIComponent(a.name)}/restore`); store.putSite(s); toast(t("sites.restored", { domain: a.domain }), "ok"); await fill(); }
          catch (e) { toastError(e); } finally { restore.classList.remove("busy"); }
        });
        const remove = h("button", { type: "button", class: "btn sm danger" }, t("common.delete"));
        remove.addEventListener("click", async () => {
          try { await api.del(`/archive/${encodeURIComponent(a.name)}`); await fill(); } catch (e) { toastError(e); }
        });
        const mb = (a.bytes / 1048576).toFixed(1);
        list.append(h("div", { class: "row" },
          h("span", { class: "grow" }, h("strong", null, a.domain), " ", h("span", { class: "small muted" }, `${new Date(a.at).toLocaleString()} · ${mb} MB`)),
          h("a", { class: "btn sm", href: `/api/v1/archive/${encodeURIComponent(a.name)}/download`, download: a.name }, icon("download")),
          restore, remove));
      }
    };
    try { await fill(); } catch (e) { toastError(e); return; }
    await open({ title: t("sites.archivedTitle"), body: [h("p", { class: "dim" }, t("sites.archivedBody")), list], buttons: [{ label: t("common.close"), value: null }] });
  });
  search.addEventListener("input", () => { filter = search.value; render(); });

  // A handover emits several events in a second; one render after the
  // burst is enough and keeps the row from flickering under the pointer.
  const rerender = debounce(render, 80);
  subs.push(store.subscribe("sites", rerender));
  subs.push(store.subscribe("events", () => { if (store.get("sitesLoaded")) rerender(); }));

  (async () => {
    try {
      const list = await api.get("/sites");
      store.setSites(list);
    } catch (e) {
      clear(container).append(errorBox(e));
      toastError(e);
    }
  })();
  render();
  if (query && query.get("focus") === "search") setTimeout(() => search.focus(), 0);

  return { el: p.el, destroy() { subs.forEach((u) => u()); } };
}
