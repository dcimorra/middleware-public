// Repositories: the git repositories inside the sites, what each has
// unpushed, unpulled or uncommitted, and the mark that keeps one pulled.
// Pulling is not deploying, and the screen says so.
//
// One repository is one row of sentences: where it is and on which
// branch, what state it is in, its last commit, then its three controls.
// A failed pull turned the whole table row red, the commit message and
// the branch included; now only the line that says what failed is.

import { h, clear, fmtTime, fmtAgo } from "../lib/dom.js";
import { t } from "../lib/i18n.js";
import { api, enc } from "../lib/api.js";
import { onEvent } from "../lib/stream.js";
import { page, load, empty } from "../components/page.js";
import { open, toggle } from "../components/dialog.js";
import { toast, toastError } from "../components/toast.js";
import { icon } from "../lib/icons.js";

export function reposScreen() {
  const p = page({ title: t("nav.repos"), sub: t("repos.sub"), icon: "branch" });
  p.el.classList.add("np-page");
  const body = h("div", { class: "card pad0" });
  p.body.append(body);

  async function render() {
    const list = await api.get("/repos");
    if (!list.length) {
      const e = empty(t("repos.none"), t("repos.noneHint"), null, "branch");
      e.classList.add("np-empty");
      clear(body).append(e);
      return null;
    }
    const items = list.map((r) => {
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
      const pull = h("button", { type: "button", class: "btn sm" + (r.behind ? " primary" : "") }, icon("download"), t("repos.pull"));
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
      if (r.behind) state.push(h("span", { class: "badge warn" }, t("repos.behind", { n: r.behind })));
      if (r.ahead) state.push(h("span", { class: "badge info" }, t("repos.ahead", { n: r.ahead })));
      if (r.dirty) state.push(h("span", { class: "badge warn" }, t("repos.dirty", { n: r.dirty })));
      if (!state.length && !r.error) state.push(h("span", { class: r.upstream ? "np-clean small" : "muted small" }, r.upstream ? t("repos.clean") : t("repos.noUpstream")));
      const lines = [];
      if (r.error) lines.push(h("div", { class: "np-repo-line level-error" }, r.error));
      if (r.last) {
        lines.push(h("div", { class: "np-repo-line " + (r.last.ok ? "np-quiet" : "level-error"), title: fmtTime(r.last.at) },
          r.last.message, " · ", fmtAgo(r.last.at, t), r.last.failures > 1 ? " · " + t("repos.failures", { n: r.last.failures }) : ""));
      }
      return h("li", { class: "np-repo" + (r.last && !r.last.ok ? " failing" : "") },
        h("div", { class: "np-repo-main" },
          h("div", { class: "np-repo-title" },
            h("a", { href: `#/sites/${enc(r.domain)}` }, r.domain), r.path !== "." ? h("span", { class: "mono np-quiet" }, "/ " + r.path) : null,
            h("span", { class: "np-branch" }, icon("branch", { size: 13 }), r.branch || "—"),
            ...state),
          r.head && r.head.hash ? h("div", { class: "np-repo-commit" }, h("code", { title: r.head.hash }, r.head.hash.slice(0, 7)), " ", r.head.subject,
            h("span", { class: "np-quiet" }, " · ", r.head.author, " · ", fmtAgo(r.head.at, t))) : null,
          r.remote ? h("div", { class: "np-repo-remote mono" }, r.remote) : null,
          ...lines),
        h("div", { class: "np-repo-side" }, auto.el, h("div", { class: "np-row-actions" }, logBtn, pull)));
    });
    clear(body).append(h("ul", { class: "np-list" }, ...items));
    return null;
  }

  const where = (r) => r.domain + (r.path !== "." ? "/" + r.path : "");

  async function showLog(r) {
    try {
      const commits = await api.get(`/repos/${enc(r.id)}/log`);
      await open({
        title: t("repos.logOf", { where: where(r) }), wide: true,
        body: h("ul", { class: "np-commits" }, ...commits.map((c) => h("li", null,
          h("code", { title: c.hash }, c.hash.slice(0, 7)), h("span", null, c.subject, h("span", { class: "np-quiet" }, " · ", c.author, " · ", fmtTime(c.at)))))),
      });
    } catch (e) { toastError(e); }
  }

  load(body, render);
  const unsub = onEvent("repo.", () => load(body, render));
  return { el: p.el, destroy() { unsub(); } };
}
