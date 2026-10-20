package archive

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestAPackedFolderComesBackAsItWas(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "a.test")
	os.MkdirAll(filepath.Join(dir, "storage", "deep"), 0o755)
	os.MkdirAll(filepath.Join(dir, "node_modules", "x"), 0o755)
	os.WriteFile(filepath.Join(dir, "site.json"), []byte(`{"version":2}`), 0o644)
	os.WriteFile(filepath.Join(dir, "storage", "deep", "data.bin"), bytes.Repeat([]byte("x"), 5000), 0o600)
	os.WriteFile(filepath.Join(dir, "node_modules", "x", "i.js"), []byte("1"), 0o644)
	os.Symlink("/etc/hostname", filepath.Join(dir, "link"))
	syscall.Mkfifo(filepath.Join(dir, "pipe"), 0o644)
	dest := filepath.Join(root, "a.tar.gz")
	rep, err := Pack(context.Background(), dir, dest)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Files != 3 || rep.Links != 1 || len(rep.Skipped) != 1 {
		t.Fatalf("report %+v", rep)
	}
	back := filepath.Join(root, "restored")
	if _, err := Unpack(context.Background(), dest, back); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(back, "storage", "deep", "data.bin")); len(b) != 5000 {
		t.Fatal("the data did not come back: an archive is faithful, node_modules and storage included")
	}
	if l, err := os.Readlink(filepath.Join(back, "link")); err != nil || l != "/etc/hostname" {
		t.Fatalf("the link was not kept as a link: %q %v", l, err)
	}
	if _, err := Unpack(context.Background(), dest, back); err == nil {
		t.Fatal("restored over an existing folder")
	}
}

func evilTar(t *testing.T, entries func(tw *tar.Writer)) string {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	entries(tw)
	tw.Close()
	gz.Close()
	p := filepath.Join(t.TempDir(), "evil.tar.gz")
	os.WriteFile(p, buf.Bytes(), 0o644)
	return p
}

func TestNoNameInTheArchiveWritesOutsideTheFolder(t *testing.T) {
	outside := t.TempDir()
	file := func(tw *tar.Writer, name string) {
		tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: 1, Typeflag: tar.TypeReg})
		tw.Write([]byte("x"))
	}
	cases := map[string]func(tw *tar.Writer){
		"dot-dot":  func(tw *tar.Writer) { file(tw, "../escaped") },
		"absolute": func(tw *tar.Writer) { file(tw, "/tmp/escaped") },
		"through a link": func(tw *tar.Writer) {
			tw.WriteHeader(&tar.Header{Name: "evil", Typeflag: tar.TypeSymlink, Linkname: outside})
			file(tw, "evil/escaped")
		},
	}
	for name, entries := range cases {
		dest := filepath.Join(t.TempDir(), "site")
		if _, err := Unpack(context.Background(), evilTar(t, entries), dest); err == nil {
			t.Errorf("%s: restored", name)
		}
		if _, err := os.Stat(filepath.Join(outside, "escaped")); err == nil {
			t.Fatalf("%s: a file was written outside", name)
		}
		if _, err := os.Stat(dest); err == nil {
			t.Errorf("%s: a half-restored folder was left", name)
		}
	}
}

func TestADownloadLeavesOutWhatABuildGivesBack(t *testing.T) {
	dir := t.TempDir()
	for _, f := range []string{"src/app.js", ".git/HEAD", "node_modules/r/i.js", ".next-20261020/x", "dist/a.js"} {
		os.MkdirAll(filepath.Dir(filepath.Join(dir, f)), 0o755)
		os.WriteFile(filepath.Join(dir, f), []byte("1"), 0o644)
	}
	var buf bytes.Buffer
	if _, err := Zip(context.Background(), &buf, dir, false); err != nil {
		t.Fatal(err)
	}
	zr, _ := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	var names []string
	for _, f := range zr.File {
		names = append(names, f.Name)
	}
	got := strings.Join(names, ",")
	if got != ".git/HEAD,src/app.js" {
		t.Fatalf("zipped %s", got)
	}
	buf.Reset()
	Zip(context.Background(), &buf, dir, true)
	zr, _ = zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if len(zr.File) != 5 {
		t.Fatalf("all: %d files", len(zr.File))
	}
}
