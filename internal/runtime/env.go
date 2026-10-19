package runtime

import (
	"os"
	"sort"
	"strconv"
	"strings"
)

// buildEnv composes a child's environment. Two rules are not negotiable:
//
//   - no duplicate keys: glibc's getenv returns the first occurrence and
//     Node's process.env keeps the last, so a duplicated key means two
//     runtimes read two different values from the same environment;
//   - PORT goes last, after everything the manifest could set, so a
//     site's own .env can never override the port the engine chose.
//
// The base is a short allow-list from the supervisor's environment (PATH,
// locale), never the whole thing: a site must not inherit the supervisor's
// secrets by accident.
func buildEnv(spec map[string]string, port int, domain, siteDir, home string) []string {
	env := map[string]string{}
	for _, k := range []string{"PATH", "LANG", "LC_ALL", "TZ"} {
		if v, ok := os.LookupEnv(k); ok {
			env[k] = v
		}
	}
	for k, v := range spec {
		env[k] = v
	}
	env["HOME"] = home
	env["HOST"] = "127.0.0.1"
	env["MIDDLEWARE_DOMAIN"] = domain
	env["MIDDLEWARE_SITE_DIR"] = siteDir
	delete(env, "PORT")
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys)+1)
	for _, k := range keys {
		out = append(out, k+"="+env[k])
	}
	return append(out, "PORT="+strconv.Itoa(port))
}

// hasDuplicateKeys is used by tests and by the process runtime's sanity
// check before exec.
func hasDuplicateKeys(env []string) bool {
	seen := map[string]bool{}
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		if seen[k] {
			return true
		}
		seen[k] = true
	}
	return false
}
