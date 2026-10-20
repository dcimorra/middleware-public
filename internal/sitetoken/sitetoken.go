// Package sitetoken is the credential a site's process carries to the
// site door: "<domain>.<HMAC(key, domain)>". The token names its site, so
// there is no list to keep: the engine, which puts it in the process's
// environment, and the door, which checks it, derive the same thing from
// the same key, and deleting a site leaves no dead row anywhere. It still
// leaks with a `cat /proc/self/environ`; what it limits is how far it
// reaches: this door and this domain only. Deleting data/site-door.key
// rotates every token at once (sites pick theirs up when they restart).
package sitetoken

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/dcimorra/middleware-public/internal/fsx"
	"github.com/dcimorra/middleware-public/internal/manifest"
)

// Keys derives and checks tokens with one key under data/.
type Keys struct {
	path string
	mu   sync.Mutex
	key  []byte
}

// New uses data/site-door.key under dataDir, created on first use.
func New(dataDir string) *Keys { return &Keys{path: filepath.Join(dataDir, "site-door.key")} }

func (k *Keys) load() ([]byte, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.key != nil {
		return k.key, nil
	}
	data, err := os.ReadFile(k.path)
	if err != nil || len(data) < 32 {
		data = make([]byte, 32)
		if _, err := rand.Read(data); err != nil {
			return nil, err
		}
		if err := fsx.WriteAtomic(k.path, data, 0o600); err != nil {
			return nil, err
		}
	}
	k.key = data
	return data, nil
}

func sign(key []byte, domain string) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(domain))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// Token is the token of domain.
func (k *Keys) Token(domain string) (string, error) {
	key, err := k.load()
	if err != nil {
		return "", err
	}
	return domain + "." + sign(key, domain), nil
}

// Domain returns the domain a token names, or "" when it is not one of
// ours.
func (k *Keys) Domain(token string) string {
	i := strings.LastIndexByte(token, '.')
	if i <= 0 {
		return ""
	}
	domain, mac := token[:i], token[i+1:]
	if !manifest.ValidName(domain) {
		return ""
	}
	key, err := k.load()
	if err != nil {
		return ""
	}
	if !hmac.Equal([]byte(sign(key, domain)), []byte(mac)) {
		return ""
	}
	return domain
}
