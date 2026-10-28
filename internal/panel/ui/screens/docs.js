// The house's documents, from the binary that runs (docs/ embedded), so
// what is read here is what this version does. /docs/api is the API's.

import { h, clear } from "../lib/dom.js";
import { t } from "../lib/i18n.js";
import { api, enc } from "../lib/api.js";
import { page, errorBox, loading } from "../components/page.js";
import { render } from "../lib/markdown.js";

export function docsScreen({ name }) {
  const current = name || "api";
  const p = page({ title: t("nav.docs"), icon: "file", sub: t("docs.sub") });
  const body = h("div", { class: "docs-wrap" });
  p.body.append(body);
  (async () => {
    body.append(loading());
    try {
      const [list, md] = await Promise.all([api.get("/docs"), api.text(`/docs/${enc(current)}`)]);
      const title = (d) => d.title.replace(/^middleware:?\s*/i, "").replace(/`/g, "") || d.name;
      const doc = render(md);
      const jump = (x, after) => (e) => {
        e.preventDefault();
        document.getElementById(x.id)?.scrollIntoView({ behavior: "smooth", block: "start" });
        if (after) after();
      };

      // A column beside the text on a wide screen: the documents, then
      // this one's headings. Eleven documents as tabs ran off the right
      // edge, and the contents sat over the text on a phone, twenty-two
      // links before the first paragraph.
      const docsNav = h("nav", { class: "docs-rail-group", "aria-label": t("docs.documents") },
        h("div", { class: "docs-rail-h" }, t("docs.documents")),
        ...list.map((d) => h("a", { href: `#/docs/${d.name}`, "aria-current": d.name === current ? "page" : null }, title(d))));
      const toc = doc.toc.length > 2 ? h("nav", { class: "docs-rail-group docs-toc2", "aria-label": t("docs.onThisPage") },
        h("div", { class: "docs-rail-h" }, t("docs.onThisPage")),
        ...doc.toc.map((x) => h("a", { href: "#", onClick: jump(x) }, x.text))) : null;
      const rail = h("aside", { class: "docs-rail" }, ...[docsNav, toc].filter(Boolean));

      // On a phone: which document as a list to pick from, and the
      // headings folded under it.
      const pick = h("select", { "aria-label": t("docs.document") }, ...list.map((d) => h("option", { value: d.name, selected: d.name === current }, title(d))));
      pick.addEventListener("change", () => { location.hash = `#/docs/${pick.value}`; });
      const fold = doc.toc.length > 2 ? h("details", { class: "docs-fold" }, h("summary", null, t("docs.onThisPage"))) : null;
      if (fold) fold.append(h("nav", { class: "docs-fold-list" }, ...doc.toc.map((x) => h("a", { href: "#", onClick: jump(x, () => { fold.open = false; }) }, x.text))));
      const mobile = h("div", { class: "docs-mobile" }, ...[pick, fold].filter(Boolean));

      clear(body).append(rail, h("div", { class: "docs-main" }, mobile, h("article", { class: "card docs-article" }, doc.el)));
    } catch (e) {
      clear(body).append(errorBox(e));
    }
  })();
  return { el: p.el, destroy() {} };
}
