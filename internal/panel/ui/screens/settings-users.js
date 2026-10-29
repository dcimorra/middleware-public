// The delegated users: people the operator lets manage some sites and
// nothing else. A card of Settings → Access. A grant names a site of any
// node of the network: the user signs in here and the house finds the
// node, so the list of sites offered is every node's.

import { h, clear, fmtTime, fmtAgo } from "../lib/dom.js";
import { t } from "../lib/i18n.js";
import { api, enc } from "../lib/api.js";
import { table, cell, load } from "../components/page.js";
import { open, confirm, field } from "../components/dialog.js";
import { toast, toastError } from "../components/toast.js";
import { icon } from "../lib/icons.js";
import { store } from "../lib/store.js";
import { cardHead, quietEmpty } from "./settings-kit.js";

// The presets the panel offers, in the order a cautious operator reads
// them. The API takes any set of operate, code and agents; these are the
// two that make sense to hand out. Viewing alone was one and was dropped:
// someone who cannot change their site has no use for seeing it (a grant
// made with it shows as Custom).
const PRESETS = {
  operate: ["operate"],
  full: ["operate", "code", "agents"],
};

function presetOf(perms) {
  const p = [...(perms || [])].filter((x) => x !== "view").sort().join(",");
  for (const [k, v] of Object.entries(PRESETS)) if ([...v].sort().join(",") === p) return k;
  return "custom";
}

// allSites is every site of every node, by name, for the grant's list.
async function allSites() {
  const names = new Set();
  const add = (list) => { for (const s of list || []) if (s && s.domain) names.add(s.domain); };
  try { add(await api.get("/sites", { node: "" })); } catch { /* this node's are what is left */ }
  const peers = (store.get("nodes") && store.get("nodes").members) || [];
  await Promise.all(peers.map(async (p) => { try { add(await api.at(p.id).get("/sites")); } catch { /* that node does not answer now */ } }));
  return [...names].sort();
}

// grantsEditor is one row per site: the site and what may be done there.
function grantsEditor(initial, sites) {
  const list = h("div", { class: "grants" });
  const dl = h("datalist", { id: "grant-sites-" + Math.random().toString(36).slice(2, 8) }, ...sites.map((s) => h("option", { value: s })));
  const rows = [];
  function addRow(g) {
    const site = h("input", { type: "text", list: dl.id, value: g ? g.site : "", placeholder: "shop.example.com", autocapitalize: "off", spellcheck: "false", class: "mono" });
    const level = h("select", null,
      ...["operate", "full"].map((k) => h("option", { value: k }, t("users.preset." + k))),
      h("option", { value: "custom", hidden: true }, t("users.preset.custom")));
    level.value = g ? presetOf(g.perms) : "operate";
    const warn = h("div", { class: "hint warn", hidden: level.value !== "full" }, t("users.fullWarn"));
    level.addEventListener("change", () => { warn.hidden = level.value !== "full"; });
    const rm = h("button", { type: "button", class: "icon-btn sm", "aria-label": t("users.removeGrant"), title: t("users.removeGrant") }, icon("x", { size: 16 }));
    const row = h("div", { class: "grant-row" }, h("div", { class: "grant-fields" }, site, level, rm), warn);
    const entry = { site, level, perms: g ? g.perms : [] };
    rm.addEventListener("click", () => { row.remove(); rows.splice(rows.indexOf(entry), 1); });
    rows.push(entry);
    list.append(row);
  }
  for (const g of initial || []) addRow(g);
  if (!rows.length) addRow(null);
  const more = h("button", { type: "button", class: "btn sm ghost" }, icon("plus"), t("users.addGrant"));
  more.addEventListener("click", () => addRow(null));
  return {
    el: h("div", null, dl, list, more),
    value() {
      return rows
        .filter((r) => r.site.value.trim())
        .map((r) => ({ site: r.site.value.trim().toLowerCase(), perms: r.level.value === "custom" ? r.perms : PRESETS[r.level.value] }));
    },
  };
}

async function editUser(user) {
  const sites = await allSites();
  const editor = grantsEditor(user ? user.grants : [], sites);
  const name = h("input", { type: "text", autocomplete: "off", autocapitalize: "off", spellcheck: "false", required: true });
  const pw = h("input", { type: "password", autocomplete: "new-password", minlength: "8", required: !user });
  const body = [
    user ? null : field(t("users.name"), name, t("users.nameHint")),
    field(user ? t("users.newPassword") : t("auth.password"), pw, user ? t("users.newPasswordHint") : t("users.passwordHint")),
    h("h3", { class: "small" }, t("users.grants")),
    h("p", { class: "dim small" }, t("users.grantsHint")),
    editor.el,
  ];
  return open({
    title: user ? t("users.editTitle", { name: user.name }) : t("users.add"),
    body,
    wide: true,
    buttons: [{ label: t("common.cancel"), value: null }, { label: user ? t("common.save") : t("common.create"), submit: true, primary: true }],
    onSubmit: async (_f, showError) => {
      try {
        if (user) {
          const patch = { grants: editor.value() };
          if (pw.value) patch.password = pw.value;
          return await api.put(`/users/${enc(user.id)}`, patch, { node: "" });
        }
        if (!name.value.trim() || pw.value.length < 8) { showError(t("users.fillIn")); return false; }
        return await api.post("/users", { name: name.value.trim().toLowerCase(), password: pw.value, grants: editor.value() }, { node: "" });
      } catch (e) { showError(e.message); return false; }
    },
  });
}

export function usersCard() {
  const el = h("div", { class: "card pad0 set-card" });
  async function render() {
    const list = await api.get("/users", { node: "" });
    const add = h("button", { type: "button", class: "btn sm primary" }, icon("plus"), t("users.add"));
    add.addEventListener("click", async () => {
      if (await editUser(null)) { toast(t("users.added"), "ok"); load(el, render); }
    });
    const rows = list.map((u) => {
      const edit = h("button", { type: "button", class: "btn sm ghost" }, icon("edit"), t("users.edit"));
      edit.addEventListener("click", async () => { if (await editUser(u)) { toast(t("users.saved"), "ok"); load(el, render); } });
      const del = h("button", { type: "button", class: "btn sm danger ghost" }, t("users.remove"));
      del.addEventListener("click", async () => {
        if (!(await confirm({ title: t("users.removeQ", { name: u.name }), body: t("users.removeBody"), confirmLabel: t("users.remove"), danger: true }))) return;
        try { await api.del(`/users/${enc(u.id)}`, { node: "" }); toast(t("users.removed"), "ok"); load(el, render); } catch (e) { toastError(e); }
      });
      const grants = u.grants.length
        ? h("div", { class: "small" }, ...u.grants.map((g) => h("div", null, h("code", null, g.site), " · ", t("users.preset." + presetOf(g.perms)))))
        : h("span", { class: "muted" }, t("users.noSites"));
      return h("tr", null,
        cell(u.name, null, "primary"),
        cell(grants, t("users.grants")),
        cell(u.last_seen ? h("span", { title: fmtTime(u.last_seen) }, fmtAgo(u.last_seen, t)) : h("span", { class: "muted" }, t("users.never")), t("users.lastSeen")),
        cell(h("div", { class: "row-actions" }, edit, del), null, "right"));
    });
    clear(el).append(
      h("div", { class: "card-pad" }, cardHead(t("users.title"), t("users.hint"), add)),
      rows.length
        ? table([{ label: t("users.name") }, { label: t("users.grants") }, { label: t("users.lastSeen") }, { label: "" }], rows, { responsive: true }).el
        : h("div", { class: "card-pad" }, quietEmpty("lock", t("users.empty"), t("users.emptyHint"))),
    );
  }
  load(el, render);
  return el;
}
