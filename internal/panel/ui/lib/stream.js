// One EventSource for the whole panel. Every list is live because it
// subscribes here, not because it polls: a second connection per screen
// would multiply the server's fan-out by the number of open tabs, and a
// poll would show a handover seconds after it happened.

import { api, apiURL, enc, signalUnauthorized } from "./api.js";
import { store } from "./store.js";
import { debounce } from "./dom.js";

let source = null;
const handlers = []; // {prefix, fn}

// What a reconnect of our own needs. The browser reconnects by itself
// only after a network error; any answer that is not a 200 event stream
// (the mesh door's 502 while a node restarts, a 401) leaves the
// EventSource CLOSED for good, and the panel heard nothing live again
// until a reload. lastId is handed back as ?last_event_id= so the server
// sends what was emitted meanwhile; a fresh EventSource cannot set the
// Last-Event-ID header the browser's own reconnect sends.
let lastId = "";
let retry = null; // the pending reconnect, from its timer to its open()
let delay = 0; // seconds before the next attempt: 1, 2, 4… up to 30
let generation = 0; // a disconnect() makes a reconnect in flight stale

export function onEvent(prefix, fn) {
  const entry = { prefix, fn };
  handlers.push(entry);
  return () => {
    const i = handlers.indexOf(entry);
    if (i >= 0) handlers.splice(i, 1);
  };
}

export function connect() {
  if (source || retry) return;
  open();
}

function open() {
  const es = new EventSource(apiURL("/events/stream" + (lastId ? "?last_event_id=" + enc(lastId) : "")), { withCredentials: true });
  source = es;
  store.set("streamState", delay ? "reconnecting" : "connecting");
  es.onopen = () => {
    delay = 0;
    store.set("streamState", "live");
  };
  es.onerror = () => {
    if (source !== es) return;
    // Still CONNECTING is the browser's own reconnect after a network
    // error, with the server's retry hint: only say so. CLOSED is the
    // browser giving up, and the reconnect is ours.
    store.set("streamState", "reconnecting");
    if (es.readyState !== EventSource.CLOSED) return;
    es.close();
    source = null;
    schedule();
  };
  es.onmessage = (m) => {
    if (m.lastEventId) lastId = m.lastEventId;
    let ev;
    try { ev = JSON.parse(m.data); } catch { return; }
    if (!ev || typeof ev !== "object" || !ev.type) return;
    store.pushEvent(ev);
    dispatch(ev);
  };
}

// schedule reconnects after the next delay. The session is asked first:
// a stream refused because the cookie died would be refused forever, and
// the login screen is what fixes that, through the same path a 401 takes
// anywhere else. A failed question (the supervisor itself restarting) is
// one more attempt later.
function schedule() {
  delay = delay ? Math.min(delay * 2, 30) : 1;
  const gen = generation;
  retry = setTimeout(async () => {
    let me = null;
    let denied = false;
    try {
      me = await api.get("/auth/me", { allow401: true });
    } catch (e) {
      denied = e.status === 401;
    }
    if (gen !== generation) return;
    retry = null;
    if (denied || (me && !me.authenticated)) {
      store.set("streamState", "closed");
      signalUnauthorized();
      return;
    }
    if (!me) {
      schedule();
      return;
    }
    open();
  }, delay * 1000);
}

export function disconnect() {
  generation++;
  clearTimeout(retry);
  retry = null;
  delay = 0;
  // Another node, or another session: an id from the stream that ends
  // here means nothing to the next one, and resuming from its time on
  // another machine would replay that machine's past as news.
  lastId = "";
  if (source) source.close();
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
