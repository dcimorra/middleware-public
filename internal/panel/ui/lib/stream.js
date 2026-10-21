// One EventSource for the whole panel. Every list is live because it
// subscribes here, not because it polls: a second connection per screen
// would multiply the server's fan-out by the number of open tabs, and a
// poll would show a handover seconds after it happened.

import { api, apiURL, enc } from "./api.js";
import { store } from "./store.js";
import { debounce } from "./dom.js";

let source = null;
const handlers = []; // {prefix, fn}

export function onEvent(prefix, fn) {
  const entry = { prefix, fn };
  handlers.push(entry);
  return () => {
    const i = handlers.indexOf(entry);
    if (i >= 0) handlers.splice(i, 1);
  };
}

export function connect() {
  if (source) return;
  source = new EventSource(apiURL("/events/stream"), { withCredentials: true });
  store.set("streamState", "connecting");
  source.onopen = () => store.set("streamState", "live");
  source.onerror = () => {
    // EventSource reconnects by itself with the server's retry hint; the
    // only thing to do is say so, and if the cookie died the reconnect
    // keeps failing until the login screen fixes it.
    store.set("streamState", source.readyState === EventSource.CLOSED ? "closed" : "reconnecting");
  };
  source.onmessage = (m) => {
    let ev;
    try { ev = JSON.parse(m.data); } catch { return; }
    if (!ev || typeof ev !== "object" || !ev.type) return;
    store.pushEvent(ev);
    dispatch(ev);
  };
}

export function disconnect() {
  if (!source) return;
  source.close();
  source = null;
  store.set("streamState", "closed");
}

function dispatch(ev) {
  for (const { prefix, fn } of [...handlers]) {
    if (ev.type === prefix || ev.type.startsWith(prefix)) {
      try { fn(ev); } catch (e) { console.error("event handler", prefix, e); }
    }
  }
}

// Anything about a site refreshes that site's object from the API instead
// of guessing what changed from the event's data: the API is the one
// source of what a site is, and a stale guess would show a wrong badge
// until the next full load.
const refreshers = new Map();
function refreshSite(domain) {
  if (!domain) return;
  if (!refreshers.has(domain)) {
    refreshers.set(domain, debounce(async (d) => {
      try {
        const site = await api.get(`/sites/${enc(d)}`);
        store.putSite(site);
      } catch (e) {
        if (e.status === 404) store.dropSite(d);
      }
    }, 150));
  }
  refreshers.get(domain)(domain);
}

const siteEventPrefixes = ["site.", "instance.", "handover.", "worker.", "deploy.", "manifest.", "hold.", "waf.site"];

onEvent("", (ev) => {
  if (!ev.subject) return;
  if (siteEventPrefixes.some((p) => ev.type.startsWith(p))) {
    if (ev.type === "site.deleted" || ev.type === "site.archived" || ev.type === "site.removed") {
      store.dropSite(ev.subject);
      return;
    }
    refreshSite(ev.subject);
  }
});
