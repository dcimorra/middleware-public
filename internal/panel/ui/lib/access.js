// What the person signed in may do. The operator may do everything; a
// delegated user has some sites, each with some permissions (/auth/me,
// user.sites), and the screens draw only what those open. The API is
// what refuses: this only keeps buttons that would be refused out of
// sight.

import { store } from "./store.js";

export function delegate() {
  const me = store.get("me");
  return !!(me && me.user);
}

// can says whether perm (view, operate, code, agents) is open on domain.
export function can(domain, perm) {
  const me = store.get("me");
  if (!me || !me.user) return true;
  const g = (me.user.sites || []).find((s) => s.site === domain);
  return !!g && (perm === "view" || g.perms.includes(perm));
}
