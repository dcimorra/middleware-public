// The shell: strings, session, navigation, the one event stream, and the
// routes. Screens live in ./screens and know nothing about each other;
// what they share is in ./lib and ./components.

import { h, clear } from "./lib/dom.js";
import { t, loadStrings } from "./lib/i18n.js";
import { api, onUnauthorized } from "./lib/api.js";
import { store } from "./lib/store.js";
import { connect, disconnect } from "./lib/stream.js";
import { route, mount, navigate, currentPath, render as rerender } from "./lib/router.js";
import { applyTheme, getTheme, setTheme, getLang } from "./lib/theme.js";
import { installShortcuts, showShortcuts, GOTO } from "./components/shortcuts.js";
import { toast } from "./components/toast.js";
import { icon } from "./lib/icons.js";
import { authScreen } from "./screens/auth.js";
import { sitesScreen } from "./screens/sites.js";
import { siteScreen } from "./screens/site.js";
import { inboundScreen } from "./screens/inbound.js";
import { wafScreen } from "./screens/waf.js";
import { modulesScreen } from "./screens/modules.js";
import { tasksScreen } from "./screens/tasks.js";
import { eventsScreen } from "./screens/events.js";
import { guardScreen } from "./screens/guard.js";
import { reposScreen } from "./screens/repos.js";
import { docsScreen } from "./screens/docs.js";
import { auditScreen } from "./screens/audit.js";
import { edgeScreen } from "./screens/edge.js";
import { openBanner } from "./screens/settings-access.js";
import { settingsScreen } from "./screens/settings.js";
import { systemScreen } from "./screens/system.js";
import { houseScreen } from "./screens/house.js";
import { claudeUsageScreen } from "./screens/claude-usage.js";

const app = document.getElementById("app");
const main = document.getElementById("main");
const sidebar = document.getElementById("sidebar");
const topbar = document.getElementById("topbar");
const backdrop = document.getElementById("backdrop");
const menuBtn = document.getElementById("menu-btn");
const themeBtn = document.getElementById("theme-btn");
const helpBtn = document.getElementById("help-btn");
const streamState = document.getElementById("stream-state");

applyTheme();

// Each entry: key, route, shortcut, icon. Two groups: what the operator
// runs (sites, doors, protection, modules, tasks) and what they watch
// (events, guard, settings, the process); the sidebar draws the line.
const NAV = [
  ["sites", "/sites", "g s", "globe"],
  ["inbound", "/inbound", "g i", "door"],
  ["waf", "/waf", "g w", "shield"],
  ["modules", "/modules", "g m", "blocks"],
  ["tasks", "/tasks", "g t", "clock"],
  ["repos", "/repos", "g r", "branch"],
  ["audit", "/audit", "g a", "alert"],
  ["edge", "/edge", "g x", "link"],
  ["house", "/house", "g h", "sparkles"],
  ["claude", "/claude", "g u", "gauge"],
  ["events", "/events", "g e", "activity"],
  ["guard", "/guard", "g g", "eye"],
  ["settings", "/settings", "g c", "sliders"],
  ["system", "/system", "g y", "cpu"],
  ["docs", "/docs", "g d", "file"],
];
const NAV_GROUPS = { sites: "nav.groupManage", events: "nav.groupObserve" };

export const NAV_ICON = Object.fromEntries(NAV.map(([k, , , i]) => [k, i]));

function buildSidebar() {
  clear(sidebar);
  for (const [key, href, k, ico] of NAV) {
    if (NAV_GROUPS[key]) sidebar.append(h("div", { class: "group" }, t(NAV_GROUPS[key])));
    sidebar.append(h("a", { href: "#" + href, "data-path": href },
      icon(ico), h("span", { class: "label" }, t("nav." + key)), h("span", { class: "k", "aria-hidden": "true" }, k)));
  }
  sidebar.append(h("div", { class: "group" }, t("nav.session")));
  const logout = h("a", { href: "#/logout" }, icon("logout"), h("span", { class: "label" }, t("auth.signOut")));
  logout.addEventListener("click", async (e) => {
    e.preventDefault();
    try { await api.post("/auth/logout", {}, { allow401: true }); } catch { /* the cookie is gone either way */ }
    showAuth("login");
  });
  sidebar.append(logout);
  markNav();
}

function markNav() {
  const path = currentPath();
  for (const a of sidebar.querySelectorAll("a[data-path]")) {
    const on = path === a.dataset.path || path.startsWith(a.dataset.path + "/") || path.startsWith(a.dataset.path + "?");
    if (on) a.setAttribute("aria-current", "page"); else a.removeAttribute("aria-current");
  }
  closeMenu();
}

function openMenu() { sidebar.classList.add("open"); backdrop.hidden = false; menuBtn.setAttribute("aria-expanded", "true"); }
function closeMenu() { sidebar.classList.remove("open"); backdrop.hidden = true; menuBtn.setAttribute("aria-expanded", "false"); }
menuBtn.addEventListener("click", () => (sidebar.classList.contains("open") ? closeMenu() : openMenu()));
backdrop.addEventListener("click", closeMenu);

function themeLabel() {
  const v = getTheme();
  clear(themeBtn).append(icon(v === "dark" ? "moon" : v === "light" ? "sun" : "monitor"));
  themeBtn.setAttribute("aria-label", t("theme.button", { theme: t("theme." + v) }));
  themeBtn.title = themeBtn.getAttribute("aria-label");
}
themeBtn.addEventListener("click", () => {
  const order = ["auto", "light", "dark"];
  setTheme(order[(order.indexOf(getTheme()) + 1) % order.length]);
  themeLabel();
});
helpBtn.addEventListener("click", showShortcuts);

store.subscribe("streamState", (s) => {
  streamState.dataset.state = s;
  streamState.textContent = s === "live" ? t("stream.live") : s === "reconnecting" ? t("stream.reconnecting") : s === "connecting" ? t("stream.connecting") : "";
});

// ---- routes ----
route("/sites", sitesScreen);
route("/sites/:domain/:tab?", siteScreen);
route("/inbound", inboundScreen);
route("/waf", wafScreen);
route("/modules", modulesScreen);
route("/tasks", tasksScreen);
route("/events", eventsScreen);
route("/guard", guardScreen);
route("/repos", reposScreen);
route("/audit", auditScreen);
route("/edge/:tab?", edgeScreen);
route("/docs/:name?", docsScreen);
route("/settings", settingsScreen);
route("/system", systemScreen);
route("/house", houseScreen);
route("/claude", claudeUsageScreen);

let authView = null;
let authed = false;
let mounted = false;

// The skip link targets #main, which the hash router would otherwise read
// as a route; focusing by hand keeps the hash as it is.
document.querySelector(".skip").addEventListener("click", (e) => { e.preventDefault(); main.focus(); });

function showAuth(mode) {
  authed = false;
  disconnect();
  app.dataset.state = "auth";
  sidebar.hidden = true;
  topbar.hidden = false;
  clear(main);
  authView = authScreen({ mode, onDone: boot });
  main.append(authView.el);
  document.title = t("app.title");
}

async function boot() {
  let me;
  try {
    me = await api.get("/auth/me", { allow401: true });
  } catch (e) {
    if (e.status === 401 || e.status === 403) me = { authenticated: false, ...(e.body || {}) };
    else { showAuth("login"); toast(e.message, "bad"); return; }
  }
  store.set("me", me);
  const needsSetup = me && (me.setup_pending || me.needs_setup);
  if (!me || !me.authenticated) { showAuth(needsSetup ? "setup" : "login"); return; }
  authed = true;
  app.dataset.state = "ready";
  document.querySelectorAll(".open-banner").forEach((b) => b.remove());
  if (me.open_until) main.before(openBanner(me.open_until));
  topbar.hidden = false;
  sidebar.hidden = false;
  buildSidebar();
  connect();
  if (!mounted) {
    mounted = true;
    mount(main, (path) => (path === "/" || path === "/logout" ? "/sites" : path));
  } else {
    rerender();
  }
}

onUnauthorized(() => { if (authed) { showAuth("login"); toast(t("auth.expired"), "bad"); } });
document.addEventListener("route", () => {
  markNav();
  const path = currentPath();
  const key = NAV.find(([, href]) => path === href || path.startsWith(href + "/"))?.[0];
  const site = /^\/sites\/([^/?]+)/.exec(path);
  document.title = (site ? decodeURIComponent(site[1]) : key ? t("nav." + key) : "") + " · " + t("app.title");
});
installShortcuts();

(async () => {
  try {
    await loadStrings(getLang());
  } catch (e) {
    main.textContent = "Cannot load the string catalog: " + e.message;
    return;
  }
  themeLabel();
  clear(helpBtn).append(icon("help"));
  helpBtn.setAttribute("aria-label", t("keys.title"));
  menuBtn.setAttribute("aria-label", t("nav.menu"));
  await boot();
})();

// The worker makes the panel installable and is where push lands. Only on
// a secure origin, where browsers allow one; a failure to register is the
// panel without those two things, never the panel not working.
if ("serviceWorker" in navigator && window.isSecureContext) {
  navigator.serviceWorker.register("/sw.js", { scope: "/" }).catch((e) => console.warn("service worker:", e.message));
}

// Exposed for the console: handy when debugging a live installation.
window.mw = { api, store, navigate, GOTO };
