package vpn

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

	"github.com/dcimorra/middleware-public/internal/fsx"
)

// FileSecrets is AES-256-GCM keyed from MIDDLEWARE_SECRET_KEY (64 hex
// characters) or, failing that, data/modules-key, created owner-only on
// first use. It exists so the CLI can manage peers without the modules
// package; the registry hands the module its own Secrets, which must use
// the same key source or the stored private key becomes unreadable.
type FileSecrets struct {
	aead cipher.AEAD
}

// KeyFile is where the installation's key lives when the variable is unset.
func KeyFile(root string) string { return filepath.Join(root, "data", "modules-key") }

// NewFileSecrets loads or creates the key.
func NewFileSecrets(root string) (*FileSecrets, error) {
	var key []byte
	if v := strings.TrimSpace(os.Getenv("MIDDLEWARE_SECRET_KEY")); v != "" {
		raw, err := hex.DecodeString(v)
		if err != nil || len(raw) != 32 {
			return nil, errors.New("MIDDLEWARE_SECRET_KEY must be 64 hex characters")
		}
		key = raw
	} else {
		path := KeyFile(root)
		text, err := fsx.ReadTrimmed(path)
		switch {
		case err == nil:
			if key, err = hex.DecodeString(text); err != nil || len(key) != 32 {
				return nil, fmt.Errorf("%s: not 64 hex characters", path)
			}
		case fsx.IsNotExist(err):
			key = make([]byte, 32)
			if _, err := rand.Read(key); err != nil {
				return nil, err
			}
			if err := fsx.WriteAtomic(path, []byte(hex.EncodeToString(key)+"\n"), 0o600); err != nil {
				return nil, err
			}
		default:
			return nil, err
		}
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &FileSecrets{aead: aead}, nil
}

// Encrypt returns base64(nonce || ciphertext).
func (s *FileSecrets) Encrypt(plain []byte) (string, error) {
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	out := s.aead.Seal(nonce, nonce, plain, nil)
	return base64.StdEncoding.EncodeToString(out), nil
}

// Decrypt reverses Encrypt.
func (s *FileSecrets) Decrypt(stored string) ([]byte, error) {
	raw, err := base64.StdEncoding.DecodeString(stored)
	if err != nil {
		return nil, err
	}
	n := s.aead.NonceSize()
	if len(raw) < n {
		return nil, errors.New("ciphertext too short")
	}
	return s.aead.Open(nil, raw[:n], raw[n:], nil)
}
