// Modules: one card each, a form built from what the module says it
// needs, and Verify / Enable that show the result lines as a checklist.
// Secrets are password fields and are never echoed back: the API masks
// them, so an empty secret field on save means "keep what is stored".

import { h, clear, copy, fmtTime } from "../lib/dom.js";
import { t } from "../lib/i18n.js";
import { api, enc } from "../lib/api.js";
import { onEvent } from "../lib/stream.js";
import { page, load, empty, notice } from "../components/page.js";
import { icon } from "../lib/icons.js";
import { openCodeProviders } from "./opencode-providers.js";

import { checklist } from "../components/checklist.js";
import { confirm, field } from "../components/dialog.js";
import { toast, toastError } from "../components/toast.js";

export function modulesScreen() {
  const p = page({ title: t("nav.modules"), sub: t("modules.sub"), icon: "blocks" });
  const body = h("div", { class: "module-groups" });
  p.body.append(body);
  const subs = [];

  async function render() {
    const [mods, deps] = await Promise.all([api.get("/modules"), api.get("/deps").catch(() => [])]);
    clear(body);
    if (!mods || mods.length === 0) { body.append(empty(t("modules.emptyTitle"), t("modules.emptyHint"))); return null; }
    const depBy = new Map((deps || []).map((d) => [d.name, d]));
    // Grouped by what they are for, in a fixed order; one the panel does
    // not know yet goes under "others" rather than nowhere.
    const groups = new Map(GROUPS.map((g) => [g, []]));
    for (const m of mods) groups.get(GROUP_OF[m.key] || "other").push(m);
    for (const [g, list] of groups) {
      if (!list.length) continue;
      body.append(h("section", { class: "module-group" },
        h("h3", null, t("modules.group." + g)),
        h("div", { class: "module-grid" }, ...list.map((m) => card(m, depBy)))));
    }
    return null;
  }

  const GROUPS = ["connect", "deploy", "agents", "other"];
  const GROUP_OF = { cloudflare: "connect", vpn: "connect", github: "deploy", "agent-claude": "agents", "agent-gemini": "agents", "agent-opencode": "agents" };
  const ICON_OF = { cloudflare: "globe", vpn: "lock", github: "branch", "agent-claude": "sparkles", "agent-gemini": "sparkles", "agent-opencode": "sparkles" };
  // The panel's own words for a module when it has them; the module's
  // (in English, from the server) otherwise.
  const said = (key, fallback) => { const s = t(key); return s === key ? fallback : s; };

  // Which command an agent module runs, for the binary line.
  const AGENT_BIN = { "agent-claude": "claude", "agent-gemini": "gemini", "agent-opencode": "opencode" };

  function card(m, depBy) {
    const inputs = {};
    const form = h("form", { novalidate: true, class: "needs" });
    // A module that needs nothing typed (every field has a default the
    // module fills, as the VPN) is a switch: on or off, its fields folded
    // under "advanced" for whoever wants to change them.
    const visible = (m.needs || []).filter((n) => !n.hidden);
    const simple = visible.length > 0 && visible.every((n) => n.optional);
    // An enabled module's settings are folded: the card says what it is
    // and that it works; the fields are there for whoever opens them.
    const folded = simple || (m.enabled && visible.length > 0);
    const fields = folded ? h("details", { class: "advanced" }, h("summary", null, t(simple ? "modules.advanced" : "modules.settings"))) : form;
    for (const n of m.needs || []) {
      // A hidden need is filled by the module itself (a session from a
      // sign-in); it has its own place on the card.
      if (n.hidden) continue;
      const input = h("input", {
        type: n.secret ? "password" : "text", autocomplete: n.secret ? "new-password" : "off",
        class: "mono", placeholder: n.secret && m.enabled ? t("modules.secretKept") : "", value: n.value || "",
      });
      inputs[n.key] = input;
      fields.append(field(said(`modules.need.${m.key}.${n.key}`, n.label || n.key), input, said(`modules.needHelp.${m.key}.${n.key}`, n.help || n.hint)));
    }
    if (folded) form.append(fields);
    const lines = checklist();
    const verifyBtn = h("button", { type: "button", class: "btn", hidden: simple || !(m.needs && m.needs.length) }, t("modules.verify"));
    const enableBtn = h("button", { type: "submit", class: "btn primary", hidden: simple && m.enabled }, m.enabled ? t("modules.save") : t("modules.enable"));
    const disableBtn = h("button", { type: "button", class: "btn" + (simple ? " danger" : ""), hidden: !m.enabled }, t("modules.disable"));
    const forgetBtn = h("button", { type: "button", class: "btn ghost sm", hidden: simple || !(m.needs && m.needs.length) }, t("modules.forget"));
    // On a switch, changing an advanced field and saving is the only reason
    // to press "save" while it is on.
    if (simple) {
      const saveAdvanced = h("button", { type: "submit", class: "btn sm", hidden: !m.enabled }, t("modules.save"));
      fields.append(h("div", { class: "form-actions" }, saveAdvanced));
    }
    const config = () => {
      const c = {};
      for (const [k, input] of Object.entries(inputs)) if (input.value !== "") c[k] = input.value;
      return c;
    };
    const run = async (btn, path, done) => {
      btn.classList.add("busy");
      lines.set([{ state: "pending", text: t("modules.running") }]);
      try {
        const r = await api.post(path, { config: config() });
        lines.set(r && r.lines);
        if (r && r.ok === false) toast(t("modules.failed", { name: m.name }), "bad");
        else if (done) { toast(done, "ok"); }
        return r;
      } catch (e) {
        lines.set([{ ok: false, text: e.message }]);
      } finally { btn.classList.remove("busy"); }
      return null;
    };
    verifyBtn.addEventListener("click", () => run(verifyBtn, `/modules/${enc(m.key)}/verify`, t("modules.verified", { name: m.name })));
    form.addEventListener("submit", async (e) => {
      e.preventDefault();
      const r = await run(enableBtn, `/modules/${enc(m.key)}/enable`, t("modules.enabled", { name: m.name }));
      if (r && r.ok !== false) {
        m.enabled = true; state.replaceWith(stateBadge(m)); enableBtn.textContent = t("modules.save"); disableBtn.hidden = false;
        if (simple) load(body, render);
      }
    });
    disableBtn.addEventListener("click", async () => {
      if (!(await confirm({ title: t("modules.disableQ", { name: m.name }), body: t("modules.disableBody"), confirmLabel: t("modules.disable"), danger: true }))) return;
      try { await api.post(`/modules/${enc(m.key)}/disable`); toast(t("modules.disabled", { name: m.name }), "ok"); load(body, render); } catch (err) { toastError(err); }
    });
    forgetBtn.addEventListener("click", async () => {
      if (!(await confirm({ title: t("modules.forgetQ", { name: m.name }), body: t("modules.forgetBody"), confirmLabel: t("modules.forget"), danger: true }))) return;
      try { await api.del(`/modules/${enc(m.key)}/credentials`); toast(t("modules.forgotten", { name: m.name }), "ok"); load(body, render); } catch (err) { toastError(err); }
    });
    form.append(lines.el, h("div", { class: "form-actions" }, verifyBtn, enableBtn, disableBtn, forgetBtn));
    const state = stateBadge(m);
    const agentBin = AGENT_BIN[m.key];
    return h("div", { class: "card module-card" + (m.enabled ? " on" : "") },
      h("div", { class: "module-head" },
        h("span", { class: "module-icon" }, icon(ICON_OF[m.key] || "blocks")),
        h("div", { class: "grow" }, h("h2", null, m.name || m.key), h("p", { class: "dim small" }, said("modules.desc." + m.key, m.description || ""))),
        state),
      m.installed === false && m.enabled && !agentBin ? notice("warn", t("modules.notInstalled")) : null,
      // OpenCode has no sign-in of its own: a key per provider instead.
      m.key === "agent-opencode" ? openCodeProviders() : agentBin ? signIn(m) : null,
      agentBin ? binaryLine(agentBin, depBy.get(agentBin)) : null,
      form,
    );
  }

  // The sign-in with a subscription: the vendor's own flow, run on a
  // terminal of the server's. Start → open the link on any device →
  // paste the code → the session is stored and the module enabled. The
  // state is polled every two seconds and never waited on in a request.
  function signIn(m) {
    const box = h("div", { class: "signin" });
    const sessionLine = h("p", { class: "small" });
    const startBtn = h("button", { type: "button", class: "btn primary sm" }, icon("key"), t("modules.signIn"));
    const flow = h("div", { hidden: true });
    let timer = null;
    const stop = () => { if (timer) { clearInterval(timer); timer = null; } };
    const showSession = (s) => {
      clear(sessionLine);
      if (!s || s.kind === "none") sessionLine.append(h("span", { class: "muted" }, t("modules.sessionNone")));
      else if (s.kind === "api_key") sessionLine.append(h("span", { class: "badge info" }, t("modules.sessionKey")));
      else sessionLine.append(h("span", { class: "badge ok" }, t("modules.sessionSubscription")), " ", h("span", { class: "muted" }, s.since ? t("modules.sessionSince", { when: fmtTime(s.since) }) : ""));
    };
    const render = (st) => {
      clear(flow);
      flow.hidden = false;
      if (st.error && st.state !== "authorize") flow.append(notice("bad", st.error));
      if (st.state === "starting") flow.append(h("p", { class: "small muted" }, t("modules.signInStarting")));
      if (st.state === "authorize") {
        const code = h("input", { type: "text", class: "mono", placeholder: t("modules.signInCodePlaceholder"), autocomplete: "off", spellcheck: "false" });
        const send = h("button", { type: "button", class: "btn primary sm" }, icon("send"), t("modules.signInSend"));
        const copyBtn = h("button", { type: "button", class: "btn sm" }, icon("copy"), t("common.copy"));
        copyBtn.addEventListener("click", async () => { toast((await copy(st.url)) ? t("common.copied") : st.url, "ok"); });
        send.addEventListener("click", async () => {
          send.classList.add("busy");
          try { render(await api.post(`/modules/${enc(m.key)}/login/code`, { code: code.value })); }
          catch (e) { toastError(e); } finally { send.classList.remove("busy"); }
        });
        flow.append(
          h("ol", { class: "steps small" },
            h("li", null, t("modules.signInStep1"), " ", h("a", { href: st.url, target: "_blank", rel: "noopener" }, icon("external", { size: 14 }), t("modules.signInOpen")), " ", copyBtn),
            h("li", null, t("modules.signInStep2")),
          ),
          h("div", { class: "row" }, code, send),
        );
        // A native append() would print "null" for a missing error.
        if (st.error) flow.append(notice("warn", st.error));
        setTimeout(() => code.focus(), 0);
      }
      if (st.state === "checking") flow.append(h("p", { class: "small muted" }, t("modules.signInChecking")));
      if (st.tail && st.state !== "authorize") flow.append(h("pre", { class: "log small" }, st.tail));
      const cancel = h("button", { type: "button", class: "btn ghost sm" }, icon("x"), t("common.cancel"));
      cancel.addEventListener("click", async () => { stop(); try { await api.del(`/modules/${enc(m.key)}/login`); } catch { /* gone already */ } flow.hidden = true; startBtn.hidden = false; });
      if (st.state === "starting" || st.state === "authorize" || st.state === "checking") flow.append(h("div", { class: "actions" }, cancel));
      else { startBtn.hidden = false; stop(); }
    };
    const poll = async () => {
      try {
        const st = await api.get(`/modules/${enc(m.key)}/login`);
        if (st.state === "done") {
          stop();
          flow.hidden = true;
          startBtn.hidden = false;
          showSession(st.session);
          toast(t("modules.signedIn", { name: m.name }), "ok");
          load(body, render0);
          return;
        }
        if (st.state === "failed") { stop(); render(st); return; }
        if (st.state) render(st);
      } catch (e) { stop(); render({ state: "failed", error: e.message }); }
    };
    startBtn.addEventListener("click", async () => {
      startBtn.hidden = true;
      render({ state: "starting" });
      try {
        render(await api.post(`/modules/${enc(m.key)}/login`, {}));
        stop();
        timer = setInterval(poll, 2000);
      } catch (e) { render({ state: "failed", error: e.message }); }
    });
    api.get(`/modules/${enc(m.key)}/session`).then((s) => showSession(s.session)).catch(() => showSession(null));
    // A sign-in left open in another tab is picked up here.
    api.get(`/modules/${enc(m.key)}/login`).then((st) => { if (st.state && st.state !== "done" && st.state !== "failed") { startBtn.hidden = true; render(st); timer = setInterval(poll, 2000); } }).catch(() => {});
    box.append(h("div", { class: "log-bar" }, h("span", { class: "label" }, t("modules.session")), sessionLine, h("span", { class: "grow" }), startBtn), flow);
    return box;
  }
  const render0 = render;

  // What runs, and whether a newer one exists. Installed and kept
  // current by the installation; the button is for not waiting a day.
  function binaryLine(name, d) {
    const line = h("p", { class: "small binary-line" });
    const btn = h("button", { type: "button", class: "btn sm" }, icon("download"), d && d.installed ? t("modules.binaryUpdate") : t("modules.binaryInstall"));
    btn.addEventListener("click", async () => {
      btn.classList.add("busy");
      try { await api.post(`/deps/${enc(name)}/install`); toast(t("modules.binaryDone", { name }), "ok"); load(body, render0); }
      catch (e) { toastError(e); } finally { btn.classList.remove("busy"); }
    });
    if (!d || !d.installed) line.append(h("span", { class: "muted" }, t("modules.binaryMissing", { name })), " ", btn);
    else {
      line.append(h("code", null, name + " " + (d.version || "?")));
      if (d.outdated) line.append(" ", h("span", { class: "badge warn" }, t("modules.binaryOutdated", { v: d.latest })), " ", btn);
      else if (d.managed) line.append(" ", h("span", { class: "muted" }, t("modules.binaryManaged")));
      else line.append(" ", h("span", { class: "muted" }, t("modules.binaryElsewhere", { path: d.path })));
    }
    return line;
  }
  function stateBadge(m) {
    return m.enabled ? h("span", { class: "badge ok" }, t("modules.on")) : h("span", { class: "badge" }, t("modules.off"));
  }

  load(body, render);
  // A module event from elsewhere (the CLI, another tab) refreshes the
  // cards, unless the operator is typing or reading result lines here:
  // rebuilding would wipe the very lines they asked for.
  subs.push(onEvent("module.", () => {
    const typing = document.activeElement && document.activeElement.tagName === "INPUT";
    const reading = body.querySelector(".checklist li");
    if (!typing && !reading) load(body, render);
  }));
  return { el: p.el, destroy() { subs.forEach((u) => u()); } };
}
