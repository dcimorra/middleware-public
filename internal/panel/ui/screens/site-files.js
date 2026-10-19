// Files: a browser inside the site folder, a read-only viewer and an edit
// mode for text. The API refuses paths outside the folder; the panel
// still never builds a path from anything but what the API listed, so a
// ".." never leaves here in the first place.

import { h, clear, fmtBytes, fmtTime } from "../lib/dom.js";
import { t } from "../lib/i18n.js";
import { api, enc, qs } from "../lib/api.js";
import { loading, errorBox, empty } from "../components/page.js";
import { toast, toastError } from "../components/toast.js";
import { confirm } from "../components/dialog.js";

const TEXT_EXT = /\.(txt|md|json|js|mjs|cjs|ts|tsx|jsx|css|scss|html|htm|xml|yml|yaml|toml|ini|env|sh|bash|go|py|rb|rs|php|java|c|h|cpp|sql|csv|log|conf|cfg|lock|gitignore|dockerfile|service|svg)$/i;
const MAX_VIEW = 2 * 1024 * 1024;

export function filesTab(domain) {
  const listBox = h("div", { class: "card pad0" });
  const viewBox = h("div", { class: "card file-view" });
  const el = h("div", { class: "files" }, listBox, viewBox);
  let dir = "/";
  let current = null;

  async function browse(path) {
    dir = path;
    clear(listBox).append(loading());
    try {
      const items = await api.get(`/sites/${enc(domain)}/files` + qs({ path }));
      clear(listBox);
      const head = h("div", { class: "log-bar", "aria-label": t("files.path") }, h("span", { class: "path grow" }, path));
      listBox.append(h("div", { class: "card-head" }, head));
      const ul = h("ul", { class: "file-list" });
      if (path !== "/") {
        const up = path.replace(/\/[^/]+\/?$/, "") || "/";
        ul.append(h("li", null, h("button", { type: "button", onClick: () => browse(up) }, h("span", { class: "ico" }, "↑"), "..")));
      }
      const sorted = [...(items || [])].sort((a, b) => (b.dir - a.dir) || a.name.localeCompare(b.name));
      for (const it of sorted) {
        const full = (path.endsWith("/") ? path : path + "/") + it.name;
        ul.append(h("li", null, h("button", { type: "button", onClick: () => it.dir ? browse(full) : view(full, it) },
          h("span", { class: "ico", "aria-hidden": "true" }, it.dir ? "▸" : "·"),
          h("span", { class: "truncate" }, it.name),
          it.dir ? null : h("span", { class: "size" }, fmtBytes(it.size)),
        )));
      }
      if (sorted.length === 0) ul.append(h("li", { class: "muted small" }, h("span", { class: "empty" }, t("files.emptyDir"))));
      listBox.append(ul);
    } catch (e) {
      clear(listBox).append(errorBox(e));
    }
  }

  async function view(path, meta) {
    current = path;
    clear(viewBox).append(loading());
    const isText = TEXT_EXT.test(path) || !/\.[a-z0-9]+$/i.test(path);
    if ((meta && meta.size > MAX_VIEW)) {
      clear(viewBox).append(h("div", { class: "path" }, path), empty(t("files.tooBig"), t("files.tooBigHint", { size: fmtBytes(meta.size) })));
      return;
    }
    try {
      const text = await api.text(`/sites/${enc(domain)}/file` + qs({ path }));
      const binary = !isText && /[\x00-\x08\x0E-\x1F]/.test(text.slice(0, 4000));
      clear(viewBox);
      const editBtn = h("button", { type: "button", class: "btn sm", hidden: binary }, t("files.edit"));
      const bar = h("div", { class: "log-bar" }, h("span", { class: "path grow" }, path), meta ? h("span", { class: "small muted" }, fmtBytes(meta.size), " · ", fmtTime(meta.mtime)) : null, editBtn);
      const pre = h("pre", { class: "log", tabindex: "0" }, binary ? t("files.binary") : (text || t("files.emptyFile")));
      viewBox.append(bar, pre);
      editBtn.addEventListener("click", () => edit(path, text, meta));
    } catch (e) {
      clear(viewBox).append(errorBox(e));
    }
  }

  function edit(path, text, meta) {
    clear(viewBox);
    const ta = h("textarea", { class: "mono", spellcheck: "false", "aria-label": path });
    ta.value = text;
    const save = h("button", { type: "button", class: "btn primary sm" }, t("common.save"));
    const cancel = h("button", { type: "button", class: "btn sm" }, t("common.cancel"));
    const bar = h("div", { class: "log-bar" }, h("span", { class: "path grow" }, path), cancel, save);
    viewBox.append(bar, ta);
    ta.addEventListener("keydown", (e) => { if ((e.ctrlKey || e.metaKey) && e.key === "s") { e.preventDefault(); save.click(); } });
    cancel.addEventListener("click", async () => {
      if (ta.value !== text && !(await confirm({ title: t("files.discardQ"), body: t("files.discardBody"), confirmLabel: t("files.discard"), danger: true }))) return;
      view(path, meta);
    });
    save.addEventListener("click", async () => {
      save.classList.add("busy");
      try {
        await api.putRaw(`/sites/${enc(domain)}/file` + qs({ path }), ta.value, "text/plain; charset=utf-8");
        toast(t("files.saved", { path }), "ok");
        view(path, meta ? { ...meta, size: new Blob([ta.value]).size, mtime: new Date().toISOString() } : null);
        browse(dir);
      } catch (e) { toastError(e); } finally { save.classList.remove("busy"); }
    });
    ta.focus();
  }

  clear(viewBox).append(empty(t("files.pick"), t("files.pickHint")));
  browse("/");
  return { el, destroy() { current = null; } };
}
