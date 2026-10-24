package runtime

import (
	"context"
	"os"
	"path/filepath"
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

// NodeDir, when set, is the folder whose node a site's command runs with
// (nodeenv: what the site asks for, installed on the fly when the machine
// has none); empty is the machine's own. Set by the wiring; nil in tests.
var NodeDir func(ctx context.Context, siteDir string, argv []string) string

// withNode puts the site's Node first in its PATH when its command needs
// one, and says where bin is when it is one of Node's programs (exec
// resolves a bare name with the supervisor's PATH, not the child's).
func withNode(ctx context.Context, siteDir string, argv []string, env map[string]string) string {
	if NodeDir == nil || len(argv) == 0 {
		return ""
	}
	dir := NodeDir(ctx, siteDir, argv)
	if dir == "" {
		return ""
	}
	base := env["PATH"]
	if base == "" {
		base = os.Getenv("PATH")
	}
	env["PATH"] = dir + string(os.PathListSeparator) + base
	if !strings.ContainsRune(argv[0], '/') {
		if info, err := os.Stat(filepath.Join(dir, argv[0])); err == nil && !info.IsDir() {
			return filepath.Join(dir, argv[0])
		}
	}
	return ""
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
