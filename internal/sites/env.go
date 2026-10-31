package sites

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/dcimorra/the-middleware/internal/manifest"
)

// Masked replaces the value of any variable whose name suggests a secret.
const Masked = "••••••••"

var secretHints = []string{"KEY", "SECRET", "TOKEN", "PASSWORD", "PASSWD", "CREDENTIAL"}

// IsSecretKey reports whether a variable name looks like it holds a secret.
// The list errs on the side of masking: a DATABASE_URL with a password in
// it is not caught, which is why the panel labels this view "as resolved"
// and not "safe to paste".
func IsSecretKey(k string) bool {
	u := strings.ToUpper(k)
	for _, h := range secretHints {
		if strings.Contains(u, h) {
			return true
		}
	}
	return false
}

// ResolvedEnv returns the environment the instance is born with, resolved
// the way the engine resolves it (env_file, env, env_from_file, the
// variables the runtime adds), with secrets masked. A site without a run
// section has no environment and gets an empty map. For a site with
// services, service names which one's (empty is the one that answers the
// site's name).
func (m *Manager) ResolvedEnv(domain string, service ...string) (map[string]string, error) {
	dir, err := m.managedDir(domain)
	if err != nil {
		return nil, err
	}
	mf, err := manifest.Load(dir)
	if err != nil {
		return nil, err
	}
	name := ""
	if len(service) > 0 {
		name = service[0]
	}
	if mf.Run == nil && len(mf.Services) == 0 {
		if name != "" {
			return nil, fmt.Errorf("%w: %s has no services", ErrNotFound, domain)
		}
		return map[string]string{}, nil
	}
	var spec *manifest.Spec
	port := 0
	if len(mf.Services) > 0 {
		if spec, err = m.execSpec(domain, dir, name, mf); err != nil {
			return nil, err
		}
		if sn, ok := m.lookup(domain); ok {
			port = sn.Services[spec.Service].Port
		}
		if spec.ContainerPort != 0 {
			port = spec.ContainerPort // what $PORT is inside its container
		}
	} else {
		if name != "" {
			return nil, fmt.Errorf("%w: %s has no services", ErrNotFound, domain)
		}
		if spec, err = manifest.Resolve(domain, dir, mf); err != nil {
			return nil, err
		}
		if sn, ok := m.lookup(domain); ok {
			port = sn.Port
		}
	}
	_, env := spec.WithPort(port)
	env["HOME"] = filepath.Join(m.Root, "data", "homes", domain)
	env["HOST"] = "127.0.0.1"
	env["MIDDLEWARE_DOMAIN"] = domain
	env["MIDDLEWARE_SITE_DIR"] = dir
	if port > 0 {
		env["PORT"] = strconv.Itoa(port)
	} else {
		env["PORT"] = "$PORT"
	}
	for k := range env {
		if IsSecretKey(k) && env[k] != "" {
			env[k] = Masked
		}
	}
	return env, nil
}
