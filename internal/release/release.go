// Package release knows where this program's releases are and how their
// versions compare. Both sides of an update use it: the supervisor, to
// say a newer one exists, and the privileged helper, which installs it and
// trusts nothing the supervisor says beyond the version's name.
package release

import (
	"regexp"
	"strconv"
	"strings"
)

// Repo is where the releases are published. The helper downloads from
// here and nowhere else: a URL in a request would make the socket a way
// to put any executable in /usr/local/bin as root.
const Repo = "dcimorra/middleware-public"

// DownloadBase is the release download prefix; a variable for tests.
var DownloadBase = "https://github.com/" + Repo + "/releases/download"

// APIBase is GitHub's API, where the list of releases is read; a variable
// for tests.
var APIBase = "https://api.github.com/repos/" + Repo

var tagRe = regexp.MustCompile(`^v(\d+)\.(\d+)\.(\d+)(?:-([0-9A-Za-z.-]+))?$`)

// Valid says whether tag names a release: v1.2.3 or v1.2.3-alpha.4.
func Valid(tag string) bool { return tagRe.MatchString(tag) }

// Asset is the binary of a release for an architecture.
func Asset(arch string) string { return "middleware-linux-" + arch }

// URL is where a release's asset is downloaded from.
func URL(tag, asset string) string { return DownloadBase + "/" + tag + "/" + asset }

// Newer reports whether a is a later release than b, by semver: a
// pre-release is earlier than the release it precedes, and its parts
// compare numerically when both are numbers (alpha.10 after alpha.9).
// A name that is not a release (a "dev" build) is never newer, and
// anything valid is newer than it.
func Newer(a, b string) bool {
	ma, mb := tagRe.FindStringSubmatch(a), tagRe.FindStringSubmatch(b)
	if ma == nil {
		return false
	}
	if mb == nil {
		return true
	}
	for i := 1; i <= 3; i++ {
		x, _ := strconv.Atoi(ma[i])
		y, _ := strconv.Atoi(mb[i])
		if x != y {
			return x > y
		}
	}
	pa, pb := ma[4], mb[4]
	switch {
	case pa == pb:
		return false
	case pa == "":
		return true
	case pb == "":
		return false
	}
	xs, ys := strings.Split(pa, "."), strings.Split(pb, ".")
	for i := 0; i < len(xs) && i < len(ys); i++ {
		if xs[i] == ys[i] {
			continue
		}
		x, errx := strconv.Atoi(xs[i])
		y, erry := strconv.Atoi(ys[i])
		switch {
		case errx == nil && erry == nil:
			return x > y
		case errx == nil:
			return false
		case erry == nil:
			return true
		}
		return xs[i] > ys[i]
	}
	return len(xs) > len(ys)
}
