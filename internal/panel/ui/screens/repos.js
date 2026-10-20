// Repositories: the git repositories inside the sites, what each has
// unpushed, unpulled or uncommitted, and the mark that keeps one pulled.
// Pulling is not deploying, and the screen says so.

import { h, clear, fmtTime, fmtAgo } from "../lib/dom.js";
import { t } from "../lib/i18n.js";
import { api, enc } from "../lib/api.js";
import { onEvent } from "../lib/stream.js";
import { page, load, table, cell, empty } from "../components/page.js";
import { open, toggle } from "../components/dialog.js";
import { toast, toastError } from "../components/toast.js";
import { icon } from "../lib/icons.js";

export function reposScreen() {
  const p = page({ title: t("nav.repos"), sub: t("repos.sub"), icon: "branch" });
  const body = h("div", { class: "card pad0" });
  p.body.append(body);

  async function render() {
    const list = await api.get("/repos");
    if (!list.length) { clear(body).append(empty(t("repos.none"), t("repos.noneHint"), null, "branch")); return null; }
    const rows = list.map((r) => {
      const auto = toggle(t("repos.auto"), { checked: r.auto });
      auto.input.addEventListener("change", async () => {
        auto.input.disabled = true;
        try {
          const out = await api.put(`/repos/${enc(r.id)}/auto`, { enabled: auto.input.checked });
          toast(auto.input.checked ? t("repos.autoOn", { where: where(r) }) : t("repos.autoOff", { where: where(r) }), out.last && !out.last.ok ? "bad" : "ok");
          load(body, render);
        } catch (e) { auto.input.checked = !auto.input.checked; toastError(e); }
        finally { auto.input.disabled = false; }
      });
      const pull = h("button", { type: "button", class: "btn sm" }, icon("download"), t("repos.pull"));
      pull.addEventListener("click", async () => {
        pull.classList.add("busy");
        try {
          const out = await api.post(`/repos/${enc(r.id)}/pull`);
          toast(out.last ? out.last.message : t("repos.pulled"), out.last && !out.last.ok ? "bad" : "ok");
          load(body, render);
        } catch (e) { toastError(e); } finally { pull.classList.remove("busy"); }
      });
      const logBtn = h("button", { type: "button", class: "btn sm ghost" }, icon("list"), t("repos.log"));
      logBtn.addEventListener("click", () => showLog(r));
      const state = [];
      if (r.error) state.push(h("span", { class: "level-error" }, r.error));
      if (r.behind) state.push(h("span", { class: "badge warn" }, t("repos.behind", { n: r.behind })));
      if (r.ahead) state.push(h("span", { class: "badge" }, t("repos.ahead", { n: r.ahead })));
      if (r.dirty) state.push(h("span", { class: "badge warn" }, t("repos.dirty", { n: r.dirty })));
      if (!state.length && !r.error) state.push(h("span", { class: "muted small" }, r.upstream ? t("repos.clean") : t("repos.noUpstream")));
      const last = r.last ? h("div", { class: "small " + (r.last.ok ? "dim" : "level-error"), title: fmtTime(r.last.at) },
        r.last.message, " · ", fmtAgo(r.last.at, t), r.last.failures > 1 ? " · " + t("repos.failures", { n: r.last.failures }) : "") : null;
      return h("tr", { class: r.last && !r.last.ok ? "level-error" : "" },
        cell(h("div", null, h("a", { href: `#/sites/${enc(r.domain)}` }, r.domain), r.path !== "." ? h("span", { class: "mono small" }, " / " + r.path) : null,
          r.remote ? h("div", { class: "small dim mono" }, r.remote) : null), null, "primary"),
        cell(h("code", null, r.branch || "—"), t("repos.branch")),
        cell(r.head && r.head.hash ? h("div", null, h("code", { title: r.head.hash }, r.head.hash.slice(0, 7)), " ", r.head.subject,
          h("div", { class: "small dim" }, r.head.author, " · ", fmtAgo(r.head.at, t))) : "—", t("repos.head")),
        cell(h("div", { class: "stack-sm" }, h("div", { class: "actions" }, ...state), last), t("repos.state")),
        cell(h("div", { class: "actions" }, auto.el, pull, logBtn), null, "right nowrap"),
      );
    });
    clear(body).append(table([{ label: t("repos.where") }, { label: t("repos.branch") }, { label: t("repos.head") }, { label: t("repos.state") }, { label: "" }], rows, { responsive: true }).el);
    return null;
  }

  const where = (r) => r.domain + (r.path !== "." ? "/" + r.path : "");

  async function showLog(r) {
    try {
      const commits = await api.get(`/repos/${enc(r.id)}/log`);
      await open({
        title: t("repos.logOf", { where: where(r) }), wide: true,
        body: h("div", { class: "stack-sm small" }, ...commits.map((c) => h("div", null,
          h("code", { title: c.hash }, c.hash.slice(0, 7)), " ", c.subject, " ", h("span", { class: "dim" }, "· ", c.author, " · ", fmtTime(c.at))))),
      });
    } catch (e) { toastError(e); }
  }

  load(body, render);
  const unsub = onEvent("repo.", () => load(body, render));
  return { el: p.el, destroy() { unsub(); } };
}
