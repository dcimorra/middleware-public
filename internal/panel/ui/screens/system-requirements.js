// The house's requirements, installed from here: the packages, docker and
// its group, nvm with the LTS, fail2ban and, when asked, a headless
// Chromium (POST /system/requirements; the helper runs it as root in a unit
// of its own). Followed by its log, which the server keeps, so it is read
// again after a reload or with the tab closed meanwhile.

import { icon } from "../lib/icons.js";
import { h, clear, fmtAgo } from "../lib/dom.js";
import { t } from "../lib/i18n.js";
import { api } from "../lib/api.js";
import { confirm, checkbox } from "../components/dialog.js";
import { toast, toastError } from "../components/toast.js";

const BADGE = { running: "info", finished: "ok", restart: "warn", failed: "bad", interrupted: "bad" };

export function requirementsCard() {
  const el = h("div", { class: "card" });
  const browser = checkbox(t("req.browser"));
  const runBtn = h("button", { type: "button", class: "btn primary" }, icon("download"), t("req.run"));
  const logBtn = h("button", { type: "button", class: "btn" }, icon("list"), t("req.seeLog"));
  const restartBtn = h("button", { type: "button", class: "btn", hidden: true }, icon("power"), t("req.restart"));
  const stateEl = h("div", { class: "row" });
  el.append(h("h2", null, t("req.title")), h("p", { class: "small muted" }, t("req.hint")), stateEl, browser.el, h("div", { class: "row" }, runBtn, logBtn, restartBtn));
  let st = { state: "none" };
  let dialog = null;
  let timer = null;

  function paint() {
    clear(stateEl);
    if (st.state !== "none") {
      stateEl.append(h("span", { class: "badge " + (BADGE[st.state] || "") }, t("req.state." + st.state)),
        st.updated ? h("span", { class: "small muted" }, fmtAgo(st.updated, t)) : null,
        st.error ? h("span", { class: "small level-error" }, st.error) : null);
    }
    runBtn.disabled = st.state === "running";
    logBtn.hidden = st.state === "none";
    restartBtn.hidden = st.state !== "restart";
    if (dialog) dialog.paint();
  }
  async function load() {
    try { st = await api.get("/system/requirements"); } catch { return; }
    paint();
    clearTimeout(timer);
    if (st.state === "running") timer = setTimeout(load, 2000);
  }

  function openLog() {
    const pre = h("pre", { class: "req-log" });
    const badge = h("span", { class: "badge" });
    const close = h("button", { type: "button", class: "btn" }, t("moves.hide"));
    const dlg = h("dialog", { class: "wide" }, h("div", { class: "dlg" },
      h("div", { class: "dlg-head" }, h("h2", null, t("req.title")), badge),
      h("div", { class: "dlg-body" }, pre),
      h("div", { class: "dlg-foot" }, h("span", { class: "small muted grow" }, t("req.background")), close)));
    document.body.append(dlg);
    const end = () => { dialog = null; dlg.close(); dlg.remove(); };
    close.addEventListener("click", end);
    dlg.addEventListener("cancel", (e) => { e.preventDefault(); end(); });
    dialog = {
      paint() {
        const atEnd = pre.scrollHeight - pre.scrollTop - pre.clientHeight < 40;
        pre.textContent = st.log || t("req.starting");
        if (atEnd) pre.scrollTop = pre.scrollHeight;
        badge.className = "badge " + (BADGE[st.state] || "info");
        badge.textContent = t("req.state." + (st.state === "none" ? "running" : st.state));
      },
    };
    dlg.showModal();
    dialog.paint();
    pre.scrollTop = pre.scrollHeight;
  }

  runBtn.addEventListener("click", async () => {
    if (!(await confirm({ title: t("req.runQ"), body: t("req.runBody"), confirmLabel: t("req.run") }))) return;
    try {
      st = await api.post("/system/requirements", { browser: browser.input.checked });
    } catch (e) { toastError(e); return; }
    openLog();
    paint();
    load();
  });
  logBtn.addEventListener("click", openLog);
  restartBtn.addEventListener("click", async () => {
    if (!(await confirm({ title: t("system.restartQ"), body: t("system.restartBody"), confirmLabel: t("system.restart"), danger: true }))) return;
    try { await api.post("/system/restart"); toast(t("system.restarting"), "ok"); } catch (e) { toastError(e); }
  });
  load();
  return { el, destroy() { clearTimeout(timer); } };
}
