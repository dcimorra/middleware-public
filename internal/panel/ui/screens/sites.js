// Home: every site on the machine, live. Rows are re-rendered from the
// store whenever the stream touches a site, so a handover shows as it
// happens; the search box filters in memory because the list is already
// here and a round trip would only add a delay.

import { h, clear, shortRev, fmtAgo, debounce } from "../lib/dom.js";
import { t } from "../lib/i18n.js";
import { api, apiURL } from "../lib/api.js";
import { store } from "../lib/store.js";
import { navigate } from "../lib/router.js";
import { page, empty, errorBox, loading, table, cell, notice, search as searchBox } from "../components/page.js";
import { icon } from "../lib/icons.js";

import { modeBadge, aliveStatus, siteMode } from "../components/badge.js";
import { open, field, checkbox } from "../components/dialog.js";
import { toast, toastError } from "../components/toast.js";
import { currentNode, setCurrentNode } from "../lib/node.js";

export function sitesScreen({ query }) {
  const search = h("input", { type: "search", placeholder: t("sites.search"), "aria-label": t("sites.search"), autocomplete: "off" });
  const newBtn = h("button", { type: "button", class: "btn primary", "data-shortcut": "new" }, icon("plus"), t("sites.new"));
  const archivedBtn = h("button", { type: "button", class: "btn" }, icon("download"), t("sites.archived"));
  const importBtn = h("button", { type: "button", class: "btn" }, icon("download"), t("sites.importV1"));
  const p = page({ title: t("nav.sites"), icon: "globe", actions: [searchBox(search), archivedBtn, importBtn, newBtn] });
  const container = h("div", { class: "card pad0" });
  const count = h("p", { class: "small muted", role: "status" });
  // Bulk actions: a bar over the list while something is selected. The
  // selection survives re-renders (the stream re-renders often) and drops
  // sites that went away.
  const bulkBar = h("div", { class: "notice bulk-bar", hidden: true });
  // With other nodes linked, the list says whose sites these are, and the
  // other machines' follow, each under its name.
  const here = h("h2", { class: "node-title", hidden: true });
  const others = h("div", { class: "node-others" });
  p.body.append(here, bulkBar, container, count, others);

  let filter = "";
  const selected = new Set();
  let visible = [];
  const subs = [];

  function render() {
    const all = store.siteList();
    if (!store.get("sitesLoaded")) { clear(container).append(loading()); return; }
    const q = filter.trim().toLowerCase();
    const groupOf = (s) => (s.manifest && s.manifest.group) || "";
    const list = (q ? all.filter((s) => s.domain.toLowerCase().includes(q) || siteMode(s).includes(q) || groupOf(s).toLowerCase().includes(q)) : all)
      // Grouped sites first, by group, then the rest; by domain inside.
      .slice().sort((a, b) => (groupOf(a) === "") - (groupOf(b) === "") || groupOf(a).localeCompare(groupOf(b)) || a.domain.localeCompare(b.domain));
    for (const d of [...selected]) if (!store.site(d)) selected.delete(d);
    visible = list.map((s) => s.domain);
    renderBulk();
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
        rows.push(h("tr", { class: "group-row" }, h("th", { colspan: "7", scope: "colgroup" }, current || t("sites.ungrouped"))));
      }
      rows.push(row(s));
    }
    const all_ = h("input", { type: "checkbox", "aria-label": t("sites.selectAll") });
    all_.checked = visible.length > 0 && visible.every((d) => selected.has(d));
    all_.addEventListener("change", () => {
      for (const d of visible) { if (all_.checked) selected.add(d); else selected.delete(d); }
      render();
    });
    container.append(table([
      { label: all_, class: "check-col" },
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
    const box = h("input", { type: "checkbox", "aria-label": t("sites.select", { domain: s.domain }) });
    box.checked = selected.has(s.domain);
    box.addEventListener("click", (e) => e.stopPropagation());
    box.addEventListener("change", () => { if (box.checked) selected.add(s.domain); else selected.delete(s.domain); render(); });
    const tr = h("tr", { class: "row-link", tabindex: "-1" },
      cell(box, null, "check-col"),
      cell(h("a", { class: "cell-link", href }, s.domain), null, "primary"),
      cell(modeBadge(s), t("sites.col.mode")),
      cell(aliveStatus(s), t("sites.col.status")),
      cell(s.port ? String(s.port) : "", t("sites.col.port"), "num mono"),
      cell(shortRev(s.revision), t("sites.col.revision"), "mono"),
      cell(last ? h("span", { class: "small dim", title: last.time }, h("span", { class: "mono" }, last.type), " ", last.message || "", " · ", fmtAgo(last.time, t)) : h("span", { class: "small muted" }, "—"), t("sites.col.lastEvent")),
    );
    tr.addEventListener("click", (e) => {
      if (e.target.closest("a") || e.target.closest("input")) return;
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
      // From a repository the template is the repository's to say.
      if (typeof source !== "undefined" && source.value === "repo") return;
      if (isProject()) template.value = "project";
      else if (template.value === "project") template.value = "static";
    };
    domain.addEventListener("input", syncPublish);
    // Where the code comes from: a template, or a GitHub repository (only
    // with the GitHub module on). From a repository, the template is what
    // its root says unless chosen.
    const source = h("select", null, h("option", { value: "template" }, t("sites.fromTemplate")), h("option", { value: "repo" }, t("sites.fromRepo")));
    const repoListID = "repos-" + Math.random().toString(36).slice(2, 8);
    const repo = h("input", { type: "text", list: repoListID, placeholder: t("sites.repoPlaceholder"), autocomplete: "off", spellcheck: "false" });
    const repoList = h("datalist", { id: repoListID });
    const repoHint = h("div", { class: "hint" }, t("sites.repoHint"));
    const branch = h("input", { type: "text", placeholder: t("sites.branchPlaceholder"), autocomplete: "off", spellcheck: "false" });
    const auto = h("option", { value: "auto", hidden: true }, t("template.auto"));
    template.prepend(auto);
    const repoFields = h("div", { hidden: true }, h("div", { class: "field" }, h("label", null, t("sites.repo")), repo, repoList, repoHint), field(t("sites.branch"), branch, t("sites.branchHint")));
    let githubOn = false;
    api.get("/modules").then((list) => {
      const gh = (Array.isArray(list) ? list : list.modules || []).find((m) => m.key === "github");
      githubOn = Boolean(gh && gh.enabled);
      source.options[1].disabled = !githubOn;
      if (!githubOn) source.options[1].textContent = t("sites.fromRepoOff");
    }).catch(() => {});
    const syncSource = async () => {
      const fromRepo = source.value === "repo";
      repoFields.hidden = !fromRepo;
      auto.hidden = !fromRepo;
      template.value = fromRepo ? "auto" : (isProject() ? "project" : "static");
      if (fromRepo && !repoList.childElementCount) {
        try {
          const r = await api.get("/modules/github/repos");
          for (const x of r.repos || []) repoList.append(h("option", { value: x.full_name }, (x.private ? "🔒 " : "") + (x.description || "")));
          repoHint.textContent = r.error ? t("sites.repoNoToken") : t("sites.repoHint", { n: (r.repos || []).length });
        } catch (e) { repoHint.textContent = e.message; }
      }
    };
    source.addEventListener("change", syncSource);
    // Where it is made: the node being viewed unless another is chosen.
    const nodes_ = store.get("nodes");
    const where = h("select", null);
    if (nodes_ && (nodes_.members || []).length) {
      where.append(h("option", { value: "" }, t("nodes.thisOne", { name: nodes_.self.name })), ...nodes_.members.map((x) => h("option", { value: x.id }, x.name)));
      where.value = currentNode();
    }
    let published = false;
    const made = await open({
      title: t("sites.newTitle"),
      body: [
        field(t("sites.domain"), domain, t("sites.domainHint")),
        where.options.length ? field(t("sites.node"), where, t("sites.nodeHint")) : null,
        field(t("sites.source"), source),
        repoFields,
        field(t("sites.template"), template, t("sites.templateHint")),
        h("div", { class: "field" }, publish.el, h("div", { class: "hint" }, t("sites.publishHint"))),
      ],
      buttons: [{ label: t("common.cancel"), value: null }, { label: t("common.create"), submit: true, primary: true }],
      onSubmit: async (_form, showError) => {
        const d = domain.value.trim().toLowerCase();
        if (!/^[a-z0-9-]+(\.[a-z0-9-]+)*$/.test(d)) { showError(t("sites.badDomain")); return false; }
        const body = { domain: d, template: template.value };
        if (source.value === "repo") {
          if (!repo.value.trim()) { showError(t("sites.repoNeeded")); return false; }
          body.repository = repo.value.trim();
          if (branch.value.trim()) body.branch = branch.value.trim();
          if (body.template === "auto") delete body.template;
        }
        if (!isProject()) body.publish = publish.input.checked;
        const target = where.options.length ? where.value : currentNode();
        const site = await api.at(target).post("/sites", body);
        published = Boolean(site.publishing);
        if (target === currentNode()) store.putSite(site);
        else setCurrentNode(target);
        return site.domain;
      },
    });
    if (made) {
      toast(t(published ? "sites.createdPublishing" : "sites.created", { domain: made }), "ok");
      navigate(`/sites/${encodeURIComponent(made)}`);
    }
  }

  function renderBulk() {
    clear(bulkBar);
    bulkBar.hidden = selected.size === 0;
    if (!selected.size) return;
    const del = h("button", { type: "button", class: "btn sm danger" }, icon("trash"), t("sites.bulkDelete"));
    const none = h("button", { type: "button", class: "btn sm" }, t("sites.selectNone"));
    del.addEventListener("click", bulkDelete);
    none.addEventListener("click", () => { selected.clear(); render(); });
    bulkBar.append(h("div", { class: "grow" }, h("strong", null, t("sites.selected", { n: selected.size }))), h("div", { class: "actions" }, none, del));
  }

  // One confirmation that names every site and makes the count typed, not
  // clicked: deleting thirty sites must not be one tired Enter. Archiving
  // is on by default, as for one site. They go one at a time and each
  // result is shown; a failure stops nothing else.
  async function bulkDelete() {
    const domains = [...selected].sort();
    const archive = checkbox(t("site.archive"), { checked: true });
    const typed = h("input", { type: "text", inputmode: "numeric", autocomplete: "off" });
    const results = h("div", { class: "stack-sm small" });
    let done = false;
    await open({
      title: t("sites.bulkDeleteQ", { n: domains.length }),
      wide: true,
      body: [
        h("p", null, t("sites.bulkDeleteBody")),
        h("p", { class: "mono small" }, domains.join("  ")),
        h("div", { class: "field" }, archive.el, h("div", { class: "hint" }, t("site.archiveHint"))),
        field(t("sites.bulkDeleteType", { n: domains.length }), typed),
        results,
      ],
      buttons: [{ label: t("common.close"), value: null }, { label: t("sites.bulkDelete"), submit: true, danger: true, primary: true }],
      onSubmit: async (_f, showError) => {
        if (done) return true;
        if (typed.value.trim() !== String(domains.length)) { showError(t("sites.bulkDeleteTypeWrong", { n: domains.length })); return false; }
        showError("");
        clear(results);
        for (const d of domains) {
          const line = h("div", null, h("code", null, d), " … ");
          results.append(line);
          try {
            await api.del(`/sites/${encodeURIComponent(d)}` + (archive.input.checked ? "?archive=true" : ""));
            store.dropSite(d);
            selected.delete(d);
            line.append(h("span", { class: "level-ok" }, archive.input.checked ? t("sites.bulkArchived") : t("sites.bulkDeleted")));
          } catch (e) {
            line.append(h("span", { class: "level-error" }, e.message));
          }
        }
        done = true;
        render();
        return false; // the results are the answer; Close ends it
      },
    });
  }

  newBtn.addEventListener("click", newSite);
  importBtn.addEventListener("click", importV1);

  // A site from a V1 house: a token for that one site and two hours, and
  // the command to paste on the V1 machine. The house makes the token; the
  // V1 machine only ever holds that one.
  async function importV1() {
    const domain = h("input", { type: "text", placeholder: "example.com", autocomplete: "off", autocapitalize: "off", spellcheck: "false" });
    const base = h("input", { type: "text", placeholder: "https://panel.example.com", autocomplete: "off", autocapitalize: "off", spellcheck: "false" });
    const made = await open({
      title: t("sites.importV1"),
      body: [h("p", { class: "dim" }, t("sites.importV1Body")), field(t("sites.importV1Domain"), domain), field(t("sites.importV1Base"), base, t("sites.importV1BaseHint"))],
      buttons: [{ label: t("common.cancel"), value: null }, { label: t("sites.importV1Make"), submit: true, primary: true }],
      onSubmit: async () => api.post("/relocate/import-token", { domain: domain.value.trim().toLowerCase(), base: base.value.trim() }),
    });
    if (!made) return;
    const code = h("pre", { class: "snippet" }, made.command);
    const copy = h("button", { type: "button", class: "btn" }, icon("copy"), t("door.copy"));
    copy.addEventListener("click", async () => { try { await navigator.clipboard.writeText(made.command); toast(t("door.copied"), "ok"); } catch { /* it stays on screen */ } });
    await open({
      title: t("sites.importV1Ready", { domain: made.domain }),
      body: [h("p", { class: "dim" }, t("sites.importV1Run", { when: new Date(made.expires).toLocaleTimeString() })), code, copy, h("p", { class: "small muted" }, t("sites.importV1After"))],
      buttons: [{ label: t("common.close"), value: null }],
    });
  }
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
          h("a", { class: "btn sm", href: apiURL(`/archive/${encodeURIComponent(a.name)}/download`), download: a.name }, icon("download")),
          restore, remove));
      }
    };
    try { await fill(); } catch (e) { toastError(e); return; }
    await open({ title: t("sites.archivedTitle"), body: [h("p", { class: "dim" }, t("sites.archivedBody")), list], buttons: [{ label: t("common.close"), value: null }] });
  });
  search.addEventListener("input", () => { filter = search.value; render(); paintOthers(); });

  // The other nodes: what was chosen in the selector is above, live; the
  // rest are read every twenty seconds through this node.
  function nodeList() {
    const n = store.get("nodes");
    if (!n || !(n.members || []).length) return [];
    const list = currentNode() ? [{ id: "", name: n.self.name }] : [];
    for (const peer of n.members) if (peer.id !== currentNode()) list.push(peer);
    return list;
  }
  function paintHere() {
    const n = store.get("nodes");
    const peers = (n && n.members) || [];
    here.hidden = peers.length === 0;
    if (here.hidden) return;
    const name = currentNode() ? (peers.find((x) => x.id === currentNode()) || {}).name : n.self.name;
    clear(here).append(...[icon("server"), h("span", null, name), currentNode() ? null : h("span", { class: "badge info" }, t("dash.thisNode"))].filter(Boolean));
  }
  let otherSites = [];
  async function loadOthers() {
    const list = nodeList();
    otherSites = await Promise.all(list.map(async (node) => {
      try { return { node, sites: await api.at(node.id).get("/sites") }; } catch (e) { return { node, error: e.message }; }
    }));
    paintOthers();
  }
  function paintOthers() {
    clear(others);
    const q = filter.trim().toLowerCase();
    for (const { node, sites, error } of otherSites) {
      const card = h("section", { class: "card node-section" }, h("h2", null, icon("server"), h("span", null, node.name)));
      if (error) { card.append(notice("bad", error)); others.append(card); continue; }
      const list = (sites || []).filter((x) => !q || x.domain.toLowerCase().includes(q) || siteMode(x).includes(q)).sort((a, b) => a.domain.localeCompare(b.domain));
      if (!list.length) { card.append(h("p", { class: "small muted" }, q ? t("sites.noMatch") : t("sites.emptyTitle"))); others.append(card); continue; }
      const open_ = (x) => { setCurrentNode(node.id); navigate(`/sites/${encodeURIComponent(x.domain)}`); };
      card.append(table([
        { label: t("sites.col.domain") }, { label: t("sites.col.mode") }, { label: t("sites.col.status") }, { label: t("sites.col.revision") },
      ], list.map((x) => {
        const a = h("a", { class: "cell-link", href: "#" }, x.domain);
        a.addEventListener("click", (e) => { e.preventDefault(); open_(x); });
        const tr = h("tr", { class: "row-link", tabindex: "-1" },
          cell(a, null, "primary"), cell(modeBadge(x), t("sites.col.mode")), cell(aliveStatus(x), t("sites.col.status")), cell(shortRev(x.revision), t("sites.col.revision"), "mono"));
        tr.addEventListener("click", (e) => { if (!e.target.closest("a")) open_(x); });
        return tr;
      }), { responsive: true }).el);
      others.append(card);
    }
  }
  subs.push(store.subscribe("nodes", () => { paintHere(); loadOthers(); }));
  paintHere();
  loadOthers();
  const othersTimer = setInterval(() => { if (!document.hidden) loadOthers(); }, 20000);
  subs.push(() => clearInterval(othersTimer));

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
