package security

import (
	"fmt"
	"path"
	"strings"
)

// Paths nobody serves on purpose. Scanners ask every domain alike for
// `/.ssh/id_rsa`, `/.env` or `/.git/config`, hundreds of times a day. In
// V1 they reached the site, which answered whatever it answered: one sent
// every unknown path to its login with a 303, and the guard, which counts
// a 2xx or 3xx to such a path as "the site gave something away", raised a
// suspicion over nothing. Cut here, the site never sees them, its log is
// not full of them and there is nothing to judge.
//
// The list is short on purpose: only what no website serves for real,
// because it applies to every site without asking. `wp-login.php`,
// `/admin` or `phpmyadmin` are not in it (a site may have them); a site
// adds those to its own `security.paths`.

// probeDirs hold credentials; matched as a whole segment anywhere in the
// path, so `/old/.git/HEAD` is cut like `/.git/config`.
var probeDirs = []string{".ssh", ".aws", ".git", ".svn", ".hg", ".docker", ".kube", ".gnupg", ".azure"}

// probeFiles are secrets by name, also as a whole segment.
var probeFiles = []string{
	"id_rsa", "id_rsa.pub", "id_dsa", "id_dsa.pub", "id_ecdsa", "id_ecdsa.pub",
	"id_ed25519", "id_ed25519.pub", "authorized_keys", "known_hosts",
	".htpasswd", ".git-credentials", ".npmrc", ".pgpass", ".netrc", ".s3cfg",
	".bash_history", ".zsh_history",
}

// probePrefixes are what a path traversal goes looking for.
var probePrefixes = []string{"/etc/passwd", "/etc/shadow", "/proc/self/"}

// BuiltinPaths is the house list in words, one entry per rule, for the
// panel and the docs.
func BuiltinPaths() []string {
	var out []string
	for _, d := range probeDirs {
		out = append(out, d+"/")
	}
	out = append(out, ".env, .env.*")
	out = append(out, probeFiles...)
	return append(out, probePrefixes...)
}

// clean is the path as compared: already decoded by net/http, lower-cased,
// with `//`, `/./` and `..` folded, so `//.ssh` and `/a/../.ssh` are no
// way around the list. A trailing slash is kept: `/proc/self/` is a folder.
func clean(p string) string {
	c := path.Clean("/" + strings.ToLower(p))
	if strings.HasSuffix(p, "/") && c != "/" {
		c += "/"
	}
	return c
}

// wellKnown is never cut: certificates and passkeys are read from it.
func wellKnown(p string) bool {
	return p == "/.well-known" || strings.HasPrefix(p, "/.well-known/")
}

// builtinMatch is the house rule a cleaned path falls under, or "".
func builtinMatch(p string) string {
	if wellKnown(p) {
		return ""
	}
	for _, pre := range probePrefixes {
		if p == strings.TrimSuffix(pre, "/") || strings.HasPrefix(p, pre) {
			return pre
		}
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "" {
			continue
		}
		if seg == ".env" || strings.HasPrefix(seg, ".env.") {
			return ".env"
		}
		for _, d := range probeDirs {
			if seg == d {
				return d + "/"
			}
		}
		for _, f := range probeFiles {
			if seg == f {
				return f
			}
		}
	}
	return ""
}

// readPaths reads a site's own prefixes, matched by whole segments. `/` alone would close the whole
// site and anything under `/.well-known` would break its certificate, so
// neither is accepted.
func readPaths(list []string) ([]string, error) {
	var out []string
	for _, raw := range list {
		v := strings.TrimSpace(raw)
		if v == "" {
			continue
		}
		if !strings.HasPrefix(v, "/") {
			return nil, fmt.Errorf("%q does not start with /", raw)
		}
		c := clean(v)
		if c == "/" {
			return nil, fmt.Errorf("%q would close the whole site", raw)
		}
		if wellKnown(c) {
			return nil, fmt.Errorf("%q is inside /.well-known, where certificates and passkeys are read", raw)
		}
		out = append(out, c)
	}
	return out, nil
}

// DecidePath looks at a request's path: the house list first, then the
// site's. What is the rule that matched, never the path itself, so the
// panel can tell which line of which list did it.
func (f *Filter) DecidePath(p string) Verdict {
	c := clean(p)
	if m := builtinMatch(c); m != "" {
		return Verdict{Reason: ByPath, What: m}
	}
	if f == nil || wellKnown(c) {
		return Verdict{Pass: true}
	}
	// Whole segments, the way access matches a zone: "/admin" closes
	// "/admin" and "/admin/x", not "/administration".
	for _, pre := range f.paths {
		base := strings.TrimSuffix(pre, "/")
		if strings.TrimSuffix(c, "/") == base || strings.HasPrefix(c, base+"/") {
			return Verdict{Reason: ByPath, What: pre}
		}
	}
	return Verdict{Pass: true}
}
