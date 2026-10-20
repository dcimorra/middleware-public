// Notifications on the operator's devices: subscribe this browser, choose
// what reaches it, see every device and whether its last push arrived.

import { h, clear, fmtTime, fmtAgo } from "../lib/dom.js";
import { t } from "../lib/i18n.js";
import { api } from "../lib/api.js";
import { table, cell, empty, load, notice } from "../components/page.js";
import { checkbox, confirm } from "../components/dialog.js";
import { toast, toastError } from "../components/toast.js";
import { icon } from "../lib/icons.js";

const toBuf = (s) => Uint8Array.from(atob(s.replace(/-/g, "+").replace(/_/g, "/") + "===".slice((s.length + 3) % 4)), (c) => c.charCodeAt(0));

function supported() {
  return "serviceWorker" in navigator && "PushManager" in window && window.isSecureContext;
}

export function pushCard() {
  const el = h("div", { class: "card pad0" });
  async function render() {
    const info = await api.get("/push");
    let mine = null;
    if (supported()) {
      const reg = await navigator.serviceWorker.getRegistration("/");
      mine = reg ? await reg.pushManager.getSubscription() : null;
    }
    const kinds = info.kinds.map((k) => ({ k, box: checkbox(t("push.kind." + k), { checked: true }) }));
    const current = mine && info.devices.find((d) => d.endpoint === mine.endpoint);
    if (current && current.kinds && current.kinds.length) kinds.forEach(({ k, box }) => { box.input.checked = current.kinds.includes(k); });

    const on = h("button", { type: "button", class: "btn sm primary" }, icon("bell"), current ? t("push.update") : t("push.subscribe"));
    on.disabled = !supported();
    on.addEventListener("click", async () => {
      on.classList.add("busy");
      try {
        const reg = await navigator.serviceWorker.ready;
        const sub = mine || await reg.pushManager.subscribe({ userVisibleOnly: true, applicationServerKey: toBuf(info.public_key) });
        const chosen = kinds.filter(({ box }) => box.input.checked).map(({ k }) => k);
        await api.post("/push/subscriptions", { ...sub.toJSON(), expirationTime: undefined, name: navigator.userAgent.split(") ")[0].split("(")[1] || "browser", kinds: chosen.length === kinds.length ? [] : chosen });
        toast(t("push.subscribed"), "ok");
        load(el, render);
      } catch (e) { toastError(e.name === "NotAllowedError" ? new Error(t("push.denied")) : e); }
      finally { on.classList.remove("busy"); }
    });
    const test = h("button", { type: "button", class: "btn sm" }, t("push.test"));
    test.disabled = !info.devices.length;
    test.addEventListener("click", async () => {
      try { const r = await api.post("/push/test"); toast(r.error ? r.error : t("push.testSent", { n: r.sent }), r.error ? "bad" : "ok"); load(el, render); } catch (e) { toastError(e); }
    });
    const rows = info.devices.map((d) => {
      const rm = h("button", { type: "button", class: "btn sm danger ghost" }, t("push.remove"));
      rm.addEventListener("click", async () => {
        if (!(await confirm({ title: t("push.removeQ"), body: d.service, confirmLabel: t("push.remove"), danger: true }))) return;
        try {
          if (mine && mine.endpoint === d.endpoint) await mine.unsubscribe();
          await api.del("/push/subscriptions?endpoint=" + encodeURIComponent(d.endpoint));
          load(el, render);
        } catch (e) { toastError(e); }
      });
      return h("tr", null,
        cell(h("div", null, d.name || d.service, mine && mine.endpoint === d.endpoint ? h("span", { class: "badge ok" }, " " + t("push.thisDevice")) : null), null, "primary"),
        cell(h("code", { class: "small" }, d.service), t("push.service")),
        cell((d.kinds && d.kinds.length ? d.kinds.map((k) => t("push.kind." + k)).join(", ") : t("push.all")), t("push.kinds")),
        cell(d.last_error ? h("span", { class: "level-error small" }, d.last_error) : d.last_sent ? h("span", { title: fmtTime(d.last_sent) }, fmtAgo(d.last_sent, t)) : h("span", { class: "muted" }, t("settings.neverUsed")), t("push.last")),
        cell(rm, null, "right"));
    });
    clear(el).append(
      h("div", { class: "log-bar card-pad" }, h("h2", { class: "grow" }, t("push.title")), test, on),
      h("div", { class: "card-pad" },
        h("p", { class: "small muted" }, supported() ? t("push.hint") : t("push.unsupported")),
        h("div", { class: "form-row" }, ...kinds.map(({ box }) => box.el))),
      rows.length ? table([{ label: t("push.device") }, { label: t("push.service") }, { label: t("push.kinds") }, { label: t("push.last") }, { label: "" }], rows, { responsive: true }).el
        : empty(t("push.none"), t("push.noneHint"), null, "bell"),
    );
    return null;
  }
  load(el, render);
  return el;
}
