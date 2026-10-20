// Package archive keeps a site folder in one file and puts it back, and
// writes a folder as a zip to download.
//
// An archive is faithful or it is not an archive. There is no "only the
// code" here, unlike the download: the archive is what will be left, and
// V1's comparaloo kept eleven gigabytes that were not its dependencies but
// its storage folder. An archive that chose by name what deserved keeping
// would restore a site without its data, and without saying so.
//
// So: tar, where a symlink is one more entry (node_modules is full of
// them), stored as a link and never followed (following them is a walk
// through the machine, or a loop). What cannot be stored (a socket, a
// named pipe) is noted and skipped; a regular file that cannot be read
// stops the archive, because the folder is deleted after this and a byte
// missing would be nowhere. On restore, no name in the tar decides where
// it is written: "../../etc" is checked against the destination first,
// and nothing is written through a link the archive itself made.
package archive

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Report is what went in, or came out, and what did not.
type Report struct {
	Files   int      `json:"files"`
	Dirs    int      `json:"dirs"`
	Links   int      `json:"links"`
	Bytes   int64    `json:"bytes"`
	Skipped []string `json:"skipped,omitempty"`
}

func (r *Report) skip(what string) {
	if len(r.Skipped) < 20 {
		r.Skipped = append(r.Skipped, what)
	} else if len(r.Skipped) == 20 {
		r.Skipped = append(r.Skipped, "…and more")
	}
}

// Pack writes dir to dest as a .tar.gz, atomically: a half-written
// archive never has the final name.
func Pack(ctx context.Context, dir, dest string) (Report, error) {
	var rep Report
	tmp := dest + ".partial"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return rep, err
	}
	fail := func(err error) (Report, error) {
		f.Close()
		os.Remove(tmp)
		return rep, err
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	err = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		if rel == "." {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		mode := info.Mode()
		link := ""
		switch {
		case mode&fs.ModeSymlink != 0:
			if link, err = os.Readlink(p); err != nil {
				return err
			}
			rep.Links++
		case mode.IsDir():
			rep.Dirs++
		case mode.IsRegular():
			rep.Files++
			rep.Bytes += info.Size()
		default:
			rep.skip(filepath.ToSlash(rel) + " (" + mode.Type().String() + ")")
			return nil
		}
		hdr, err := tar.FileInfoHeader(info, link)
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(rel)
		if mode.IsDir() {
			hdr.Name += "/"
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if mode.IsRegular() {
			src, err := os.Open(p)
			if err != nil {
				return fmt.Errorf("%s cannot be read, and the folder would be deleted without it: %w", rel, err)
			}
			_, err = io.Copy(tw, src)
			src.Close()
			if err != nil {
				return fmt.Errorf("%s: %w", rel, err)
			}
		}
		return nil
	})
	if err != nil {
		return fail(err)
	}
	if err := tw.Close(); err != nil {
		return fail(err)
	}
	if err := gz.Close(); err != nil {
		return fail(err)
	}
	if err := f.Sync(); err != nil {
		return fail(err)
	}
	if err := f.Close(); err != nil {
		return fail(err)
	}
	return rep, os.Rename(tmp, dest)
}

// Unpack restores src into dir, which must not exist yet.
func Unpack(ctx context.Context, src, dir string) (Report, error) {
	var rep Report
	if _, err := os.Lstat(dir); err == nil {
		return rep, fmt.Errorf("%s already exists", dir)
	}
	f, err := os.Open(src)
	if err != nil {
		return rep, err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return rep, err
	}
	tmp := dir + ".restoring"
	_ = os.RemoveAll(tmp)
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		return rep, err
	}
	fail := func(err error) (Report, error) {
		os.RemoveAll(tmp)
		return rep, err
	}
	tr := tar.NewReader(gz)
	for {
		if ctx.Err() != nil {
			return fail(ctx.Err())
		}
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fail(err)
		}
		target, err := inside(tmp, hdr.Name)
		if err != nil {
			return fail(err)
		}
		// Through a link the archive made, a later entry would land
		// wherever that link points: the parent must resolve inside too.
		if parent, err := filepath.EvalSymlinks(filepath.Dir(target)); err == nil {
			realTmp, _ := filepath.EvalSymlinks(tmp)
			if !within(parent, realTmp) {
				return fail(fmt.Errorf("%s would be written through a link out of the folder", hdr.Name))
			}
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, fs.FileMode(hdr.Mode)&0o777|0o700); err != nil {
				return fail(err)
			}
			rep.Dirs++
		case tar.TypeSymlink:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return fail(err)
			}
			if err := os.Symlink(hdr.Linkname, target); err != nil {
				return fail(err)
			}
			rep.Links++
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return fail(err)
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, fs.FileMode(hdr.Mode)&0o777)
			if err != nil {
				return fail(err)
			}
			n, err := io.Copy(out, tr)
			out.Close()
			if err != nil {
				return fail(err)
			}
			rep.Files++
			rep.Bytes += n
		default:
			rep.skip(hdr.Name)
		}
	}
	return rep, os.Rename(tmp, dir)
}

func inside(dir, name string) (string, error) {
	clean := filepath.Clean("/" + filepath.FromSlash(name))
	if name == "" || strings.HasPrefix(name, "/") || strings.Contains("/"+filepath.ToSlash(name)+"/", "/../") {
		return "", fmt.Errorf("%q is not a path inside the archive", name)
	}
	return filepath.Join(dir, clean), nil
}

func within(p, dir string) bool {
	rel, err := filepath.Rel(dir, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, "../")
}

// heavy are folders a download leaves out: not because they weigh, but
// because an install or a build gives them back, which is the only honest
// reason not to hand them over. .git stays: a project's history is part of
// its code. The pointer deploys build into .next-<date>, which a list of
// exact names would miss (V1 measured four of them in one site).
var heavy = map[string]bool{
	"node_modules": true, ".next": true, ".nuxt": true, ".svelte-kit": true, ".output": true,
	".turbo": true, ".parcel-cache": true, ".cache": true, "dist": true, "build": true, "out": true,
	"coverage": true, "target": true, "vendor": true, "venv": true, ".venv": true, "__pycache__": true,
	".pytest_cache": true, ".gradle": true, ".mypy_cache": true, "bower_components": true,
}

// Rebuildable says whether a folder name is one a download leaves out.
func Rebuildable(name string) bool {
	if heavy[name] {
		return true
	}
	for _, p := range []string{".next-", ".nuxt-", "dist-", "build-"} {
		if strings.HasPrefix(name, p) && len(name) > len(p) {
			return true
		}
	}
	return false
}

// Zip writes dir as a zip to w, leaving out rebuildable folders unless all
// is set. Links are not followed: they are skipped and reported.
func Zip(ctx context.Context, w io.Writer, dir string, all bool) (Report, error) {
	var rep Report
	zw := zip.NewWriter(w)
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		if rel == "." {
			return nil
		}
		if d.IsDir() {
			if !all && Rebuildable(d.Name()) {
				rep.skip(filepath.ToSlash(rel) + "/ (rebuildable)")
				return filepath.SkipDir
			}
			rep.Dirs++
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			rep.skip(filepath.ToSlash(rel) + " (" + info.Mode().Type().String() + ")")
			return nil
		}
		hdr, err := zip.FileInfoHeader(info)
		if err != nil {
			return err
		}
		hdr.Name, hdr.Method = filepath.ToSlash(rel), zip.Deflate
		out, err := zw.CreateHeader(hdr)
		if err != nil {
			return err
		}
		src, err := os.Open(p)
		if err != nil {
			rep.skip(filepath.ToSlash(rel) + " (unreadable)")
			return nil
		}
		n, err := io.Copy(out, src)
		src.Close()
		rep.Files++
		rep.Bytes += n
		return err
	})
	if err != nil {
		return rep, err
	}
	return rep, zw.Close()
}
