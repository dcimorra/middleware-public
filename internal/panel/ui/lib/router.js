// Hash router. The panel is served from one index.html with no server
// routes, so the hash is the only part of the URL that is ours: a deep
// link survives a reload, a copy-paste and the back button, and the
// server never has to know which screens exist.

const routes = []; // {pattern: RegExp, keys: [], handler}
let current = null; // {destroy}
let outlet = null;
let beforeEach = null;

export function route(pattern, handler) {
  // One pass, left to right, so keys line up with the capture groups: two
  // passes (optional first, then required) pushed :tab before :domain and
  // every site screen opened "undefined".
  const keys = [];
  const re = pattern.replace(/\/:(\w+)(\?)?/g, (_, k, opt) => {
    keys.push(k);
    return opt ? "(?:/([^/]+))?" : "/([^/]+)";
  });
  routes.push({ pattern: new RegExp("^" + re + "$"), keys, handler });
}

export function mount(el, guard) {
  outlet = el;
  beforeEach = guard;
  window.addEventListener("hashchange", render);
  render();
}

export function navigate(path, replaceHistory) {
  const target = "#" + (path.startsWith("/") ? path : "/" + path);
  if (replaceHistory) history.replaceState(null, "", target);
  else location.hash = target;
  if (replaceHistory) render();
}

export function currentPath() {
  const h = location.hash.replace(/^#/, "");
  return h || "/";
}

export function render() {
  if (!outlet) return;
  const path = currentPath();
  const guarded = beforeEach ? beforeEach(path) : null;
  if (guarded && guarded !== path) {
    history.replaceState(null, "", "#" + guarded);
    return render();
  }
  const [clean, query = ""] = path.split("?");
  for (const r of routes) {
    const m = clean.match(r.pattern);
    if (!m) continue;
    const params = {};
    r.keys.forEach((k, i) => { params[k] = m[i + 1] ? decodeURIComponent(m[i + 1]) : undefined; });
    params.query = new URLSearchParams(query);
    swap(r.handler, params);
    return;
  }
  // An unknown path (a typo, an old bookmark) lands on the home screen
  // rather than on a blank page with nothing to click.
  if (clean !== "/sites") {
    history.replaceState(null, "", "#/sites");
    render();
  }
}

function swap(handler, params) {
  if (current && current.destroy) {
    try { current.destroy(); } catch (e) { console.error("destroy", e); }
  }
  current = null;
  while (outlet.firstChild) outlet.removeChild(outlet.firstChild);
  if (!handler) return;
  const view = handler(params);
  current = view;
  if (view && view.el) outlet.append(view.el);
  window.scrollTo(0, 0);
  document.dispatchEvent(new CustomEvent("route", { detail: { path: currentPath(), params } }));
}
