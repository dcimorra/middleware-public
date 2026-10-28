// Inbound: the two doors and which one each domain comes in by. A door is
// toggled through POST /inbound/doors/{door}. Moving domains shows the
// verification lines as a checklist as they come, from the
// response and from inbound.* events for the same domains.
//
// Read top to bottom it goes from what is decided often to what is
// decided once: the doors, the domains behind them, then where new sites
// go, the VPN and the names no site claims. The local door's addresses
// and its resolver recipe are folded: they are set once per machine and
// made its card three times as tall as the other two.

import { icon } from "../lib/icons.js";
import { h, clear, fmtTime } from "../lib/dom.js";
import { t } from "../lib/i18n.js";
import { api, enc } from "../lib/api.js";
import { onEvent } from "../lib/stream.js";
import { page, load, table, cell, empty } from "../components/page.js";
import { checklist } from "../components/checklist.js";
import { open, confirm, toggle, field } from "../components/dialog.js";
import { toast, toastError } from "../components/toast.js";
import { vpnCard } from "./vpn.js";
import { unroutedCard } from "./unrouted.js";

const DOOR_ICON = { tunnel: "link", ports: "globe", local: "terminal" };

// dirtySave keeps a settings form's Save quiet until something in it
// changed: two blue Save buttons on a screen at rest competed with what
// the screen is for, and an enabled one did not say whether anything was
// unsaved. It returns the function that marks the form clean again.
export function dirtySave(form, btn) {
  btn.disabled = true;
  const dirty = () => { btn.disabled = false; btn.classList.add("primary"); };
  form.addEventListener("input", dirty);
  form.addEventListener("change", dirty);
  return () => { btn.disabled = true; btn.classList.remove("primary"); };
}

// doorHead is a door's title with its mark, green when the door is open,
// so the three cards read alike at a glance whatever is under them.
function doorHead(key, extra) {
  return h("div", { class: "np-door-head" },
    h("span", { class: "np-door-icon" }, icon(DOOR_ICON[key] || "door")),
    h("h2", null, t("inbound.door." + key)),
    extra || null);
}

export function inboundScreen() {
  const p = page({ title: t("nav.inbound"), sub: t("inbound.sub"), icon: "door" });
  p.el.classList.add("np-page");
  const body = h("div", { class: "np-stack" });
  const migration = h("div");
  let migTimer = null;
  // Its own card, loaded once: the doors re-render on every inbound event
  // and a device being named must not be wiped by one.
  p.body.append(migration, body, vpnCard(), unroutedCard());
  const subs = [];
  let selected = new Set();
  // The cards are drawn again on every inbound event: a fold the operator
  // opened stays open across it.
  let localOpen = false;

  async function render() {
    const data = await api.get("/inbound");
    const doors = data.doors || {};
    const domains = data.domains || [];
    let certs = [];
    try { certs = await api.get("/inbound/certificates"); } catch { certs = []; }
    const certBy = new Map((certs || []).map((c) => [c.domain, c]));

    const doorCards = h("div", { class: "grid-3 np-doors" }, doorCard("tunnel", doors.tunnel || {}), doorCard("ports", doors.ports || {}), localCard(doors.local || {}));

    const moveBtn = h("button", { type: "button", class: "btn", disabled: true }, t("inbound.move"));
    const count = h("span", { class: "np-count-pill", hidden: true });
    const sync = () => {
      moveBtn.disabled = selected.size === 0;
      moveBtn.classList.toggle("primary", selected.size > 0);
      count.hidden = selected.size === 0;
      count.textContent = String(selected.size);
    };
    // A checkbox is 15px; the label around it is the finger's target.
    const box = (input) => h("label", { class: "np-check" }, input);
    const all = h("input", { type: "checkbox", "aria-label": t("inbound.selectAll") });
    const rows = domains.map((d) => {
      const cb = h("input", { type: "checkbox", "aria-label": d.domain, checked: selected.has(d.domain) });
      cb.addEventListener("change", () => { if (cb.checked) selected.add(d.domain); else selected.delete(d.domain); sync(); });
      const cert = certBy.get(d.domain);
      const exp = d.cert_expires || (cert && cert.expires);
      const daysLeft = exp ? Math.round((new Date(exp) - Date.now()) / 86400000) : null;
      return h("tr", null,
        h("td", { class: "np-check-cell" }, box(cb)),
        cell(h("a", { class: "cell-link", href: `#/sites/${enc(d.domain)}` }, d.domain), null, "primary"),
        cell(doorBadge(d.door), t("inbound.col.door")),
        cell(dnsBadge(d.dns), t("inbound.col.dns")),
        cell(d.published ? h("span", { class: "badge " + ({ published: "ok", failed: "bad", manual: "warn", retiring: "warn", conflict: "bad" }[d.published] || ""), title: d.publish_error || "" }, t("publish.state." + d.published)) : "", t("inbound.col.published")),
        // Renewal is automatic, thirty days before expiry (checked hourly):
        // the hover says when, so nobody wonders whether to press Renew.
        cell(exp ? h("span", { class: "status", title: t("inbound.certAuto", { expires: fmtTime(exp), renews: fmtTime(new Date(new Date(exp) - 30 * 86400000)) }) },
          h("span", { class: "dot " + (daysLeft < 7 ? "bad" : daysLeft < 21 ? "warn" : "ok") }),
          daysLeft < 0 ? t("inbound.certExpired") : t("inbound.certDays", { n: daysLeft }),
        ) : "", t("inbound.col.cert")),
        cell(d.door === "ports" ? renewBtn(d.domain) : (d.tunnel_route ? h("code", { class: "small np-route" }, d.tunnel_route) : ""), null, "right"),
      );
    });
    all.addEventListener("change", () => {
      selected = all.checked ? new Set(domains.map((d) => d.domain)) : new Set();
      for (const cb of body.querySelectorAll("tbody input[type=checkbox]")) cb.checked = all.checked;
      sync();
    });
    sync();
    moveBtn.addEventListener("click", move);
    let tbl;
    if (domains.length) {
      tbl = table([
        { label: "" }, { label: t("inbound.col.domain") }, { label: t("inbound.col.door") }, { label: t("inbound.col.dns") }, { label: t("inbound.col.published") }, { label: t("inbound.col.cert") }, { label: "" },
      ], rows, { responsive: true }).el;
      tbl.classList.add("np-domains");
      const thCb = tbl.querySelector("th");
      if (thCb) { thCb.className = "np-check-cell"; clear(thCb).append(box(all)); }
    } else {
      tbl = empty(t("inbound.emptyTitle"), t("inbound.emptyHint"), h("a", { class: "btn", href: "#/sites" }, t("inbound.toSites")), "globe");
      tbl.classList.add("np-empty");
    }
    // What failed to publish is retried from where it shows, not from the
    // settings card it used to sit in.
    const failed = domains.filter((d) => ["failed", "conflict", "manual"].includes(d.published)).length;
    const retry = h("button", { type: "button", class: "btn sm ghost", hidden: !failed }, icon("refresh"), t("inbound.retry"));
    retry.addEventListener("click", async () => {
      try { await api.post("/publish/retry"); toast(t("inbound.retried"), "ok"); } catch (e) { toastError(e); }
    });

    clear(body).append(
      doorCards,
      h("div", { class: "card pad0" },
        h("div", { class: "log-bar card-pad np-card-head" }, h("h2", { class: "grow" }, t("inbound.domains"), domains.length ? h("span", { class: "np-quiet" }, String(domains.length)) : null), retry, h("span", { class: "np-move" }, count, moveBtn)),
        tbl,
      ),
      publishingCard(data),
    );
  }

  function doorCard(key, door) {
    const sw = toggle(door.open ? t("inbound.open") : t("inbound.closed"), { checked: Boolean(door.open) });
    const lines = checklist();
    const card = h("div", { class: "card np-door" + (door.open ? " on" : "") });
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
        card.classList.toggle("on", sw.input.checked);
      } catch (e) {
        sw.input.checked = !opening;
        toastError(e);
      } finally { sw.input.disabled = false; }
    });
    const details = key === "ports"
      ? t("inbound.portsDetail", { http: door.http ?? 80, https: door.https ?? 443 })
      : t("inbound.tunnelDetail");
    card.append(doorHead(key), h("p", { class: "np-door-text" }, details), h("div", { class: "np-door-foot" }, sw.el), lines.el);
    return card;
  }

  // The local door is the development bench: plain http on this machine
  // and a name server for the sites. Its addresses are edited here
  // because a Docker install needs them on 0.0.0.0 with the ports mapped.
  function localCard(door) {
    const sw = toggle(door.open ? t("inbound.open") : t("inbound.closed"), { checked: Boolean(door.open) });
    const httpIn = h("input", { type: "text", class: "mono", value: door.http || "", placeholder: "127.0.0.1:80" });
    const dnsIn = h("input", { type: "text", class: "mono", value: door.dns || "", placeholder: "127.0.0.1:5353" });
    const saveBtn = h("button", { type: "submit", class: "btn sm" }, icon("save"), t("common.save"));
    const broken = door.open && !door.listening;
    const state = door.open
      ? (door.listening ? h("span", { class: "badge ok" }, t("inbound.localListening")) : h("span", { class: "badge bad", title: door.error || "" }, t("inbound.localNotListening")))
      : null;
    const apply = async (open) => {
      const r = await api.put("/inbound/local", { open, http: httpIn.value.trim(), dns: dnsIn.value.trim() });
      const l = (r && r.local) || {};
      sw.input.checked = Boolean(l.open);
      sw.el.lastChild.textContent = l.open ? t("inbound.open") : t("inbound.closed");
      if (l.open && !l.listening) toast(l.error || t("inbound.localNotListening"), "bad");
      return l;
    };
    sw.input.addEventListener("change", async () => {
      sw.input.disabled = true;
      try {
        const l = await apply(sw.input.checked);
        toast(l.open ? t("inbound.opened", { door: t("inbound.door.local") }) : t("inbound.closedNow", { door: t("inbound.door.local") }), "ok");
        load(body, render);
      } catch (e) { sw.input.checked = !sw.input.checked; toastError(e); } finally { sw.input.disabled = false; }
    });
    const form = h("form", { class: "np-fold-body np-local", novalidate: true },
      h("div", { class: "form-row" }, field(t("inbound.localHTTP"), httpIn), field(t("inbound.localDNS"), dnsIn)),
      h("div", { class: "actions" }, saveBtn),
      h("details", { class: "np-more" }, h("summary", null, t("inbound.localHelp")), h("pre", { class: "log help" }, door.help || "")));
    const clean = dirtySave(form, saveBtn);
    form.addEventListener("submit", async (e) => {
      e.preventDefault();
      saveBtn.classList.add("busy");
      try { await apply(sw.input.checked); clean(); toast(t("inbound.localSaved"), "ok"); load(body, render); }
      catch (ex) { toastError(ex); } finally { saveBtn.classList.remove("busy"); }
    });
    // Folded unless the door is open and not listening: then the address
    // is what to look at.
    const addrs = h("details", { class: "np-fold", open: broken || localOpen }, h("summary", null, t("inbound.localAddrs", { http: door.http || "—", dns: door.dns || "—" })), form);
    addrs.addEventListener("toggle", () => { localOpen = addrs.open; });
    return h("div", { class: "card np-door" + (door.open ? " on" : "") },
      doorHead("local", state),
      h("p", { class: "np-door-text" }, t("inbound.localDetail")),
      h("div", { class: "np-door-foot" }, sw.el),
      addrs,
    );
  }

  // Where new sites go and the address A records point at: the two
  // decisions publishing needs that are not a site's own.
  function publishingCard(data) {
    const door = h("select", null, h("option", { value: "tunnel" }, t("inbound.door.tunnel")), h("option", { value: "ports" }, t("inbound.door.ports")));
    door.value = data.new_sites || "tunnel";
    const ip = h("input", { type: "text", class: "mono", value: data.public_ip || "", placeholder: "203.0.113.10", autocomplete: "off" });
    const save = h("button", { type: "submit", class: "btn" }, icon("save"), t("common.save"));
    const form = h("form", { novalidate: true },
      h("div", { class: "form-row" }, field(t("inbound.newSites"), door), field(t("inbound.publicIP"), ip, t("inbound.publicIPHint"))),
      h("div", { class: "form-actions" }, save));
    const clean = dirtySave(form, save);
    form.addEventListener("submit", async (e) => {
      e.preventDefault();
      save.classList.add("busy");
      try { await api.put("/inbound/publishing", { new_sites: door.value, public_ip: ip.value.trim() }); clean(); toast(t("inbound.publishingSaved"), "ok"); }
      catch (ex) { toastError(ex); } finally { save.classList.remove("busy"); }
    });
    return h("div", { class: "card" },
      h("h2", null, t("inbound.publishing")),
      h("p", { class: "np-lead" }, t("inbound.publishingDetail")),
      form,
    );
  }

  function doorBadge(door) {
    if (door === "tunnel") return h("span", { class: "badge info" }, t("inbound.door.tunnel"));
    if (door === "ports") return h("span", { class: "badge accent" }, t("inbound.door.ports"));
    return h("span", { class: "badge warn" }, t("inbound.door.none"));
  }
  // Not checked yet is not news: a grey "unknown" pill on every row was
  // the loudest thing in the table and said nothing.
  function dnsBadge(dns) {
    if (dns === true || dns === "ok") return h("span", { class: "badge ok" }, t("inbound.dnsOk"));
    if (dns === false || dns === "missing") return h("span", { class: "badge bad" }, t("inbound.dnsMissing"));
    if (!dns) return h("span", { class: "muted small" }, t("inbound.dnsUnknown"));
    return h("span", { class: "badge" }, String(dns));
  }
  function renewBtn(domain) {
    const b = h("button", { type: "button", class: "btn sm ghost" }, icon("refresh"), t("inbound.renew"));
    b.addEventListener("click", async () => {
      b.classList.add("busy");
      try { await api.post(`/inbound/certificates/${enc(domain)}/renew`); toast(t("inbound.renewStarted", { domain }), "ok"); }
      catch (e) { toastError(e); } finally { b.classList.remove("busy"); }
    });
    return b;
  }

  // Moving starts a migration on the server, one domain at a time; the
  // card above the doors follows it and can stop it, here or after coming
  // back to this screen.
  async function move() {
    const list = [...selected];
    const to = h("select", null, h("option", { value: "tunnel" }, t("inbound.door.tunnel")), h("option", { value: "ports" }, t("inbound.door.ports")));
    const ok = await open({
      title: t("inbound.moveQ", { n: list.length }),
      body: [h("p", { class: "dim" }, list.join(", ")), field(t("inbound.moveTo"), to), h("p", { class: "small muted" }, t("inbound.moveBackground"))],
      buttons: [{ label: t("common.cancel"), value: null }, { label: t("inbound.move"), primary: true, submit: true }],
      onSubmit: async () => { await api.post("/inbound/migration", { domains: list, to: to.value }); return true; },
    });
    if (!ok) return;
    selected = new Set();
    showMigration();
  }

  async function showMigration() {
    let m;
    try { m = await api.get("/inbound/migration"); } catch { return; }
    clearTimeout(migTimer);
    if (!m.id) { clear(migration); return; }
    const total = m.domains.length || 1;
    const fill = h("div", { class: "fill" + (m.failed ? " warn" : "") });
    fill.style.width = `${Math.round((m.done / total) * 100)}%`;
    const cancel = h("button", { type: "button", class: "btn sm danger", hidden: !m.running }, t("inbound.migrationCancel"));
    cancel.addEventListener("click", async () => { try { await api.post("/inbound/migration/cancel"); toast(t("inbound.migrationCancelling"), "ok"); } catch (e) { toastError(e); } });
    const dismiss = h("button", { type: "button", class: "btn sm ghost", hidden: m.running }, t("common.close"));
    dismiss.addEventListener("click", () => clear(migration));
    const state = m.running ? t("inbound.migrationRunning", { done: m.done, total, current: m.current || "…", door: t("inbound.door." + m.to) })
      : t(m.cancelled ? "inbound.migrationCancelled" : "inbound.migrationDone", { done: m.done, total, failed: m.failed, door: t("inbound.door." + m.to) });
    clear(migration).append(h("div", { class: "card" },
      h("div", { class: "log-bar" }, h("h2", { class: "grow" }, t("inbound.migrationTitle")), cancel, dismiss),
      h("p", { class: "small" }, state),
      h("div", { class: "meter", role: "progressbar", "aria-valuenow": String(m.done), "aria-valuemax": String(total) }, fill),
      h("details", { class: "np-more" }, h("summary", null, t("inbound.migrationLines", { n: m.lines.length })),
        h("div", { class: "stack-sm small mono" }, ...m.lines.map((l) => h("div", { class: l.status === "fail" ? "level-error" : l.status === "ok" ? "" : "muted" }, `[${l.status}] ${l.text}`))))));
    if (m.running) migTimer = setTimeout(showMigration, 1500);
    else load(body, render);
  }
  subs.push(onEvent("inbound.migration.", () => showMigration()));
  showMigration();


  // Rebuild on inbound events from elsewhere, but not while the operator is
  // reading result lines, has the move dialog open, or is typing in a form.
  const busy = () => document.querySelector("dialog[open]") || body.querySelector(".checklist li") || body.querySelector("form button[type=submit]:not(:disabled)");
  subs.push(onEvent("inbound.", () => { if (!busy()) load(body, render); }));
  subs.push(onEvent("cert.", () => { if (!busy()) load(body, render); }));
  subs.push(onEvent("publish.", () => { if (!busy()) load(body, render); }));
  load(body, render);
  return { el: p.el, destroy() { clearTimeout(migTimer); subs.forEach((u) => u()); } };
}
