// OpenCode's providers on its module card: the ones connected (each with
// how many of its models the chat now offers), a box to connect another
// from OpenCode's own catalog with its key, and the models the chat lists.
// Connecting is what makes models appear: the list is the CLI's, read with
// the keys it has.

import { h, clear, fmtAgo } from "../lib/dom.js";
import { t } from "../lib/i18n.js";
import { api, enc } from "../lib/api.js";
import { notice } from "../components/page.js";
import { field, confirm } from "../components/dialog.js";
import { toast, toastError } from "../components/toast.js";
import { icon } from "../lib/icons.js";

const BASE = "/modules/agent-opencode";

// The best known, offered as one tap; the catalog's other two hundred are
// one search away. Ids as OpenCode's catalog names them.
const POPULAR = ["anthropic", "openai", "google", "deepseek", "zai", "zai-coding-plan", "moonshotai", "alibaba", "mistral", "xai", "minimax", "groq", "openrouter", "opencode", "togetherai", "fireworks-ai", "cerebras", "perplexity"];

export function openCodeProviders() {
  const el = h("div", { class: "oc-providers" });
  let state = null;

  async function load(fresh) {
    try { state = fresh || await api.get(BASE + "/providers"); paint(); }
    catch (e) { clear(el).append(notice("bad", e.message)); }
  }

  function paint() {
    const s = state;
    clear(el);
    el.append(h("h3", null, t("oc.title")));
    // The models are read after a key is stored, and the screen may have
    // been drawn again meanwhile: while they are being read, say so and
    // ask again, or the card would stay at zero until a reload.
    if (s.reading) {
      el.append(h("p", { class: "small muted" }, t("oc.reading")));
      setTimeout(() => { if (el.isConnected) load(); }, 2000);
    }
    if ((s.connected || []).length === 0) el.append(h("p", { class: "small muted" }, t("oc.none")));
    else {
      el.append(h("ul", { class: "oc-list" }, ...s.connected.map((p) => {
        const rm = h("button", { type: "button", class: "btn sm ghost", title: t("oc.remove") }, icon("trash"));
        rm.addEventListener("click", async () => {
          if (!(await confirm({ title: t("oc.removeQ", { name: p.name }), body: t("oc.removeBody"), confirmLabel: t("oc.remove"), danger: true }))) return;
          rm.classList.add("busy");
          try { await load(await api.del(`${BASE}/providers/${enc(p.id)}`)); toast(t("oc.removed", { name: p.name }), "ok"); }
          catch (e) { toastError(e); rm.classList.remove("busy"); }
        });
        return h("li", null, h("span", { class: "dot ok" }), h("strong", null, p.name), h("span", { class: "mono small muted" }, p.id),
          h("span", { class: "small muted grow" }, t("oc.models", { n: p.models })), rm);
      })));
    }
    // Connect one: a name from the catalog (searchable) and its key.
    const cat = s.catalog || [];
    const listID = "oc-catalog";
    const pick = h("input", { type: "text", list: listID, placeholder: t("oc.pickPlaceholder"), autocomplete: "off", spellcheck: "false" });
    const dl = h("datalist", { id: listID }, ...cat.map((c) => h("option", { value: c.name + " · " + c.id })));
    const key = h("input", { type: "password", class: "mono", autocomplete: "new-password", placeholder: t("oc.keyPlaceholder") });
    const envHint = h("div", { class: "hint" });
    const idOf = () => {
      const v = pick.value.trim();
      const tail = v.includes("·") ? v.split("·").pop().trim() : v;
      const c = cat.find((x) => x.id === tail.toLowerCase()) || cat.find((x) => x.name.toLowerCase() === v.toLowerCase());
      return c ? c : (tail ? { id: tail.toLowerCase(), name: tail } : null);
    };
    pick.addEventListener("input", () => {
      const c = idOf();
      envHint.textContent = c && c.env && c.env.length ? t("oc.envHint", { env: c.env.join(", ") }) : "";
    });
    const add = h("button", { type: "button", class: "btn primary sm" }, icon("plus"), t("oc.connect"));
    add.addEventListener("click", async () => {
      const c = idOf();
      if (!c || !key.value.trim()) { toast(t("oc.needBoth"), "bad"); return; }
      add.classList.add("busy");
      try {
        await load(await api.put(`${BASE}/providers/${enc(c.id)}`, { key: key.value.trim() }));
        toast(t("oc.connected", { name: c.name }), "ok");
      } catch (e) { toastError(e); } finally { add.classList.remove("busy"); }
    });
    const connectedIDs = new Set((s.connected || []).map((c) => c.id));
    const byID = new Map(cat.map((c) => [c.id, c]));
    const chips = h("div", { class: "oc-popular" }, ...POPULAR.map((id) => {
      const c = byID.get(id) || { id, name: id };
      const chip = h("button", { type: "button", class: "chip" + (connectedIDs.has(id) ? " on" : ""), title: c.id }, connectedIDs.has(id) ? icon("check", { size: 14 }) : null, c.name);
      chip.addEventListener("click", () => {
        pick.value = c.name + " · " + c.id;
        pick.dispatchEvent(new Event("input"));
        key.focus();
      });
      return chip;
    }));
    el.append(h("div", { class: "oc-add" },
      h("div", { class: "field" }, h("label", null, t("oc.popular")), chips),
      field(t("oc.provider"), pick, cat.length ? t("oc.providerHint", { n: cat.length }) : t("oc.noCatalog")), dl,
      h("div", { class: "field" }, h("label", null, t("oc.key")), key, envHint),
      h("div", { class: "form-actions" }, add)));
    // The models the chat lists now.
    const models = s.models || [];
    const refresh = h("button", { type: "button", class: "btn sm" }, icon("refresh"), t("oc.refresh"));
    refresh.addEventListener("click", async () => {
      refresh.classList.add("busy");
      try { await load(await api.post(BASE + "/models/refresh", {})); } catch (e) { toastError(e); } finally { refresh.classList.remove("busy"); }
    });
    const when = s.models_at ? t("oc.readAgo", { ago: fmtAgo(s.models_at, t) }) : "";
    el.append(...[h("details", { class: "oc-models" },
      h("summary", null, t("oc.modelsTitle", { n: models.length }), when ? h("span", { class: "small muted" }, " · " + when) : null),
      models.length ? h("ul", { class: "oc-model-list mono small" }, ...models.map((m) => h("li", null, m))) : h("p", { class: "small muted" }, t("oc.noModels"))),
    s.models_error ? notice("warn", t("oc.modelsError", { error: s.models_error })) : null,
    h("div", { class: "form-actions" }, refresh)].filter(Boolean));
  }

  load();
  return el;
}
