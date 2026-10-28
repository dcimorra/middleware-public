// The VPN as the operator uses it: its devices (each added one gives its
// WireGuard file at once, the only time it can be had: the device's key is
// not kept), the panel inside the VPN or not, and, per site, whether the
// site or some of its paths answer only through it.

import { h, clear, fmtAgo, fmtBytes } from "../lib/dom.js";
import { t } from "../lib/i18n.js";
import { api, enc } from "../lib/api.js";
import { notice, table, cell, empty } from "../components/page.js";
import { open, confirm, field, toggle, checkbox } from "../components/dialog.js";
import { toast, toastError } from "../components/toast.js";
import { store } from "../lib/store.js";
import { icon } from "../lib/icons.js";

const VPN = "/modules/vpn";

async function vpnStatus() {
  try { return await api.get(VPN + "/"); } catch { return null; }
}

// download hands the browser a file made here, from text: no URL to it
// exists on the server, because the device's private key is in it.
function download(name, text) {
  const url = URL.createObjectURL(new Blob([text], { type: "text/plain" }));
  const a = h("a", { href: url, download: name });
  document.body.append(a);
  a.click();
  a.remove();
  setTimeout(() => URL.revokeObjectURL(url), 10000);
}

// vpnCard is the Inbound screen's VPN card.
export function vpnCard() {
  const el = h("div", { class: "card np-vpn" });
  // The head reads like the doors' above it: a mark, the name, and what
  // it is in a line; off, the way to turn it on sits right beside.
  const head = (text, ...side) => h("div", { class: "np-link-head" },
    h("span", { class: "np-link-icon" }, icon("lock")),
    h("div", { class: "grow" }, h("h2", null, t("vpn.title")), h("p", null, text)),
    side.length ? h("div", { class: "actions" }, ...side) : null);
  async function load() {
    const st = await vpnStatus();
    clear(el);
    if (!st || !st.enabled) {
      el.classList.add("off");
      el.append(head(t("vpn.off"), h("a", { class: "btn", href: "#/modules" }, t("vpn.enableIt"))));
      return;
    }
    el.classList.remove("off");
    el.append(head(t("vpn.on", { endpoint: st.endpoint || "—", address: st.address || "—" })));
    if (st.error) el.append(notice("bad", st.error));
    if (!st.helper) el.append(notice("warn", t("vpn.noHelper")));
    else if (!st.up) el.append(notice("bad", t("vpn.down")));

    const add = h("button", { type: "button", class: "btn" }, icon("plus"), t("vpn.addDevice"));
    add.addEventListener("click", addDevice);
    const peers = st.peers || [];
    const rows = peers.map((p) => {
      const del = h("button", { type: "button", class: "btn sm ghost danger", "aria-label": t("vpn.remove", { name: p.name }) }, icon("trash"));
      del.addEventListener("click", async () => {
        if (!(await confirm({ title: t("vpn.removeQ", { name: p.name }), body: t("vpn.removeBody"), confirmLabel: t("common.delete"), danger: true }))) return;
        try { await api.del(`${VPN}/peers/${enc(p.name)}`); toast(t("vpn.removed", { name: p.name }), "ok"); load(); } catch (e) { toastError(e); }
      });
      return h("tr", null,
        cell(h("strong", null, p.name), null, "primary"),
        cell(h("code", null, p.address), t("vpn.address"), "mono"),
        cell(h("span", { class: "status" }, h("span", { class: "dot " + (p.online ? "ok" : "") }), p.online ? t("vpn.online") : (p.last_handshake && !p.last_handshake.startsWith("0001") ? fmtAgo(p.last_handshake, t) : t("vpn.never"))), t("vpn.seen")),
        cell(`${fmtBytes(p.rx || 0)} / ${fmtBytes(p.tx || 0)}`, t("vpn.traffic"), "num mono"),
        cell(del, null, "right"));
    });
    let devices;
    if (rows.length) devices = table([{ label: t("vpn.device") }, { label: t("vpn.address") }, { label: t("vpn.seen") }, { label: t("vpn.traffic"), class: "num" }, { label: "" }], rows, { responsive: true }).el;
    else {
      // With no device the VPN reaches nobody: adding one is the thing to
      // do here, and the empty state says so with the button in it.
      add.classList.add("primary");
      devices = empty(t("vpn.noDevices"), t("vpn.noDevicesHint"), add, "lock");
      devices.classList.add("np-empty");
    }
    el.append(
      h("div", { class: "log-bar np-sub-head" }, h("h3", { class: "grow" }, t("vpn.devices")), rows.length ? add : null),
      devices);

    // The panel inside the VPN. Refused with no device: nobody could
    // reach it then. SSH to the machine stays the way back.
    const only = toggle(t("vpn.panelOnly"), { checked: st.vpn_only, disabled: !peers.length && !st.vpn_only });
    only.input.addEventListener("change", async () => {
      const on = only.input.checked;
      if (!(await confirm({ title: t(on ? "vpn.panelOnlyQ" : "vpn.panelOpenQ"), body: t(on ? "vpn.panelOnlyBody" : "vpn.panelOpenBody"), confirmLabel: t(on ? "vpn.panelOnlyBtn" : "vpn.panelOpenBtn"), danger: on }))) {
        only.input.checked = !on;
        return;
      }
      try { await api.post(VPN + "/only", { enabled: on }); toast(t(on ? "vpn.panelOnlyDone" : "vpn.panelOpenDone"), "ok"); } catch (e) { only.input.checked = !on; toastError(e); }
    });
    // In a network the switch is the network's: it closes or opens every
    // node's panel, and says so.
    const net = store.get("nodes");
    el.append(h("div", { class: "field np-vpn-only" }, only.el, h("p", { class: "hint" }, t("vpn.panelOnlyHint")),
      net && net.network ? h("p", { class: "hint" }, t("vpn.panelOnlyNetwork")) : null));
  }

  async function addDevice() {
    const name = h("input", { type: "text", class: "mono", placeholder: "phone", autocomplete: "off", spellcheck: "false" });
    let made = null;
    const ok = await open({
      title: t("vpn.addDevice"),
      body: [field(t("vpn.deviceName"), name, t("vpn.deviceNameHint"))],
      buttons: [{ label: t("common.cancel"), value: null }, { label: t("vpn.create"), submit: true, primary: true }],
      onSubmit: async () => { made = await api.post(VPN + "/peers", { name: name.value.trim() }); },
    });
    if (!ok || !made) return;
    const file = `${made.name}.conf`;
    download(file, made.config);
    const again = h("button", { type: "button", class: "btn" }, icon("download"), t("vpn.downloadAgain"));
    again.addEventListener("click", () => download(file, made.config));
    await open({
      title: t("vpn.created", { name: made.name }),
      body: [notice("warn", t("vpn.onlyNow")), h("p", { class: "small" }, t("vpn.howTo")), h("pre", { class: "snippet" }, made.config), h("div", { class: "form-actions" }, again)],
      buttons: [{ label: t("common.close"), value: null, primary: true }],
    });
    load();
  }
  load().catch((e) => clear(el).append(notice("bad", e.message)));
  return el;
}

// vpnSiteCard is a site's choice: public, the whole site inside the VPN,
// or only some paths; and, optionally, which devices.
export function vpnSiteCard(domain) {
  const el = h("div", { class: "card" });
  (async () => {
    const st = await vpnStatus();
    const s = store.site(domain);
    const v = (s && s.manifest && s.manifest.vpn) || null;
    clear(el).append(h("h2", null, t("vpn.siteTitle")), h("p", { class: "small muted" }, t("vpn.siteHint")));
    if ((!st || !st.enabled) && !v) { el.append(notice("info", t("vpn.siteNeedsVPN"))); return; }
    const mode = h("select", null, ...["public", "whole", "paths"].map((k) => h("option", { value: k }, t("vpn.mode." + k))));
    mode.value = !v ? "public" : (v.paths && v.paths.length ? "paths" : "whole");
    const paths = h("textarea", { class: "mono", rows: "3", placeholder: "/admin\n/api/internal", value: ((v && v.paths) || []).join("\n") });
    const pathsField = field(t("vpn.paths"), paths, t("vpn.pathsHint"));
    const devs = (st && st.peers || []).map((p) => ({ name: p.name, box: checkbox(p.name, { checked: Boolean(v && (v.devices || []).includes(p.name)) }) }));
    const devsField = h("div", { class: "field" }, h("label", null, t("vpn.onlyDevices")),
      devs.length ? h("div", { class: "row" }, ...devs.map((d) => d.box.el)) : h("p", { class: "small muted" }, t("vpn.noDevices")),
      h("div", { class: "hint" }, t("vpn.onlyDevicesHint")));
    const sync = () => { pathsField.hidden = mode.value !== "paths"; devsField.hidden = mode.value === "public"; };
    mode.addEventListener("change", sync);
    sync();
    const save = h("button", { type: "submit", class: "btn primary" }, t("common.save"));
    const form = h("form", { novalidate: true }, field(t("vpn.mode"), mode), pathsField, devsField, h("div", { class: "form-actions" }, save));
    form.addEventListener("submit", async (e) => {
      e.preventDefault();
      let value = null;
      if (mode.value !== "public") {
        value = {};
        const ds = devs.filter((d) => d.box.input.checked).map((d) => d.name);
        if (ds.length) value.devices = ds;
        if (mode.value === "paths") {
          const ps = paths.value.split(/[\n,]/).map((x) => x.trim()).filter(Boolean);
          if (!ps.length) { toastError(new Error(t("vpn.pathsEmpty"))); return; }
          value.paths = ps;
        }
      }
      if (mode.value === "whole" && !(await confirm({ title: t("vpn.wholeQ", { domain }), body: t("vpn.wholeBody"), confirmLabel: t("common.save"), danger: true }))) return;
      save.classList.add("busy");
      try {
        const updated = await api.patch(`/sites/${enc(domain)}/manifest`, { vpn: value });
        if (updated && updated.domain) store.putSite(updated);
        toast(t("vpn.siteSaved"), "ok");
      } catch (ex) { toastError(ex); } finally { save.classList.remove("busy"); }
    });
    el.append(form);
  })();
  return el;
}
