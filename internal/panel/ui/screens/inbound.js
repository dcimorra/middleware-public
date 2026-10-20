// Inbound: the two doors and which one each domain comes in by. A door is
// toggled through POST /inbound/doors/{door}. Moving domains shows the
// verification lines as a checklist as they come, from the
// response and from inbound.* events for the same domains.

import { icon } from "../lib/icons.js";
import { h, clear, fmtTime, fmtAgo } from "../lib/dom.js";
import { t } from "../lib/i18n.js";
import { api, enc } from "../lib/api.js";
import { onEvent } from "../lib/stream.js";
import { page, load, table, cell, empty } from "../components/page.js";
import { checklist } from "../components/checklist.js";
import { open, confirm, toggle, field } from "../components/dialog.js";
import { toast, toastError } from "../components/toast.js";

export function inboundScreen() {
  const p = page({ title: t("nav.inbound"), sub: t("inbound.sub"), icon: "door" });
  const body = h("div");
  p.body.append(body);
  const subs = [];
  let selected = new Set();

  async function render() {
    const data = await api.get("/inbound");
    const doors = data.doors || {};
    const domains = data.domains || [];
    let certs = [];
    try { certs = await api.get("/inbound/certificates"); } catch { certs = []; }
    const certBy = new Map((certs || []).map((c) => [c.domain, c]));

    const doorCards = h("div", { class: "grid-2" }, doorCard("tunnel", doors.tunnel || {}), doorCard("ports", doors.ports || {}));

    const moveBtn = h("button", { type: "button", class: "btn", disabled: true }, t("inbound.move"));
    const all = h("input", { type: "checkbox", "aria-label": t("inbound.selectAll") });
    const rows = domains.map((d) => {
      const cb = h("input", { type: "checkbox", "aria-label": d.domain, checked: selected.has(d.domain) });
      cb.addEventListener("change", () => { if (cb.checked) selected.add(d.domain); else selected.delete(d.domain); moveBtn.disabled = selected.size === 0; });
      const cert = certBy.get(d.domain);
      const exp = d.cert_expires || (cert && cert.expires);
      const daysLeft = exp ? Math.round((new Date(exp) - Date.now()) / 86400000) : null;
      return h("tr", null,
        h("td", null, cb),
        cell(h("a", { class: "cell-link", href: `#/sites/${enc(d.domain)}` }, d.domain), null, "primary"),
        cell(doorBadge(d.door), t("inbound.col.door")),
        cell(dnsBadge(d.dns), t("inbound.col.dns")),
        cell(exp ? h("span", { class: "status", title: fmtTime(exp) },
          h("span", { class: "dot " + (daysLeft < 7 ? "bad" : daysLeft < 21 ? "warn" : "ok") }),
          daysLeft < 0 ? t("inbound.certExpired") : t("inbound.certDays", { n: daysLeft }),
        ) : h("span", { class: "muted" }, "—"), t("inbound.col.cert")),
        cell(d.door === "ports" ? renewBtn(d.domain) : (d.tunnel_route ? h("code", { class: "small" }, d.tunnel_route) : ""), null, "right"),
      );
    });
    all.addEventListener("change", () => {
      selected = all.checked ? new Set(domains.map((d) => d.domain)) : new Set();
      for (const cb of body.querySelectorAll("tbody input[type=checkbox]")) cb.checked = all.checked;
      moveBtn.disabled = selected.size === 0;
    });
    moveBtn.disabled = selected.size === 0;
    moveBtn.addEventListener("click", move);
    const tbl = domains.length ? table([
      { label: "" }, { label: t("inbound.col.domain") }, { label: t("inbound.col.door") }, { label: t("inbound.col.dns") }, { label: t("inbound.col.cert") }, { label: "" },
    ], rows, { responsive: true }).el : empty(t("inbound.emptyTitle"), t("inbound.emptyHint"));
    const thCb = tbl.querySelector("th");
    if (thCb) { clear(thCb).append(all); }

    clear(body).append(
      doorCards,
      h("div", { class: "card pad0" },
        h("div", { class: "log-bar card-pad" }, h("h2", { class: "grow" }, t("inbound.domains")), moveBtn),
        tbl,
      ),
    );
  }

  function doorCard(key, door) {
    const sw = toggle(door.open ? t("inbound.open") : t("inbound.closed"), { checked: Boolean(door.open) });
    const lines = checklist();
    sw.input.addEventListener("change", async () => {
      const opening = sw.input.checked;
      if (!opening && !(await confirm({ title: t("inbound.closeQ", { door: t("inbound.door." + key) }), body: t("inbound.closeBody"), confirmLabel: t("inbound.close"), danger: true }))) {
        sw.input.checked = true;
        return;
      }
      sw.input.disabled = true;
      try {
        const r = await api.post(`/inbound/doors/${key}`, { open: opening });
        lines.set(r && r.lines);
        if (r && r.ok === false) { sw.input.checked = !opening; toast(t("inbound.doorFailed"), "bad"); }
        else toast(opening ? t("inbound.opened", { door: t("inbound.door." + key) }) : t("inbound.closedNow", { door: t("inbound.door." + key) }), "ok");
        sw.el.lastChild.textContent = sw.input.checked ? t("inbound.open") : t("inbound.closed");
      } catch (e) {
        sw.input.checked = !opening;
        toastError(e);
      } finally { sw.input.disabled = false; }
    });
    const details = key === "ports"
      ? h("p", { class: "small muted" }, t("inbound.portsDetail", { http: door.http ?? 80, https: door.https ?? 443 }))
      : h("p", { class: "small muted" }, t("inbound.tunnelDetail"));
    return h("div", { class: "card" }, h("h2", null, t("inbound.door." + key)), details, sw.el, lines.el);
  }

  function doorBadge(door) {
    if (door === "tunnel") return h("span", { class: "badge info" }, t("inbound.door.tunnel"));
    if (door === "ports") return h("span", { class: "badge accent" }, t("inbound.door.ports"));
    return h("span", { class: "badge warn" }, t("inbound.door.none"));
  }
  function dnsBadge(dns) {
    if (dns === true || dns === "ok") return h("span", { class: "badge ok" }, t("inbound.dnsOk"));
    if (dns === false || dns === "missing") return h("span", { class: "badge bad" }, t("inbound.dnsMissing"));
    if (!dns) return h("span", { class: "badge" }, t("inbound.dnsUnknown"));
    return h("span", { class: "badge" }, String(dns));
  }
  function renewBtn(domain) {
    const b = h("button", { type: "button", class: "btn sm" }, icon("refresh"), t("inbound.renew"));
    b.addEventListener("click", async () => {
      b.classList.add("busy");
      try { await api.post(`/inbound/certificates/${enc(domain)}/renew`); toast(t("inbound.renewStarted", { domain }), "ok"); }
      catch (e) { toastError(e); } finally { b.classList.remove("busy"); }
    });
    return b;
  }

  async function move() {
    const list = [...selected];
    const to = h("select", null, h("option", { value: "tunnel" }, t("inbound.door.tunnel")), h("option", { value: "ports" }, t("inbound.door.ports")));
    const lines = checklist();
    const unsub = onEvent("inbound.", (ev) => { if (list.includes(ev.subject) && (ev.message || ev.data)) lines.add({ ok: ev.level !== "error", text: `${ev.subject}: ${ev.message || ev.type}` }); });
    await open({
      title: t("inbound.moveQ", { n: list.length }),
      body: [h("p", { class: "dim" }, list.join(", ")), field(t("inbound.moveTo"), to), lines.el],
      buttons: [{ label: t("common.close"), value: null }, { label: t("inbound.move"), primary: true, onClick: async (_f, showError) => {
        lines.set(list.map((d) => ({ state: "pending", text: d })));
        const r = await api.post("/inbound/move", { domains: list, to: to.value });
        lines.set(r && r.lines);
        if (r && r.ok === false) { showError(t("inbound.moveFailed")); return false; }
        toast(t("inbound.moved", { n: list.length, door: t("inbound.door." + to.value) }), "ok");
        return false; // keep open so the lines can be read; Close dismisses
      } }],
    });
    unsub();
    selected = new Set();
    load(body, render);
  }

  // Rebuild on inbound events from elsewhere, but not while the operator is
  // reading result lines or has the move dialog open.
  subs.push(onEvent("inbound.", () => { if (!document.querySelector("dialog[open]") && !body.querySelector(".checklist li")) load(body, render); }));
  subs.push(onEvent("cert.", () => load(body, render)));
  load(body, render);
  return { el: p.el, destroy() { subs.forEach((u) => u()); } };
}
