package secrets

import (
	"encoding/base64"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAValueRoundTripsAndNeverEncryptsTheSameTwice(t *testing.T) {
	t.Setenv(EnvKey, "")
	box, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a, err := box.Encrypt("cf-token-123")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := box.Encrypt("cf-token-123")
	if a == b {
		t.Fatal("two encryptions of the same value must differ (random nonce)")
	}
	if !strings.HasPrefix(a, "v1:") {
		t.Fatalf("want a v1: prefix, got %q", a)
	}
	got, err := box.Decrypt(a)
	if err != nil || got != "cf-token-123" {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestTheKeyFileIsCreatedOwnerOnlyAndReusedAcrossOpens(t *testing.T) {
	t.Setenv(EnvKey, "")
	root := t.TempDir()
	first, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(root, "data", KeyFile))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("key file mode %o, want 0600", info.Mode().Perm())
	}
	sealed, _ := first.Encrypt("x")
	second, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := second.Decrypt(sealed); err != nil || got != "x" {
		t.Fatalf("a second Open must read the same key: %q, %v", got, err)
	}
}

func TestTheEnvironmentKeyWinsOverTheFileInHexOrBase64(t *testing.T) {
	root := t.TempDir()
	key := []byte("0123456789abcdef0123456789abcdef")
	t.Setenv(EnvKey, hex.EncodeToString(key))
	hexBox, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "data", KeyFile)); err == nil {
		t.Fatal("no key file must be written when the environment carries the key")
	}
	t.Setenv(EnvKey, base64.StdEncoding.EncodeToString(key))
	b64Box, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	sealed, _ := hexBox.Encrypt("same key")
	if got, err := b64Box.Decrypt(sealed); err != nil || got != "same key" {
		t.Fatalf("hex and base64 spellings must give the same key: %q, %v", got, err)
	}
}

func TestAWrongKeyOrATamperedValueIsRefused(t *testing.T) {
	a, _ := New([]byte(strings.Repeat("a", 32)))
	b, _ := New([]byte(strings.Repeat("b", 32)))
	sealed, _ := a.Encrypt("secret")
	if _, err := b.Decrypt(sealed); err == nil {
		t.Fatal("another key must not decrypt")
	}
	tampered := sealed[:len(sealed)-2] + "AA"
	if _, err := a.Decrypt(tampered); err == nil {
		t.Fatal("a tampered value must not decrypt")
	}
	if _, err := a.Decrypt("plain"); err != ErrNotEncrypted {
		t.Fatalf("a clear value must be reported as such, got %v", err)
	}
}

func TestAShortOrMalformedKeyIsRejected(t *testing.T) {
	if _, err := New([]byte("short")); err == nil {
		t.Fatal("a key that is not 32 bytes must be refused")
	}
	t.Setenv(EnvKey, "not-a-key")
	if _, err := Open(t.TempDir()); err == nil {
		t.Fatal("a malformed environment key must be refused, not silently replaced by a file")
	}
}
