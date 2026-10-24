package auth

import (
	"errors"
	"fmt"
	"strings"
)

// A token with scopes is a service door: it opens a closed list of
// routes, optionally for one site, and nothing else. It exists for the
// programs that act on the house from outside (an app that builds a site
// and needs to restart it, a deploy script that reads logs), which V1
// gave a full token or a door of their own written route by route.
// A token without scopes is the operator's, and opens everything, as
// every token did before scopes existed.
//
// The list is closed and lives here, so what a scope opens is read in one
// place; the API asks Allows with the route it matched, never the caller.

// scopeRoutes are the routes each scope opens, as the API registers them.
var scopeRoutes = map[string][]string{
	"read": {
		"GET /api/v1/sites",
		"GET /api/v1/sites/{domain}",
		"GET /api/v1/sites/{domain}/logs",
		"GET /api/v1/events",
	},
	"restart": {
		"POST /api/v1/sites/{domain}/restart",
		"POST /api/v1/sites/{domain}/workers/{name}/restart",
	},
	"deploy": {
		"POST /api/v1/sites/{domain}/deploy",
	},
	// import is a site arriving from another installation (V1): its
	// bundle, its last copy, the commit, and seeing whether it came up or
	// removing it when it did not. Always for one site, and made to
	// expire (POST /relocate/import-token).
	"import": {
		"POST /api/v1/relocate/{domain}/check",
		"PUT /api/v1/relocate/{domain}",
		"PUT /api/v1/relocate/{domain}/delta",
		"POST /api/v1/relocate/{domain}/commit",
		"DELETE /api/v1/relocate/{domain}",
		"GET /api/v1/sites/{domain}",
		"DELETE /api/v1/sites/{domain}",
	},
}

// Scopes is the closed list, for the panel.
func Scopes() []string { return []string{"read", "restart", "deploy"} }

// ErrBadScope is a scope outside the list.
var ErrBadScope = errors.New("scope")

// CheckScopes validates "action" or "action:site" entries.
func CheckScopes(scopes []string, validSite func(string) bool) error {
	for _, sc := range scopes {
		action, site, hasSite := strings.Cut(sc, ":")
		if _, ok := scopeRoutes[action]; !ok {
			return fmt.Errorf("%w: %q is not one of %s", ErrBadScope, sc, strings.Join(Scopes(), ", "))
		}
		if hasSite && !validSite(site) {
			return fmt.Errorf("%w: %q names no valid site", ErrBadScope, sc)
		}
		if action == "import" && !hasSite {
			return fmt.Errorf("%w: import is for one site (import:<domain>)", ErrBadScope)
		}
	}
	return nil
}

// Allows says whether scopes open the route pattern (as the mux matched
// it, "POST /api/v1/sites/{domain}/restart") for the site in it, if any.
// Empty scopes open everything.
func Allows(scopes []string, pattern, site string) bool {
	if len(scopes) == 0 {
		return true
	}
	for _, sc := range scopes {
		action, only, hasSite := strings.Cut(sc, ":")
		if hasSite && only != site {
			continue
		}
		for _, p := range scopeRoutes[action] {
			if p == pattern {
				return true
			}
		}
	}
	return false
}

// ScopeRoutes is every route a scope opens, for the test that checks
// each one is still registered.
func ScopeRoutes() []string {
	var out []string
	for _, rs := range scopeRoutes {
		out = append(out, rs...)
	}
	return out
}
