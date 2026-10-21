// Which node the panel is working on. Every call goes to it through this
// node's API (/nodes/{id}/api/…) unless it is about this node itself: the
// links and the operator's credentials are always this panel's own.
//
// Kept per tab, not per browser: two tabs on two machines is a normal way
// to work, and one tab switching under the other would be a click landing
// on the wrong machine.

const KEY = "mw.node";
let current = sessionStorage.getItem(KEY) || "";
const listeners = new Set();

export function currentNode() { return current; }

export function setCurrentNode(id) {
  id = id || "";
  if (id === current) return;
  current = id;
  if (id) sessionStorage.setItem(KEY, id); else sessionStorage.removeItem(KEY);
  for (const fn of [...listeners]) fn(id);
}

export function onNodeChange(fn) {
  listeners.add(fn);
  return () => listeners.delete(fn);
}

// Paths that always mean this node, whichever is chosen.
const LOCAL = ["/nodes", "/node/", "/auth/"];

// nodePath is where path goes for node (the current one when undefined;
// "" is this one).
export function nodePath(path, node) {
  const n = node === undefined ? current : node;
  if (!n || LOCAL.some((p) => path === p || path.startsWith(p.endsWith("/") ? p : p + "/") || path.startsWith(p + "?"))) return path;
  return `/nodes/${encodeURIComponent(n)}/api` + path;
}
