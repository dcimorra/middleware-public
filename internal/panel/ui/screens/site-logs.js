// Logs: the last lines, and a follow toggle that keeps a streaming GET
// open and appends as it comes. The stream is aborted when the tab goes
// away so a forgotten tab does not keep a reader on the file.

import { h } from "../lib/dom.js";
import { t } from "../lib/i18n.js";
import { api, enc, qs } from "../lib/api.js";
import { store } from "../lib/store.js";
import { toggle } from "../components/dialog.js";

const MAX_CHARS = 400000;

export function logsTab(domain) {
  const s = store.site(domain);
  const workers = Object.keys((s.manifest && s.manifest.workers) || {});
  const pre = h("pre", { class: "log", tabindex: "0", "aria-live": "off", "aria-label": t("logs.label") });
  const worker = h("select", { "aria-label": t("logs.worker") }, h("option", { value: "" }, t("logs.main")), ...workers.map((w) => h("option", { value: w }, w)));
  const lines = h("select", { "aria-label": t("logs.lines") }, ...[100, 200, 500, 2000].map((n) => h("option", { value: String(n), selected: n === 200 }, t("logs.lastN", { n }))));
  const follow = toggle(t("logs.follow"), {});
  const reload = h("button", { type: "button", class: "btn sm" }, t("common.reload"));
  const clearBtn = h("button", { type: "button", class: "btn sm ghost" }, t("logs.clear"));
  const status = h("span", { class: "small muted", role: "status" });
  const el = h("div", null,
    h("div", { class: "log-bar" }, workers.length ? worker : null, lines, reload, clearBtn, follow.el, h("span", { class: "grow" }), status),
    pre,
  );
  let ctl = null;
  let stick = true;

  function append(text) {
    pre.append(document.createTextNode(text));
    if (pre.textContent.length > MAX_CHARS) {
      const keep = pre.textContent.slice(-MAX_CHARS);
      pre.textContent = keep;
    }
    if (stick) pre.scrollTop = pre.scrollHeight;
  }
  pre.addEventListener("scroll", () => {
    stick = pre.scrollHeight - pre.scrollTop - pre.clientHeight < 24;
  });

  async function fetchTail() {
    stop();
    status.textContent = t("common.loading");
    try {
      const text = await api.text(`/sites/${enc(domain)}/logs` + qs({ lines: lines.value, worker: worker.value }));
      pre.textContent = text || "";
      if (!text) pre.textContent = t("logs.empty");
      status.textContent = "";
      pre.scrollTop = pre.scrollHeight;
    } catch (e) {
      pre.textContent = "";
      status.textContent = e.message;
    }
    if (follow.input.checked) start();
  }

  function start() {
    stop();
    ctl = new AbortController();
    status.textContent = t("logs.following");
    api.stream(`/sites/${enc(domain)}/logs` + qs({ follow: 1, lines: 0, worker: worker.value }), append, ctl.signal)
      .then(() => { if (ctl) status.textContent = t("logs.ended"); })
      .catch((e) => { if (e.name !== "AbortError") status.textContent = e.message; });
  }
  function stop() {
    if (ctl) { ctl.abort(); ctl = null; }
    if (!follow.input.checked) status.textContent = "";
  }

  follow.input.addEventListener("change", () => { if (follow.input.checked) start(); else stop(); });
  worker.addEventListener("change", fetchTail);
  lines.addEventListener("change", fetchTail);
  reload.addEventListener("click", fetchTail);
  clearBtn.addEventListener("click", () => { pre.textContent = ""; });
  fetchTail();

  return { el, destroy() { follow.input.checked = false; stop(); } };
}
