package deps

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha512"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dcimorra/the-middleware/internal/events"
)

func tarGz(t *testing.T, name string, body []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg})
	tw.Write(body)
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

func TestAManifestBuildIsInstalledOnlyWhenItsSHA512Matches(t *testing.T) {
	archive := tarGz(t, "antigravity", []byte("#!/bin/sh\necho 1.4.6\n"))
	sum := sha512.Sum512(archive)
	good := hex.EncodeToString(sum[:])
	sha := good
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/manifests/"):
			w.Write([]byte(`{"version":"1.4.6","url":"https://example.test/cli_linux.tar.gz","sha512":"` + sha + `"}`))
		default:
			w.Write(archive)
		}
	}))
	defer srv.Close()
	old := Download
	Download = &http.Client{Transport: hostTo{srv.URL}}
	defer func() { Download = old }()

	m := New(t.TempDir(), events.New(""))
	p := Package{Name: "agy", Binary: &Binary{Manifest: "https://example.test/manifests/linux_{arch}.json", Member: "antigravity", Arch: map[string]string{"amd64": "amd64", "arm64": "arm64"}}}
	// A download that is not what the manifest says is refused and leaves
	// nothing behind.
	sha = strings.Repeat("0", 128)
	if _, err := m.installBinary(context.Background(), p, "latest", func(string) {}); err == nil || !strings.Contains(err.Error(), "sha512") {
		t.Fatalf("a wrong sha512 was accepted: %v", err)
	}
	if _, err := os.Stat(filepath.Join(m.BinDir(), "agy")); err == nil {
		t.Fatal("a refused download was installed")
	}
	sha = good
	v, err := m.installBinary(context.Background(), p, "latest", func(string) {})
	if err != nil || v != "1.4.6" {
		t.Fatalf("install: %q %v", v, err)
	}
	raw, err := os.ReadFile(filepath.Join(m.BinDir(), "agy"))
	if err != nil || !strings.Contains(string(raw), "1.4.6") {
		t.Fatalf("the member was not extracted: %q %v", raw, err)
	}
	if m.installedVersion(p) != "1.4.6" {
		t.Fatalf("version not recorded: %q", m.installedVersion(p))
	}
	if latest, err := m.binaryLatest(context.Background(), p.Binary); err != nil || latest != "1.4.6" {
		t.Fatalf("latest: %q %v", latest, err)
	}
	if _, err := m.installBinary(context.Background(), p, "1.0.0", func(string) {}); err == nil {
		t.Fatal("a version the manifest does not name was installed")
	}
}

// hostTo sends every request to base, whatever its host.
type hostTo struct{ base string }

func (h hostTo) RoundTrip(req *http.Request) (*http.Response, error) {
	nr, _ := http.NewRequestWithContext(req.Context(), req.Method, h.base+req.URL.Path, req.Body)
	return http.DefaultTransport.RoundTrip(nr)
}
