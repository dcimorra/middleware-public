// The house's documents, from the binary that runs (docs/ embedded), so
// what is read here is what this version does. /docs/api is the API's.

import { h, clear } from "../lib/dom.js";
import { t } from "../lib/i18n.js";
import { api, enc } from "../lib/api.js";
import { page, errorBox, loading } from "../components/page.js";
import { tabs } from "../components/tabs.js";
import { render } from "../lib/markdown.js";

export function docsScreen({ name }) {
  const current = name || "api";
  const p = page({ title: t("nav.docs"), icon: "file", sub: t("docs.sub") });
  const nav = h("div");
  const body = h("div", { class: "docs-layout" });
  p.body.append(nav, body);
  (async () => {
    body.append(loading());
    try {
      const [list, md] = await Promise.all([api.get("/docs"), api.text(`/docs/${enc(current)}`)]);
      clear(nav).append(tabs(list.map((d) => ({ key: d.name, href: `#/docs/${d.name}`, label: d.title.replace(/^middleware:?\s*/i, "").replace(/`/g, "") || d.name })), current));
      const doc = render(md);
      const toc = h("nav", { class: "docs-toc", "aria-label": t("docs.contents") }, h("strong", null, t("docs.contents")),
        ...doc.toc.map((x) => h("a", { href: "#", onClick: (e) => { e.preventDefault(); document.getElementById(x.id)?.scrollIntoView(); } }, x.text)));
      clear(body).append(doc.toc.length > 2 ? toc : h("div"), h("div", { class: "card" }, doc.el));
    } catch (e) {
      clear(body).append(errorBox(e));
    }
  })();
  return { el: p.el, destroy() {} };
}
