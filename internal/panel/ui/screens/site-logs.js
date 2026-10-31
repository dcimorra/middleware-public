// Logs: the last lines, and a follow toggle that keeps a streaming GET
// open and appends as it comes. The stream is aborted when the tab goes
// away so a forgotten tab does not keep a reader on the file.

import { h } from "../lib/dom.js";
import { icon } from "../lib/icons.js";
import { t } from "../lib/i18n.js";
import { api, enc, qs } from "../lib/api.js";
import { store } from "../lib/store.js";
import { toggle } from "../components/dialog.js";

const MAX_CHARS = 400000;

export function logsTab(domain, pick = "") {
  const s = store.site(domain);
  const m = s.manifest || {};
  const workers = Object.keys(m.workers || {});
  const services = Object.keys(m.services || {}).sort();
  const pre = h("pre", { class: "log site-log", tabindex: "0", "aria-live": "off", "aria-label": t("logs.label") });
  // One picker for every log the site has: a service's (and its image's
  // build, when it is built) by a prefix, a worker's by its name, as
  // before services.
  const choices = [];
  for (const n of services) {
    choices.push(["service:" + n, n]);
    const run = (m.services[n] || {}).run || {};
    if (run.runtime === "docker" && run.docker && run.docker.build) choices.push(["build:" + n, t("logs.build", { name: n })]);
  }
  for (const w of workers) choices.push([w, w]);
  const worker = h("select", { "aria-label": t("logs.worker") },
    services.length ? null : h("option", { value: "" }, t("logs.main")),
    ...choices.map(([v, label]) => h("option", { value: v, selected: v === pick }, label)));
  const params = () => {
    const v = worker.value;
    if (v.startsWith("service:")) return { service: v.slice(8) };
    if (v.startsWith("build:")) return { service: v.slice(6), build: 1 };
    return { worker: v };
  };
  const lines = h("select", { "aria-label": t("logs.lines") }, ...[100, 200, 500, 2000].map((n) => h("option", { value: String(n), selected: n === 200 }, t("logs.lastN", { n }))));
  const follow = toggle(t("logs.follow"), {});
  const reload = h("button", { type: "button", class: "btn sm" }, icon("refresh"), t("common.reload"));
  const clearBtn = h("button", { type: "button", class: "btn sm ghost" }, icon("trash"), t("logs.clear"));
  const status = h("span", { class: "small muted", role: "status" });
  const el = h("div", null,
    h("div", { class: "log-bar site-logs-bar" }, choices.length ? worker : null, lines, reload, clearBtn, follow.el, h("span", { class: "grow" }), status),
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
      const text = await api.text(`/sites/${enc(domain)}/logs` + qs({ lines: lines.value, ...params() }));
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
    api.stream(`/sites/${enc(domain)}/logs` + qs({ follow: 1, lines: 0, ...params() }), append, ctl.signal)
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
