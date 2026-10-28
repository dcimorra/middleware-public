// Tasks: scheduled work. The list is live from task.* events; the editor
// is the same JSON editor the manifest uses because a task file is one
// object and a form would need updating every time a field is added.

import { icon } from "../lib/icons.js";
import { h, clear, fmtTime, fmtAgo, fmtDuration } from "../lib/dom.js";
import { t, currentLang } from "../lib/i18n.js";
import { api, enc } from "../lib/api.js";
import { onEvent } from "../lib/stream.js";
import { page, load, empty, loading, errorBox } from "../components/page.js";
import { jsonEditor } from "../components/editor.js";
import { open, confirm, field } from "../components/dialog.js";
import { toast, toastError } from "../components/toast.js";

const TEMPLATE = { version: 2, schedule: "0 3 * * *", site: "", command: ["./scripts/backup.sh"], timeout: "1h", overlap: "skip" };

// cronWords says a schedule as a sentence ("Every day at 03:00", "Every
// 15 minutes") for the shapes people write, and null for the rest, which
// then show as written. "0 3 * * *" is a puzzle at a glance even for
// whoever wrote it; the raw expression stays beside the sentence.
// The aliases are internal/cron's; a schedule the server would refuse
// never reaches the list, so this only has to be right, not strict.
const ALIASES = { "@hourly": "0 * * * *", "@daily": "0 0 * * *", "@midnight": "0 0 * * *", "@weekly": "0 0 * * 0", "@monthly": "0 0 1 * *", "@yearly": "0 0 1 1 *", "@annually": "0 0 1 1 *" };
const DOW = { sun: 0, mon: 1, tue: 2, wed: 3, thu: 4, fri: 5, sat: 6 };
const MON = { jan: 1, feb: 2, mar: 3, apr: 4, may: 5, jun: 6, jul: 7, aug: 8, sep: 9, oct: 10, nov: 11, dec: 12 };

export function cronWords(expr) {
  const e = String(expr || "").trim().toLowerCase();
  const f = (ALIASES[e] || e).split(/\s+/);
  if (f.length !== 5) return null;
  const [mi, ho, dom, mon, dow] = f;
  const num = (s, names) => (/^\d+$/.test(s) ? Number(s) : names && s in names ? names[s] : null);
  // A list of plain values ("1,15", "mon,thu"), or null if it is more.
  const list = (s, names) => {
    const out = s.split(",").map((x) => num(x, names));
    return out.some((x) => x === null) ? null : out;
  };
  const every = (s) => { const m = /^\*\/(\d+)$/.exec(s); return m ? Number(m[1]) : null; };
  const pad = (n) => String(n).padStart(2, "0");
  const join = (xs) => (xs.length < 2 ? xs.join("") : xs.slice(0, -1).join(", ") + " " + t("tasks.cron.and") + " " + xs[xs.length - 1]);
  const anyDay = dom === "*" && mon === "*" && dow === "*";

  // Within the hour: every minute, every N minutes, once an hour.
  if (ho === "*" && anyDay) {
    if (mi === "*") return t("tasks.cron.everyMinute");
    if (every(mi)) return t("tasks.cron.everyMinutes", { n: every(mi) });
    if (num(mi) !== null) return num(mi) === 0 ? t("tasks.cron.hourly") : t("tasks.cron.hourlyAt", { m: pad(num(mi)) });
    return null;
  }
  if (every(ho) && num(mi) !== null && anyDay) return t("tasks.cron.everyHours", { n: every(ho), m: pad(num(mi)) });

  // At set times of the day: the minute is one value, the hours a few.
  const m = num(mi), hs = list(ho);
  if (m === null || !hs || hs.length > 4 || hs.some((x) => x > 23) || m > 59) return null;
  const time = join(hs.map((x) => `${pad(x)}:${pad(m)}`));
  if (anyDay) return t("tasks.cron.daily", { time });
  if (dom === "*" && mon === "*") {
    const days = list(dow, DOW);
    let ds = days;
    if (!ds) {
      const r = /^(\w+)-(\w+)$/.exec(dow);
      const a = r && num(r[1], DOW), b = r && num(r[2], DOW);
      if (a === null || b === null || a === undefined || b === undefined || a > b) return null;
      ds = [];
      for (let d = a; d <= b; d++) ds.push(d);
    }
    const set = [...new Set(ds.map((d) => d % 7))].sort();
    if (set.join() === "1,2,3,4,5") return t("tasks.cron.weekdays", { time });
    if (set.join() === "0,6") return t("tasks.cron.weekends", { time });
    if (set.length === 7) return t("tasks.cron.daily", { time });
    // 2024-01-07 was a Sunday: the day's name in the panel's language.
    const name = (d) => new Date(2024, 0, 7 + d).toLocaleDateString(currentLang(), { weekday: "long" });
    return t("tasks.cron.weekly", { days: join(set.map(name)), time });
  }
  if (dow === "*") {
    const days = list(dom);
    if (!days || days.some((d) => d < 1 || d > 31)) return null;
    if (mon === "*") return t("tasks.cron.monthly", { days: join(days.map(String)), time });
    const mo = num(mon, MON);
    if (mo === null || days.length !== 1 || mo < 1 || mo > 12) return null;
    const date = new Date(2024, mo - 1, days[0]).toLocaleDateString(currentLang(), { day: "numeric", month: "long" });
    return t("tasks.cron.yearly", { date, time });
  }
  return null;
}

// The server says how long a run took as a Go duration ("15.2ms",
// "1m3.5s"); read as a number it was NaN and the line showed "ok · ·".
function runLength(d) {
  if (d === undefined || d === null || d === "") return "";
  if (typeof d === "number") return fmtDuration(d);
  const unit = { h: 3600, m: 60, s: 1, ms: 1e-3, "µs": 1e-6, us: 1e-6, ns: 1e-9 };
  let secs = 0;
  let any = false;
  for (const [, n, u] of String(d).matchAll(/([\d.]+)(h|ms|µs|us|ns|m|s)/g)) { secs += Number(n) * unit[u]; any = true; }
  if (!any) return "";
  return secs < 1 ? t("tasks.underSecond") : fmtDuration(secs);
}
// A run's status in the reader's words; one the panel does not know yet
// shows as the server sent it.
function statusWord(st) {
  const k = "tasks.status." + st;
  const w = t(k);
  return w === k ? st : w;
}

export function tasksScreen() {
  const list = tasksList();
  const newBtn = h("button", { type: "button", class: "btn primary", "data-shortcut": "new" }, icon("plus"), t("tasks.new"));
  newBtn.addEventListener("click", () => list.edit(null));
  const p = page({ title: t("nav.tasks"), sub: t("tasks.sub"), icon: "clock", actions: newBtn });
  p.body.append(list.el);
  return { el: p.el, destroy() { list.destroy(); } };
}

// tasksList is the list of tasks with everything done to them: the house's
// every task, or, with site, only that site's, through the site's own
// routes (/sites/{domain}/tasks). Those routes only show and touch the
// site's tasks and put a new one in the site's folder whatever its JSON
// says, so the site's tab (and its agent, and a token scoped to it)
// manages its own work and nobody else's.
export function tasksList({ site = "" } = {}) {
  const base = site ? `/sites/${enc(site)}/tasks` : "/tasks";
  const body = h("div", { class: "card pad0 task-box" });
  const subs = [];

  // A task is a line: its title (the file's name under it), when it runs
  // in words with the expression beside it, where, and what happened last;
  // run now is the one button, the log and delete stay quiet. The list
  // used to lead with the file name ("backup") and the bare cron.
  async function render() {
    const tasks = await api.get(base);
    if (!tasks || tasks.length === 0) {
      return site ? empty(t("tasks.siteEmptyTitle"), t("tasks.siteEmptyHint", { domain: site }), h("button", { type: "button", class: "btn primary", onClick: () => edit(null) }, icon("plus"), t("tasks.new")), "clock")
        : empty(t("tasks.emptyTitle"), t("tasks.emptyHint"), h("button", { type: "button", class: "btn primary", onClick: () => edit(null) }, icon("plus"), t("tasks.new")), "clock");
    }
    const head = h("div", { class: "task-row task-cols", "aria-hidden": "true" },
      h("span", null, t("tasks.col.name")), h("span", null, t("tasks.col.schedule")), h("span", null, t("tasks.col.when")), h("span"));
    return h("div", { class: "task-list" }, head, ...tasks.map((task) => {
      const last = task.last || {};
      const st = last.status;
      const took = runLength(last.duration);
      const lastEl = st ? h("span", { class: "status", title: last.at ? fmtTime(last.at) : "" },
        h("span", { class: "dot " + (st === "ok" || st === "success" || st === "ended" ? "ok" : st === "running" || st === "skipped" ? "warn" : "bad") }),
        t("tasks.lastWas", { status: statusWord(st) }), took ? " · " + took : null,
        last.at ? " · " + fmtAgo(last.at, t) : null,
      ) : h("span", null, t("tasks.neverRan"));
      const words = cronWords(task.schedule);
      const title = task.title || task.name;
      return h("div", { class: "task-row" + (task.running ? " running" : "") },
        h("div", { class: "task-main" },
          h("a", { href: "#", class: "task-title", onClick: (e) => { e.preventDefault(); edit(task); } }, title),
          h("div", { class: "task-sub" },
            task.title ? h("code", null, task.name) : null,
            site ? null : task.site ? h("a", { href: `#/sites/${enc(task.site)}` }, task.site) : h("span", null, t("tasks.rootLong")))),
        h("div", { class: "task-when" },
          h("span", { class: "task-words" }, words || h("code", null, task.schedule || "")),
          h("span", { class: "task-cron" }, words ? h("code", { title: t("tasks.cronTitle") }, task.schedule) : null, task.timezone ? h("span", null, task.timezone) : null)),
        h("div", { class: "task-runs" },
          task.running ? h("span", { class: "status task-now" }, h("span", { class: "dot warn" }), t("tasks.runningNow"))
            : task.next ? h("span", { title: fmtTime(task.next) }, t("tasks.nextIn", { when: fmtAgo(task.next, t) })) : null,
          h("span", { class: "task-last" }, lastEl)),
        h("div", { class: "task-actions" }, task.running ? stopBtn(task) : runBtn(task), logBtn(task), delBtn(task)),
      );
    }));
  }

  function runBtn(task) {
    const b = h("button", { type: "button", class: "btn sm" }, icon("play"), t("tasks.run"));
    b.addEventListener("click", async () => {
      b.classList.add("busy");
      try { await api.post(`${base}/${enc(task.name)}/run`); toast(t("tasks.started", { name: task.name }), "ok"); }
      catch (e) { toastError(e); } finally { b.classList.remove("busy"); }
    });
    return b;
  }
  function stopBtn(task) {
    const b = h("button", { type: "button", class: "btn sm danger" }, icon("stop"), t("tasks.stop"));
    b.addEventListener("click", async () => {
      b.classList.add("busy");
      try { await api.post(`${base}/${enc(task.name)}/stop`); toast(t("tasks.stopped", { name: task.name }), "ok"); }
      catch (e) { toastError(e); } finally { b.classList.remove("busy"); }
    });
    return b;
  }
  function logBtn(task) {
    const b = h("button", { type: "button", class: "btn sm ghost" }, icon("list"), t("tasks.log"));
    b.addEventListener("click", async () => {
      const pre = h("pre", { class: "log", tabindex: "0" });
      const box = h("div", null, loading());
      const dlg = open({ title: t("tasks.logOf", { name: task.name }), body: box, wide: true, buttons: [{ label: t("common.close"), value: null }] });
      try {
        const text = await api.text(`${base}/${enc(task.name)}/log`);
        pre.textContent = text || t("tasks.logEmpty");
        clear(box).append(pre);
        pre.scrollTop = pre.scrollHeight;
      } catch (e) { clear(box).append(errorBox(e)); }
      await dlg;
    });
    return b;
  }
  function delBtn(task) {
    // Deleting is an icon alone, named for a screen reader and a pointer:
    // a third worded button beside "Run now" pulled the eye from it.
    const b = h("button", { type: "button", class: "icon-btn sm task-del", "aria-label": t("tasks.deleteNamed", { name: task.title || task.name }), title: t("common.delete") }, icon("trash"));
    b.addEventListener("click", async () => {
      if (!(await confirm({ title: t("tasks.deleteQ", { name: task.name }), body: t("tasks.deleteBody"), confirmLabel: t("common.delete"), danger: true }))) return;
      try { await api.del(`${base}/${enc(task.name)}`); toast(t("tasks.deleted", { name: task.name }), "ok"); load(body, render); } catch (e) { toastError(e); }
    });
    return b;
  }

  async function edit(task) {
    const name = h("input", { type: "text", class: "mono", value: task ? task.name : "", readOnly: Boolean(task), placeholder: "backup", pattern: "[a-z0-9-]+" });
    const { version, schedule, site: taskSite, command, timeout, overlap, ...rest } = task || {};
    const initial = task ? { version: version || 2, schedule, site: taskSite, command, timeout, overlap, ...rest } : { ...TEMPLATE, site };
    delete initial.name; delete initial.next; delete initial.last;
    const editor = jsonEditor({ value: initial, rows: 12 });
    editor.el.querySelector(".editor-bar").hidden = true;
    const done = await open({
      title: task ? t("tasks.editTitle", { name: task.name }) : t("tasks.newTitle"),
      wide: true,
      body: [field(t("tasks.name"), name, task ? null : t("tasks.nameHint")), editor.el, h("p", { class: "small muted" }, t(site ? "tasks.siteJsonHint" : "tasks.jsonHint", { domain: site }))],
      buttons: [{ label: t("common.cancel"), value: null }, { label: t("common.save"), submit: true, primary: true }],
      onSubmit: async (_f, showError) => {
        const n = name.value.trim();
        if (!/^[a-z0-9-]+$/.test(n)) { showError(t("tasks.badName")); return false; }
        const r = editor.parse();
        if (!r.ok) { showError(r.error.message); return false; }
        await api.put(`${base}/${enc(n)}`, r.value);
        return n;
      },
    });
    if (done) { toast(t("tasks.saved", { name: done }), "ok"); load(body, render); }
  }

  load(body, render);
  subs.push(onEvent("task.", () => { if (!document.querySelector("dialog[open]")) load(body, render); }));
  return { el: body, edit, refresh: () => load(body, render), destroy() { subs.forEach((u) => u()); } };
}

// The tasks of one site, in its own tab: the list above, with its own
// "New task" whose JSON already names the site.
export function siteTasksTab(domain) {
  const list = tasksList({ site: domain });
  const newBtn = h("button", { type: "button", class: "btn primary" }, icon("plus"), t("tasks.new"));
  newBtn.addEventListener("click", () => list.edit(null));
  const el = h("div", { class: "cards" },
    h("div", { class: "site-tasks-head" },
      h("div", null, h("h2", null, t("site.tab.tasks")), h("p", { class: "small muted" }, t("tasks.siteSub"))), newBtn),
    list.el);
  return { el, destroy() { list.destroy(); } };
}
