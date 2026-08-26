// Package pack writes the tarball layout consumers expect: a gzip tar holding
// exactly one top-level directory named <TARGET>-cross or <TARGET>-native.
// Output is a pure function of the tree, so a changed sha256 always means
// changed content.
package pack

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/junikimm717/gccfactory/src/gccfactory/internal/ensure"
	"github.com/junikimm717/gccfactory/src/gccfactory/internal/triple"
)

var epoch = time.Unix(0, 0).UTC()

// Only the architecture is compared: arm-linux-musleabi and
// arm-linux-musleabihf are both native on an arm host, i386 on x86_64 is not.
func Kind(host, target triple.Triple) string {
	if host.Arch == target.Arch {
		return "native"
	}
	return "cross"
}

// Also the tarball's own basename: consumers extract into their deps directory
// and then open <TopDir>/bin/<TARGET>-gcc, so the two must agree.
func TopDir(host, target triple.Triple) string {
	return target.Raw + "-" + Kind(host, target)
}

func FileName(host, target triple.Triple) string { return TopDir(host, target) + ".tgz" }

type Result struct {
	Path      string
	SHA256    string
	Size      int64
	Files     int
	Dirs      int
	Symlinks  int
	Hardlinks int
}

// Writes to a temp file and renames, so an interrupted run never leaves a
// half-written tarball that looks valid.
func Write(dst, src, top string, t triple.Triple) (Result, error) {
	res := Result{Path: dst}
	names, err := entries(src)
	if err != nil {
		return res, err
	}
	aliases := aliasLinks(src, t)
	for rel := range aliases {
		names = append(names, rel)
	}
	sort.Strings(names)
	dir := filepath.Dir(dst)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return res, err
	}
	tmp, err := os.CreateTemp(dir, ".pack-*.tgz")
	if err != nil {
		return res, err
	}
	committed := false
	defer func() {
		if !committed {
			tmp.Close()
			os.Remove(tmp.Name())
		}
	}()

	sum := sha256.New()
	bw := bufio.NewWriterSize(io.MultiWriter(tmp, sum), 1<<20)
	zw, err := gzip.NewWriterLevel(bw, gzip.DefaultCompression)
	if err != nil {
		return res, err
	}
	zw.ModTime = time.Time{}
	tw := tar.NewWriter(zw)

	if err := tw.WriteHeader(&tar.Header{
		Name:     top + "/",
		Typeflag: tar.TypeDir,
		Mode:     0o755,
		ModTime:  epoch,
	}); err != nil {
		return res, err
	}
	res.Dirs++

	hard := map[[2]uint64]string{}
	for _, rel := range names {
		if link, ok := aliases[rel]; ok {
			if err := tw.WriteHeader(&tar.Header{
				Name:     path.Join(top, rel),
				Typeflag: tar.TypeSymlink,
				Linkname: link,
				Mode:     0o777,
				ModTime:  epoch,
			}); err != nil {
				return res, err
			}
			res.Symlinks++
			continue
		}
		full := filepath.Join(src, rel)
		fi, err := os.Lstat(full)
		if err != nil {
			return res, err
		}
		h, err := header(top, rel, full, fi)
		if err != nil {
			return res, fmt.Errorf("%s: %w", full, err)
		}
		if fi.Mode().IsRegular() {
			if st, ok := fi.Sys().(*syscall.Stat_t); ok && uint64(st.Nlink) > 1 {
				key := [2]uint64{uint64(st.Dev), uint64(st.Ino)}
				if first, seen := hard[key]; seen {
					h.Typeflag, h.Linkname, h.Size = tar.TypeLink, first, 0
				} else {
					hard[key] = h.Name
				}
			}
		}
		if err := tw.WriteHeader(h); err != nil {
			return res, err
		}
		switch h.Typeflag {
		case tar.TypeDir:
			res.Dirs++
		case tar.TypeSymlink:
			res.Symlinks++
		case tar.TypeLink:
			res.Hardlinks++
		case tar.TypeReg:
			res.Files++
			if err := copyFile(tw, full, h.Size); err != nil {
				return res, err
			}
		}
	}

	if err := tw.Close(); err != nil {
		return res, err
	}
	if err := zw.Close(); err != nil {
		return res, err
	}
	if err := bw.Flush(); err != nil {
		return res, err
	}
	if err := tmp.Sync(); err != nil {
		return res, err
	}
	fi, err := tmp.Stat()
	if err != nil {
		return res, err
	}
	if err := tmp.Chmod(0o644); err != nil {
		return res, err
	}
	if err := tmp.Close(); err != nil {
		return res, err
	}
	if err := os.Rename(tmp.Name(), dst); err != nil {
		return res, err
	}
	committed = true
	res.Size = fi.Size()
	res.SHA256 = hex.EncodeToString(sum.Sum(nil))
	return res, nil
}

// A short read means the tree changed under us; a tar with a truncated body is
// worse than no tar at all.
func copyFile(w io.Writer, full string, size int64) error {
	f, err := os.Open(full)
	if err != nil {
		return err
	}
	defer f.Close()
	n, err := io.Copy(w, f)
	if err != nil {
		return err
	}
	if n != size {
		return fmt.Errorf("%s: read %d bytes, header says %d", full, n, size)
	}
	return nil
}

func header(top, rel, full string, fi fs.FileInfo) (*tar.Header, error) {
	link := ""
	if fi.Mode()&fs.ModeSymlink != 0 {
		var err error
		if link, err = os.Readlink(full); err != nil {
			return nil, err
		}
	}
	h, err := tar.FileInfoHeader(fi, link)
	if err != nil {
		return nil, err
	}
	h.Name = path.Join(top, filepath.ToSlash(rel))
	if fi.IsDir() {
		h.Name += "/"
	}
	h.Uid, h.Gid, h.Uname, h.Gname = 0, 0, "", ""
	h.ModTime, h.AccessTime, h.ChangeTime = epoch, time.Time{}, time.Time{}
	h.Mode = int64(normalMode(fi))
	// FileInfoHeader may pin a format; letting the writer choose keeps entries
	// in USTAR unless a long path forces PAX.
	h.Format = tar.FormatUnknown
	return h, nil
}

// Permissions are normalised so the builder's umask cannot change the bytes.
// The executable bit is what actually matters and is preserved.
func normalMode(fi fs.FileInfo) fs.FileMode {
	switch {
	case fi.IsDir():
		return 0o755
	case fi.Mode()&fs.ModeSymlink != 0:
		return 0o777
	case fi.Mode().Perm()&0o111 != 0:
		return 0o755
	default:
		return 0o644
	}
}

// AliasTools are the unprefixed names bin/ also answers to. Consumers that
// drive the toolchain through PATH -- mimux's cross/build, musl-cross-make's
// own README -- otherwise have to mint these by hand after unpacking.
func AliasTools() map[string]string {
	m := map[string]string{"cc": "gcc"}
	for _, n := range ensure.BinutilsTools {
		m[n] = n
	}
	return m
}

// An alias is dropped when bin/ already ships that name, so the tarball can
// never shadow a real binary with a link.
func aliasLinks(src string, t triple.Triple) map[string]string {
	out := map[string]string{}
	for name, tool := range AliasTools() {
		link := t.Raw + "-" + tool
		if _, err := os.Lstat(filepath.Join(src, "bin", name)); err == nil {
			continue
		}
		if _, err := os.Lstat(filepath.Join(src, "bin", link)); err != nil {
			continue
		}
		out[filepath.Join("bin", name)] = link
	}
	return out
}

// Sorted full relative paths. A parent is a strict prefix of its children, so
// lexical order always emits a directory before anything inside it.
func entries(src string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		out = append(out, rel)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(out)
	return out, nil
}

type Entry struct {
	Type byte
	Mode int64
	Link string
	Size int64
}

func (e Entry) Executable() bool { return e.Mode&0o111 != 0 }

type Archive struct {
	Tops    []string // distinct top-level path components, sorted
	Entries map[string]Entry
}

func (a *Archive) Has(name string) (Entry, bool) {
	e, ok := a.Entries[name]
	return e, ok
}

func Inspect(file string) (*Archive, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	zr, err := gzip.NewReader(bufio.NewReaderSize(f, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", file, err)
	}
	defer zr.Close()

	a := &Archive{Entries: map[string]Entry{}}
	tops := map[string]bool{}
	tr := tar.NewReader(zr)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("%s: %w", file, err)
		}
		name := path.Clean(h.Name)
		a.Entries[name] = Entry{Type: h.Typeflag, Mode: h.Mode, Link: h.Linkname, Size: h.Size}
		tops[strings.SplitN(name, "/", 2)[0]] = true
	}
	for t := range tops {
		a.Tops = append(a.Tops, t)
	}
	sort.Strings(a.Tops)
	return a, nil
}
