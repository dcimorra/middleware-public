// Tasks: scheduled work. The list is live from task.* events; the editor
// is the same JSON editor the manifest uses because a task file is one
// object and a form would need updating every time a field is added.

import { icon } from "../lib/icons.js";
import { h, clear, fmtTime, fmtAgo, fmtDuration } from "../lib/dom.js";
import { t } from "../lib/i18n.js";
import { api, enc } from "../lib/api.js";
import { onEvent } from "../lib/stream.js";
import { page, load, table, cell, empty, loading, errorBox } from "../components/page.js";
import { jsonEditor } from "../components/editor.js";
import { open, confirm, field } from "../components/dialog.js";
import { toast, toastError } from "../components/toast.js";

const TEMPLATE = { version: 2, schedule: "0 3 * * *", site: "", command: ["./scripts/backup.sh"], timeout: "1h", overlap: "skip" };

export function tasksScreen() {
  const newBtn = h("button", { type: "button", class: "btn primary", "data-shortcut": "new" }, icon("plus"), t("tasks.new"));
  const p = page({ title: t("nav.tasks"), sub: t("tasks.sub"), icon: "clock", actions: newBtn });
  const body = h("div", { class: "card pad0" });
  p.body.append(body);
  const subs = [];

  async function render() {
    const tasks = await api.get("/tasks");
    if (!tasks || tasks.length === 0) return empty(t("tasks.emptyTitle"), t("tasks.emptyHint"), h("button", { type: "button", class: "btn primary", onClick: () => edit(null) }, icon("plus"), t("tasks.new")), "clock");
    const rows = tasks.map((task) => {
      const last = task.last || {};
      const st = last.status;
      const statusEl = st ? h("span", { class: "status", title: last.at ? fmtTime(last.at) : "" },
        h("span", { class: "dot " + (st === "ok" || st === "success" ? "ok" : st === "running" ? "warn" : "bad") }),
        st, last.duration ? h("span", { class: "muted small" }, " · " + fmtDuration(Number(last.duration))) : null,
        last.at ? h("span", { class: "muted small" }, " · " + fmtAgo(last.at, t)) : null,
      ) : h("span", { class: "muted" }, t("tasks.neverRan"));
      return h("tr", null,
        cell(h("a", { href: "#", class: "cell-link", onClick: (e) => { e.preventDefault(); edit(task); } }, task.name), null, "primary"),
        cell(h("code", null, task.schedule || ""), t("tasks.col.schedule")),
        cell(task.site ? h("a", { href: `#/sites/${enc(task.site)}` }, task.site) : h("span", { class: "muted" }, t("tasks.root")), t("tasks.col.site")),
        cell(task.next ? h("span", { title: fmtTime(task.next) }, fmtAgo(task.next, t)) : "", t("tasks.col.next")),
        cell(statusEl, t("tasks.col.last")),
        cell(h("div", { class: "actions" }, task.running ? stopBtn(task) : runBtn(task), logBtn(task), delBtn(task)), null, "right"),
      );
    });
    return table([
      { label: t("tasks.col.name") }, { label: t("tasks.col.schedule") }, { label: t("tasks.col.site") }, { label: t("tasks.col.next") }, { label: t("tasks.col.last") }, { label: "" },
    ], rows, { responsive: true }).el;
  }

  function runBtn(task) {
    const b = h("button", { type: "button", class: "btn sm" }, icon("play"), t("tasks.run"));
    b.addEventListener("click", async () => {
      b.classList.add("busy");
      try { await api.post(`/tasks/${enc(task.name)}/run`); toast(t("tasks.started", { name: task.name }), "ok"); }
      catch (e) { toastError(e); } finally { b.classList.remove("busy"); }
    });
    return b;
  }
  function stopBtn(task) {
    const b = h("button", { type: "button", class: "btn sm danger" }, icon("stop"), t("tasks.stop"));
    b.addEventListener("click", async () => {
      b.classList.add("busy");
      try { await api.post(`/tasks/${enc(task.name)}/stop`); toast(t("tasks.stopped", { name: task.name }), "ok"); }
      catch (e) { toastError(e); } finally { b.classList.remove("busy"); }
    });
    return b;
  }
  function logBtn(task) {
    const b = h("button", { type: "button", class: "btn sm" }, t("tasks.log"));
    b.addEventListener("click", async () => {
      const pre = h("pre", { class: "log", tabindex: "0" });
      const box = h("div", null, loading());
      const dlg = open({ title: t("tasks.logOf", { name: task.name }), body: box, wide: true, buttons: [{ label: t("common.close"), value: null }] });
      try {
        const text = await api.text(`/tasks/${enc(task.name)}/log`);
        pre.textContent = text || t("tasks.logEmpty");
        clear(box).append(pre);
        pre.scrollTop = pre.scrollHeight;
      } catch (e) { clear(box).append(errorBox(e)); }
      await dlg;
    });
    return b;
  }
  function delBtn(task) {
    const b = h("button", { type: "button", class: "btn sm danger ghost" }, icon("trash"), t("common.delete"));
    b.addEventListener("click", async () => {
      if (!(await confirm({ title: t("tasks.deleteQ", { name: task.name }), body: t("tasks.deleteBody"), confirmLabel: t("common.delete"), danger: true }))) return;
      try { await api.del(`/tasks/${enc(task.name)}`); toast(t("tasks.deleted", { name: task.name }), "ok"); load(body, render); } catch (e) { toastError(e); }
    });
    return b;
  }

  async function edit(task) {
    const name = h("input", { type: "text", class: "mono", value: task ? task.name : "", readOnly: Boolean(task), placeholder: "backup", pattern: "[a-z0-9-]+" });
    const { version, schedule, site, command, timeout, overlap, ...rest } = task || {};
    const initial = task ? { version: version || 2, schedule, site, command, timeout, overlap, ...rest } : TEMPLATE;
    delete initial.name; delete initial.next; delete initial.last;
    const editor = jsonEditor({ value: initial, rows: 12 });
    editor.el.querySelector(".editor-bar").hidden = true;
    const done = await open({
      title: task ? t("tasks.editTitle", { name: task.name }) : t("tasks.newTitle"),
      wide: true,
      body: [field(t("tasks.name"), name, task ? null : t("tasks.nameHint")), editor.el, h("p", { class: "small muted" }, t("tasks.jsonHint"))],
      buttons: [{ label: t("common.cancel"), value: null }, { label: t("common.save"), submit: true, primary: true }],
      onSubmit: async (_f, showError) => {
        const n = name.value.trim();
        if (!/^[a-z0-9-]+$/.test(n)) { showError(t("tasks.badName")); return false; }
        const r = editor.parse();
        if (!r.ok) { showError(r.error.message); return false; }
        await api.put(`/tasks/${enc(n)}`, r.value);
        return n;
      },
    });
    if (done) { toast(t("tasks.saved", { name: done }), "ok"); load(body, render); }
  }

  newBtn.addEventListener("click", () => edit(null));
  load(body, render);
  subs.push(onEvent("task.", () => { if (!document.querySelector("dialog[open]")) load(body, render); }));
  return { el: p.el, destroy() { subs.forEach((u) => u()); } };
}
