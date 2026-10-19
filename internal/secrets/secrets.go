// Package secrets encrypts the small values a module must keep (API tokens,
// webhook secrets) so data/modules.json can be backed up, copied and read
// with cat without handing out credentials. AES-256-GCM with a random nonce
// per value: the same token encrypted twice gives two different strings, so
// nothing can be learnt from two installations sharing a token.
//
// The key comes from MIDDLEWARE_SECRET_KEY (32 bytes as hex or base64) or,
// when that is not set, from data/modules-key, created on first use with
// owner-only permissions. The environment variable exists so a deployment
// that keeps its key in a secret store never writes one to disk.
package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/dcimorra/middleware-public/internal/fsx"
)

// EnvKey is the environment variable that can carry the key.
const EnvKey = "MIDDLEWARE_SECRET_KEY"

// KeyFile is the key file's name under data/.
const KeyFile = "modules-key"

// prefix marks an encrypted value and names the format, so a later change
// of cipher can tell old values apart instead of failing to decrypt them.
const prefix = "v1:"

// Box encrypts and decrypts with one key.
type Box struct {
	aead cipher.AEAD
}

// ErrNotEncrypted says a value lacks the prefix: it was written in clear,
// probably by hand, and the caller decides whether that is acceptable.
var ErrNotEncrypted = errors.New("value is not encrypted")

// New builds a Box from a 32-byte key.
func New(key []byte) (*Box, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("secrets: key must be 32 bytes, got %d", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Box{aead: aead}, nil
}

// Open returns the Box for an installation root: the environment's key when
// set, otherwise the key file, created if missing. The file is written with
// mode 0600 through an atomic rename, and re-read after writing: if the
// supervisor and the CLI start at the same instant and both create a key,
// the second rename wins and both end up using the file's key, so no value
// is ever encrypted with a key that is not on disk.
func Open(root string) (*Box, error) {
	if v := os.Getenv(EnvKey); v != "" {
		key, err := ParseKey(v)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", EnvKey, err)
		}
		return New(key)
	}
	path := filepath.Join(root, "data", KeyFile)
	key, err := readKeyFile(path)
	if fsx.IsNotExist(err) {
		fresh := make([]byte, 32)
		if _, err := rand.Read(fresh); err != nil {
			return nil, err
		}
		if err := writeKeyFile(path, fresh); err != nil {
			return nil, err
		}
		key, err = readKeyFile(path)
	}
	if err != nil {
		return nil, err
	}
	return New(key)
}

// ParseKey accepts a 32-byte key written as hex (64 chars) or base64
// (standard or URL, padded or not).
func ParseKey(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	if len(s) == 64 {
		if key, err := hex.DecodeString(s); err == nil {
			return key, nil
		}
	}
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if key, err := enc.DecodeString(s); err == nil && len(key) == 32 {
			return key, nil
		}
	}
	return nil, errors.New("want 32 bytes as hex or base64")
}

// Encrypt returns "v1:" plus base64 of nonce||ciphertext.
func (b *Box) Encrypt(plain string) (string, error) {
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	sealed := b.aead.Seal(nonce, nonce, []byte(plain), nil)
	return prefix + base64.StdEncoding.EncodeToString(sealed), nil
}

// Decrypt reverses Encrypt. A value without the prefix returns
// ErrNotEncrypted so a store that was edited by hand is reported, not
// silently accepted as ciphertext.
func (b *Box) Decrypt(value string) (string, error) {
	if !IsEncrypted(value) {
		return "", ErrNotEncrypted
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(value, prefix))
	if err != nil {
		return "", fmt.Errorf("secrets: %w", err)
	}
	n := b.aead.NonceSize()
	if len(raw) < n {
		return "", errors.New("secrets: value too short")
	}
	plain, err := b.aead.Open(nil, raw[:n], raw[n:], nil)
	if err != nil {
		return "", errors.New("secrets: cannot decrypt (wrong key or corrupted value)")
	}
	return string(plain), nil
}

// IsEncrypted reports whether value carries the format prefix.
func IsEncrypted(value string) bool { return strings.HasPrefix(value, prefix) }

// fileMu serialises the key file inside one process; across processes the
// atomic rename does the job.
var fileMu sync.Mutex

func readKeyFile(path string) ([]byte, error) {
	fileMu.Lock()
	defer fileMu.Unlock()
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	key, err := ParseKey(string(data))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return key, nil
}

func writeKeyFile(path string, key []byte) error {
	fileMu.Lock()
	defer fileMu.Unlock()
	return fsx.WriteAtomic(path, []byte(hex.EncodeToString(key)+"\n"), 0o600)
}
