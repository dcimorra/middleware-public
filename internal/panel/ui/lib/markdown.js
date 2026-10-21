// A small Markdown renderer for the house's own documents: headings,
// paragraphs, lists, tables, fenced code, inline code, bold, links. It
// builds DOM nodes and never sets innerHTML, so nothing in a document can
// run as markup, and the CSP has nothing to object to.

import { h } from "./dom.js";

// inline turns `code`, **bold** and [text](url) into nodes.
function inline(text) {
  const out = [];
  const re = /`([^`]+)`|\*\*([^*]+)\*\*|\[([^\]]+)\]\(([^)\s]+)\)|(?<![\w*])\*([^*\s](?:[^*]*[^*\s])?)\*(?![\w*])/g;
  let last = 0;
  let m;
  while ((m = re.exec(text))) {
    if (m.index > last) out.push(text.slice(last, m.index));
    if (m[1] !== undefined) out.push(h("code", null, m[1]));
    else if (m[2] !== undefined) out.push(h("strong", null, m[2]));
    else if (m[5] !== undefined) out.push(h("em", null, m[5]));
    else {
      const href = m[4];
      // Another document of this set opens here; anything else must be
      // http(s) and opens apart. No javascript:, no data:.
      const doc = /^([a-z-]+)\.md(#.*)?$/.exec(href);
      if (doc) out.push(h("a", { href: `#/docs/${doc[1]}` }, m[3]));
      else if (/^https?:\/\//.test(href)) out.push(h("a", { href, target: "_blank", rel: "noopener" }, m[3]));
      else out.push(m[3]);
    }
    last = re.lastIndex;
  }
  if (last < text.length) out.push(text.slice(last));
  return out;
}

const slug = (s) => s.toLowerCase().replace(/[`*]/g, "").replace(/[^a-z0-9]+/g, "-").replace(/^-|-$/g, "");

// render returns a container with the document, and its headings for a
// table of contents.
// breaks keeps single line breaks (a chat); documents are hard-wrapped
// and join their lines.
export function render(md, { breaks = false } = {}) {
  const root = h("div", { class: "md" });
  const toc = [];
  const lines = md.replace(/\r/g, "").split("\n");
  let i = 0;
  const isTableRow = (l) => /^\s*\|.*\|\s*$/.test(l);
  const cells = (l) => l.trim().replace(/^\||\|$/g, "").split(/(?<!\\)\|/).map((c) => c.trim().replace(/\\\|/g, "|"));
  while (i < lines.length) {
    const l = lines[i];
    if (/^```/.test(l)) {
      const body = [];
      for (i++; i < lines.length && !/^```/.test(lines[i]); i++) body.push(lines[i]);
      i++;
      root.append(h("pre", { class: "log" }, h("code", null, body.join("\n"))));
      continue;
    }
    const head = /^(#{1,4})\s+(.*)$/.exec(l);
    if (head) {
      const level = head[1].length;
      const id = slug(head[2]);
      root.append(h("h" + Math.min(level + 1, 5), { id }, ...inline(head[2])));
      if (level === 2) toc.push({ id, text: head[2].replace(/[`*]/g, "") });
      i++;
      continue;
    }
    if (isTableRow(l) && i + 1 < lines.length && /^\s*\|[\s:|-]+\|\s*$/.test(lines[i + 1])) {
      const th = cells(l);
      const rows = [];
      for (i += 2; i < lines.length && isTableRow(lines[i]); i++) rows.push(cells(lines[i]));
      root.append(h("div", { class: "table-wrap" }, h("table", { class: "table" },
        h("thead", null, h("tr", null, ...th.map((c) => h("th", null, ...inline(c))))),
        h("tbody", null, ...rows.map((r) => h("tr", null, ...r.map((c) => h("td", null, ...inline(c)))))))));
      continue;
    }
    if (/^\s*\d+[.)]\s+/.test(l)) {
      const items = [];
      while (i < lines.length && (/^\s*\d+[.)]\s+/.test(lines[i]) || (/^\s{2,}\S/.test(lines[i]) && items.length))) {
        if (/^\s*\d+[.)]\s+/.test(lines[i])) items.push(lines[i].replace(/^\s*\d+[.)]\s+/, ""));
        else items[items.length - 1] += " " + lines[i].trim();
        i++;
      }
      root.append(h("ol", null, ...items.map((t) => h("li", null, ...inline(t)))));
      continue;
    }
    if (/^\s*>\s?/.test(l)) {
      const quote = [];
      while (i < lines.length && /^\s*>\s?/.test(lines[i])) quote.push(lines[i++].replace(/^\s*>\s?/, ""));
      root.append(h("blockquote", null, render(quote.join("\n"), { breaks }).el));
      continue;
    }
    if (/^\s*([-*_])(\s*\1){2,}\s*$/.test(l)) { root.append(h("hr")); i++; continue; }
    if (/^\s*[-*]\s+/.test(l)) {
      const items = [];
      while (i < lines.length && (/^\s*[-*]\s+/.test(lines[i]) || (/^\s{2,}\S/.test(lines[i]) && items.length))) {
        if (/^\s*[-*]\s+/.test(lines[i])) items.push(lines[i].replace(/^\s*[-*]\s+/, ""));
        else items[items.length - 1] += " " + lines[i].trim();
        i++;
      }
      root.append(h("ul", null, ...items.map((t) => h("li", null, ...inline(t)))));
      continue;
    }
    if (l.trim() === "") { i++; continue; }
    const para = [];
    while (i < lines.length && lines[i].trim() !== "" && !/^(#{1,4}\s|```|\s*[-*]\s+|\s*\d+[.)]\s+|\s*>)/.test(lines[i]) && !isTableRow(lines[i])) para.push(lines[i++].trim());
    if (!breaks) { root.append(h("p", null, ...inline(para.join(" ")))); continue; }
    const kids = [];
    para.forEach((line, k) => { if (k) kids.push(h("br")); kids.push(...inline(line)); });
    root.append(h("p", null, ...kids));
  }
  return { el: root, toc };
}
