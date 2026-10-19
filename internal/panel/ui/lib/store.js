// A small observable store. Screens subscribe to a key and re-render the
// part that changed; the stream writes here and nowhere else, so a site
// row in the list and the detail header of the same site are always the
// same object and can never disagree.

const state = {
  me: null,          // {authenticated, via, needs_setup}
  sites: new Map(),  // domain -> site
  sitesLoaded: false,
  events: [],        // newest last, capped
  lastEvent: new Map(), // subject -> event
  streamState: "idle",
};

const subs = new Map(); // key -> Set<fn>

export const store = {
  get(key) { return state[key]; },
  set(key, value) {
    state[key] = value;
    notify(key);
  },
  update(key, fn) {
    state[key] = fn(state[key]);
    notify(key);
  },
  subscribe(key, fn) {
    if (!subs.has(key)) subs.set(key, new Set());
    subs.get(key).add(fn);
    return () => subs.get(key).delete(fn);
  },

  // sites
  setSites(list) {
    const m = new Map();
    for (const s of list || []) m.set(s.domain, s);
    state.sites = m;
    state.sitesLoaded = true;
    notify("sites");
  },
  putSite(site) {
    if (!site || !site.domain) return;
    state.sites.set(site.domain, site);
    notify("sites");
    notify("site:" + site.domain);
  },
  dropSite(domain) {
    state.sites.delete(domain);
    notify("sites");
    notify("site:" + domain);
  },
  site(domain) { return state.sites.get(domain); },
  siteList() { return [...state.sites.values()].sort((a, b) => a.domain.localeCompare(b.domain)); },

  // events
  pushEvent(ev) {
    state.events.push(ev);
    if (state.events.length > 1000) state.events.splice(0, state.events.length - 1000);
    if (ev.subject) state.lastEvent.set(ev.subject, ev);
    notify("events");
  },
  seedEvents(list) {
    state.events = (list || []).slice(-1000);
    for (const ev of state.events) if (ev.subject) state.lastEvent.set(ev.subject, ev);
    notify("events");
  },
  lastEventFor(subject) { return state.lastEvent.get(subject); },
};

function notify(key) {
  const set = subs.get(key);
  if (!set) return;
  for (const fn of [...set]) {
    try { fn(state[key]); } catch (e) { console.error("store subscriber", key, e); }
  }
}
