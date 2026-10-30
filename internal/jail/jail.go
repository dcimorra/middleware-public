// Package jail runs a site's processes where the site is all there is.
//
// A site's process, its workers, its tasks, a command run from the panel,
// a deploy's build and a turn of its conversation all ran as the
// supervisor's user, and that user can read the whole installation:
// every other site, every home, the credentials box. The agent's own jail
// (internal/agent/jail.go) only asked the CLI to stay away from those
// paths, and `ls /srv/middleware/sites` from "run a command" listed the
// neighbours anyway. A delegate with code was a shell on the house.
//
// So the process starts in namespaces of its own (user, mount, pid, ipc,
// uts), unprivileged, the same uid inside as outside, and its root is a
// fresh tmpfs holding only what the plan names, each at its own path: the
// system read-only (/usr, /etc, the libraries), the site's folder and its
// HOME writable, a private /tmp, a /proc of its own pid namespace (no
// other process exists for it), a /dev of a few devices. Everything else
// is not denied: it is not there. Paths keep their names, so the
// environment the site was given (HOME, MIDDLEWARE_SITE_DIR, the API
// socket) means the same inside.
//
// The network is the machine's: a site listens on its loopback port and
// reaches the internet as before. What it reaches on the network is the
// API's business (tokens, the site-scoped socket), not the filesystem's.
//
// The parent re-executes this very binary with Arg; init_linux.go is what
// then runs as pid 1 of the new namespaces, builds the root, drops what it
// may not keep and starts the command. Any binary that imports this
// package answers to Arg (the package's init), tests included.
package jail

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Arg is the hidden first argument the jail's init runs under.
const Arg = "__jail"

// Mount is one path of the jail. Dst is where it appears inside, Src the
// real path outside (links resolved by the parent: inside the new
// namespace an absolute link would be read against the wrong root). Link
// makes Dst a symbolic link to Link instead (/bin -> usr/bin).
type Mount struct {
	Src   string `json:"src,omitempty"`
	Dst   string `json:"dst"`
	Write bool   `json:"write,omitempty"`
	Link  string `json:"link,omitempty"`
}

// Plan is what a jailed process sees and where it starts.
type Plan struct {
	Mounts []Mount `json:"mounts"`
	// Dir is the working directory inside; the parent's cmd.Dir by default.
	Dir string `json:"dir,omitempty"`
	// Hostname is what the process's uname says: the site's name, not the
	// machine's.
	Hostname string `json:"hostname,omitempty"`
}

// systemDirs are read-only in every jail: what a program needs to run.
// /etc whole rather than a list of its files: a list missed what a
// toolchain reads (alternatives, ld.so.cache, ssl), and nothing in /etc is
// a tenant's; /etc/shadow stays unreadable to this user inside as outside.
// The installation's root is not here: it lives under /srv or a home.
var systemDirs = []string{"/usr", "/bin", "/sbin", "/lib", "/lib32", "/lib64", "/libx32", "/etc", "/opt"}

// System is a plan with the system's directories in it, read-only.
func System(hostname string) *Plan {
	p := &Plan{Hostname: hostname}
	for _, d := range systemDirs {
		st, err := os.Lstat(d)
		if err != nil {
			continue
		}
		if st.Mode()&os.ModeSymlink != 0 {
			// Merged /usr: /bin is a link to usr/bin. Kept as a link, so a
			// path through it resolves inside to the /usr that is there.
			if target, err := os.Readlink(d); err == nil {
				p.Mounts = append(p.Mounts, Mount{Dst: d, Link: target})
			}
			continue
		}
		p.Read(d)
	}
	// Ubuntu's resolv.conf is a link into /run, which is not there: the
	// folder it points to comes too, or no name resolves.
	if real, err := filepath.EvalSymlinks("/etc/resolv.conf"); err == nil && !strings.HasPrefix(real, "/etc/") {
		p.Read(filepath.Dir(real))
	}
	return p
}

// Read adds paths read-only. A path that does not exist is left out.
func (p *Plan) Read(paths ...string) *Plan { return p.add(false, paths) }

// Write adds paths read-write. A path that does not exist is left out.
func (p *Plan) Write(paths ...string) *Plan { return p.add(true, paths) }

func (p *Plan) add(write bool, paths []string) *Plan {
	for _, path := range paths {
		if path == "" || !filepath.IsAbs(path) {
			continue
		}
		dst := filepath.Clean(path)
		src, err := filepath.EvalSymlinks(dst)
		if err != nil {
			continue
		}
		// A path the plan already shows through a folder of its (a PATH
		// entry under /opt, /opt being there) needs nothing more to be
		// read, and is mounted again only to be written; then at its real
		// name, which is where a link on the way to it leads inside.
		if !p.Sees(dst) {
			p.put(Mount{Src: src, Dst: dst, Write: write})
		} else if write {
			p.put(Mount{Src: src, Dst: src, Write: write})
		}
		if src != dst && (write || !p.Sees(src)) {
			// By its real name too: a program that resolves its own
			// folder (realpath, process.cwd() after a chdir through the
			// link) must find it.
			p.put(Mount{Src: src, Dst: src, Write: write})
		}
	}
	return p
}

// put adds m, or makes the mount already at m.Dst writable when m is.
func (p *Plan) put(m Mount) {
	for i := range p.Mounts {
		if p.Mounts[i].Dst == m.Dst {
			p.Mounts[i].Write = p.Mounts[i].Write || m.Write
			return
		}
	}
	p.Mounts = append(p.Mounts, m)
}

// Sees says whether path is inside something the plan shows.
func (p *Plan) Sees(path string) bool { return p.sees(filepath.Clean(path), 0) }

// sees follows the plan's own links: /bin/sh is seen through /bin → usr/bin
// and /usr. Before, it was not, so a command named /bin/sh was lent as a
// mount of its own at /bin/sh, which sorts before /usr: its folder was
// made through the link before the /usr it points to was there, and every
// such command failed with "mkdir …/bin: file exists". The depth bounds a
// loop of links, which the system's directories never make.
func (p *Plan) sees(path string, depth int) bool {
	for _, m := range p.Mounts {
		if m.Link == "" && (path == m.Dst || strings.HasPrefix(path, m.Dst+"/")) {
			return true
		}
	}
	if depth > 8 {
		return false
	}
	for _, m := range p.Mounts {
		if m.Link != "" && strings.HasPrefix(path, m.Dst+"/") {
			target := m.Link
			if !filepath.IsAbs(target) {
				target = filepath.Join(filepath.Dir(m.Dst), target)
			}
			if p.sees(filepath.Join(target, strings.TrimPrefix(path, m.Dst)), depth+1) {
				return true
			}
		}
	}
	return false
}

// sorted is the mounts parents first: a folder lent inside another is
// mounted over it, not under it.
func (p *Plan) sorted() []Mount {
	out := append([]Mount(nil), p.Mounts...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Dst < out[j].Dst })
	return out
}

func (p *Plan) encode() (string, error) {
	raw, err := json.Marshal(p)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func decode(s string) (*Plan, error) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return nil, err
	}
	var p Plan
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, err
	}
	return &p, nil
}
