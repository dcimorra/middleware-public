// The site's specs (OpenSpec): what the project says it does today
// (openspec/specs) and what is being changed (openspec/changes), each
// change with its tasks ticked off as the agent works through them. The
// buttons do not build anything themselves: approving or archiving is a
// message into the site's conversation, where the agent runs OpenSpec's
// own workflow and the person sees it happen. The model proposes; a
// person says yes.

import { h, clear } from "../lib/dom.js";
import { t } from "../lib/i18n.js";
import { api, enc } from "../lib/api.js";
import { onEvent } from "../lib/stream.js";
import { loading, errorBox, empty } from "../components/page.js";
import { confirm } from "../components/dialog.js";
import { toast, toastError } from "../components/toast.js";
import { render as renderMd } from "../lib/markdown.js";
import { icon } from "../lib/icons.js";

export function specsTab(domain) {
  const el = h("div", { class: "specs" });
  const base = `/sites/${enc(domain)}`;
  const subs = [];

  async function load() {
    let d;
    try { d = await api.get(`${base}/specs`); } catch (e) { clear(el).append(errorBox(e)); return; }
    clear(el);
    const refresh = h("button", { type: "button", class: "btn ghost sm" }, icon("refresh"), t("common.refresh"));
    refresh.addEventListener("click", load);
    el.append(h("div", { class: "log-bar" }, h("p", { class: "small muted grow" }, t("specs.hint")), refresh));
    if (!d.initialized) { el.append(setUp(d)); return; }

    el.append(h("h3", { class: "specs-h" }, t("specs.changes"), " ", h("span", { class: "badge" }, String(d.changes.length))));
    if (!d.changes.length) el.append(h("p", { class: "small muted" }, t("specs.noChanges")));
    for (const c of d.changes) el.append(changeCard(c));

    el.append(h("h3", { class: "specs-h" }, t("specs.specs"), " ", h("span", { class: "badge" }, String(d.specs.length))));
    if (!d.specs.length) el.append(h("p", { class: "small muted" }, t("specs.noSpecs")));
    else el.append(h("div", { class: "card pad0" }, ...d.specs.map(specRow)));

    if (d.archived.length) {
      el.append(h("details", { class: "advanced specs-archived" },
        h("summary", null, t("specs.archived", { n: d.archived.length })),
        h("ul", { class: "small" }, ...d.archived.map((a) => h("li", null, h("code", null, a.id), a.date ? h("span", { class: "muted" }, " · " + a.date) : null)))));
    }
  }

  // A project without openspec/: set up from here when the module is on,
  // or a word on where to turn it on.
  function setUp(d) {
    if (!d.enabled) {
      return empty(t("specs.offTitle"), t("specs.offHint"), h("a", { class: "btn", href: "#/modules" }, t("specs.toModules")), "file");
    }
    const btn = h("button", { type: "button", class: "btn primary" }, t("specs.init"));
    btn.addEventListener("click", async () => {
      btn.classList.add("busy");
      try { await api.post(`${base}/specs/init`); toast(t("specs.initDone"), "ok"); load(); }
      catch (e) { toastError(e); } finally { btn.classList.remove("busy"); }
    });
    return empty(t("specs.noneTitle"), t("specs.noneHint"), btn, "file");
  }

  function changeCard(c) {
    const pct = c.total ? Math.round((100 * c.done) / c.total) : 0;
    const finished = c.total > 0 && c.done === c.total;
    const body = h("div", { class: "specs-files", hidden: true });
    const see = h("button", { type: "button", class: "btn sm" }, icon("eye"), t("specs.read"));
    see.addEventListener("click", () => toggleFiles(body, c.files, see));
    const acts = h("div", { class: "row" }, see);
    if (!finished) {
      const go = h("button", { type: "button", class: "btn sm primary" }, icon("check"), t(c.done ? "specs.resume" : "specs.approve"));
      go.addEventListener("click", () => ask(c, "apply", go));
      acts.append(go);
    } else {
      const arch = h("button", { type: "button", class: "btn sm primary" }, icon("box"), t("specs.archive"));
      arch.addEventListener("click", () => ask(c, "archive", arch));
      acts.append(arch);
    }
    return h("div", { class: "card specs-change" },
      h("div", { class: "row" }, h("strong", { class: "grow" }, c.title), h("code", { class: "small muted" }, c.id)),
      c.why ? h("p", { class: "small dim" }, c.why) : null,
      c.total ? h("div", null,
        h("div", { class: "meter", role: "progressbar", "aria-valuenow": String(pct), "aria-valuemin": "0", "aria-valuemax": "100" }, h("div", { class: "fill", style: `width:${pct}%` })),
        h("div", { class: "small muted" }, t("specs.tasks", { done: c.done, total: c.total })))
        : h("div", { class: "small muted" }, t("specs.noTasks")),
      acts, body);
  }

  function specRow(s) {
    const body = h("div", { class: "specs-files", hidden: true });
    const see = h("button", { type: "button", class: "btn sm ghost" }, t("specs.read"));
    see.addEventListener("click", () => toggleFiles(body, [s.path], see));
    return h("div", { class: "specs-spec" },
      h("div", { class: "row" },
        h("div", { class: "grow" }, h("strong", null, s.title), " ", h("code", { class: "small muted" }, s.capability)),
        h("span", { class: "small muted" }, t("specs.requirements", { n: s.requirements })), see),
      body);
  }

  // The markdown as the agent wrote it, read from the site's folder.
  async function toggleFiles(body, files, btn) {
    if (!body.hidden) { body.hidden = true; return; }
    body.hidden = false;
    if (body.dataset.loaded) return;
    clear(body).append(loading());
    btn.classList.add("busy");
    try {
      const texts = await Promise.all(files.map((p) => api.text(`${base}/file?path=${enc(p)}`).catch((e) => `> ${e.message}`)));
      clear(body);
      files.forEach((p, i) => {
        const md = renderMd(texts[i], {}).el;
        md.classList.add("chat-md");
        body.append(h("div", { class: "specs-file" }, h("div", { class: "small muted mono" }, p.replace(/^openspec\//, "")), md));
      });
      body.dataset.loaded = "1";
    } catch (e) { clear(body).append(errorBox(e)); } finally { btn.classList.remove("busy"); }
  }

  // Approving and archiving are said to the site's agent, in its first
  // conversation, and the panel goes there to watch it: the work happens
  // where the person can read it and stop it.
  async function ask(c, what, btn) {
    if (!(await confirm({ title: t(`specs.${what}Q`, { title: c.title }), body: t(`specs.${what}Body`), confirmLabel: t(what === "apply" ? (c.done ? "specs.resume" : "specs.approve") : "specs.archive") }))) return;
    btn.classList.add("busy");
    try {
      await api.post(`${base}/chat`, { text: t(`specs.${what}Msg`, { id: c.id }) });
      location.hash = `#/sites/${enc(domain)}/chat`;
    } catch (e) { toastError(e); } finally { btn.classList.remove("busy"); }
  }

  // The tasks tick as the agent works: the end of each of its turns in
  // this site reads the folder again.
  subs.push(onEvent("agent.", (ev) => {
    if (/^agent\.(done|error|stopped)$/.test(ev.type) && ev.subject && ev.subject.split("#")[0] === domain) load();
  }));
  subs.push(onEvent("specs.init", (ev) => { if (ev.subject === domain) load(); }));

  el.append(loading());
  load();
  return { el, destroy() { subs.forEach((u) => u()); } };
}

