package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/junikimm717/gccfactory/src/gccfactory/internal/core"
	"github.com/junikimm717/gccfactory/src/gccfactory/internal/ensure"
	"github.com/junikimm717/gccfactory/src/gccfactory/internal/pack"
	"github.com/junikimm717/gccfactory/src/gccfactory/internal/recipe"
	"github.com/junikimm717/gccfactory/src/gccfactory/internal/triple"
)

var cmdPack = &command{
	Name:     "pack",
	Short:    "package built toolchains into distributable .tgz tarballs",
	Synopsis: "gccfactory pack [--host LIST] [--target LIST]",
	Long: `Turns each built canadian toolchain in dist/toolchains/out/<HOST>/<TARGET>/
into one gzip tarball, ready to publish or to hand to a consumer.

WHERE IT LANDS
  dist/tarballs/<HOST-ARCH>/<TARGET>-<cross|native>.tgz
  dist/tarballs/<HOST-ARCH>/SHA256SUMS

  One directory per HOST architecture, because a tarball is only usable on the
  machine its binaries run on, and consumers fetch by architecture. SHA256SUMS
  is rewritten to cover every tarball in the directory and is in the format
  ` + "`sha256sum -c`" + ` reads.

NAMING, AND WHY IT MATTERS
  The basename is <TARGET>-native when TARGET's architecture equals HOST's, and
  <TARGET>-cross otherwise. Only the architecture -- the first field of the
  triple -- is compared, so on an arm host both arm-linux-musleabi and
  arm-linux-musleabihf are native, while i386 on an x86_64 host is a cross.

  The tarball contains exactly ONE top-level directory, with that same name.
  A consumer runs

      tar -xzf <TARGET>-cross.tgz -C deps-<TARGET>

  and then expects deps-<TARGET>/<TARGET>-cross/bin/<TARGET>-gcc to exist, so
  the suffix is load-bearing: it is how the consumer decides whether it may run
  the compiler's own output, and the directory name is what its paths are built
  from. Get either wrong and nothing downstream resolves.

WHY THE CHECKSUMS ARE STABLE
  A tarball is a pure function of the tree it was made from. Entries are
  written in sorted order; uid, gid, uname and gname are zeroed; every mtime is
  set to the unix epoch; permissions are normalised to 0755/0644 (the
  executable bit is preserved, the builder's umask is not); and the gzip header
  carries no timestamp. Packing the same tree twice therefore produces
  byte-identical bytes, so a changed sha256 always means changed content.

  Symlinks are stored as symlinks and hardlinked files as hardlinks -- the
  tooldir layout depends on both, and dereferencing them would inflate the
  tarball by tens of megabytes.

WHAT IS CHECKED
  Each tarball is reopened after writing and verified, rather than assumed:
  exactly one top-level directory with the expected name, bin/<TARGET>-gcc
  present and executable, and bin/make present (make ships with the
  deliverable). Failures are reported per toolchain and set the exit status.

  Writing is atomic: the tarball is built in a temp file beside its final path
  and renamed into place, so an interrupted run never leaves a truncated .tgz
  that looks complete.

FLAGS
  --host LIST     restrict to these host triples
  --target LIST   restrict to these target triples

With neither flag, every canadian toolchain currently present in dist/ is
packed, so ` + "`gccfactory pack`" + ` on its own is always a valid thing to type.
A (host, target) pair that is not built is skipped, not an error; so is one
that another gccfactory is republishing right now.`,
	Run: runPack,
}

func runPack(g *Global, args []string) error {
	fs := g.flagSet("pack")
	host := fs.String("host", "", tripleFlagHelp)
	target := fs.String("target", "", tripleFlagHelp)
	if err := parse(fs, args); err != nil {
		return finish("pack", err)
	}
	if err := g.resolve(); err != nil {
		return err
	}

	hosts, targets := allTriples(), allTriples()
	// Skips are only worth printing when the user named a cell; over the whole
	// matrix they would bury the toolchains that did get packed.
	explicit := *host != "" || *target != ""
	var err error
	if *host != "" {
		if hosts, err = parseTriples("host", *host); err != nil {
			return err
		}
	}
	if *target != "" {
		if targets, err = parseTriples("target", *target); err != nil {
			return err
		}
	}

	e, done, err := g.env(defaultJobs, defaultWorkers)
	if err != nil {
		return err
	}
	defer done()

	ctx, stop := signalContext()
	defer stop()

	p := &packer{e: e, ctx: ctx, loud: explicit, sums: map[string]map[string]string{}}
	for _, h := range hosts {
		for _, t := range targets {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			p.one(h, t)
		}
	}
	return p.finish()
}

type packer struct {
	e       *core.Env
	ctx     context.Context
	loud    bool
	ran     int
	failed  int
	skipped int
	// sums[dir][basename] = sha256, so SHA256SUMS reuses what we just hashed
	// instead of reading every tarball back a second time.
	sums map[string]map[string]string
}

func (p *packer) skip(h, t triple.Triple, why string) {
	p.skipped++
	if p.loud {
		fmt.Fprintf(os.Stderr, "%s host=%s target=%s: %s\n", dim("skip:"), h.Raw, t.Raw, why)
	}
}

func (p *packer) one(h, t triple.Triple) {
	j := recipe.Canadian(h, t)
	prefix := j.ArtifactDir(p.e)
	if _, ok := artifactManifest(prefix); !ok {
		p.skip(h, t, "not built; run `gccf build --host "+h.Raw+" --target "+t.Raw+"` first")
		return
	}
	release, ok, err := core.TryReadLease(p.e, j.Slug())
	if err != nil {
		p.report(failedReport(h, t, err))
		return
	}
	if !ok {
		p.skip(h, t, "another gccfactory holds it; it is being rebuilt right now")
		return
	}
	defer release()
	// The lease only became ours after the check above, so re-check: a publish
	// may have completed in between.
	if _, ok := artifactManifest(prefix); !ok {
		p.skip(h, t, "not built")
		return
	}

	top := pack.TopDir(h, t)
	dir := filepath.Join(p.e.Dist, "tarballs", h.Arch)
	dst := filepath.Join(dir, pack.FileName(h, t))
	fmt.Printf("%s host=%s target=%s -> %s\n", bold("pack:"), h.Raw, t.Raw, cyan(rel(p.e.Dist, dst)))

	start := time.Now()
	r := ensure.NewReport(fmt.Sprintf("pack host=%s target=%s", h.Raw, t.Raw))
	res, err := pack.Write(dst, prefix, top)
	if err != nil {
		r.Fail("tarball", err, "%s", rel(p.e.Dist, dst))
		r.Dur = time.Since(start)
		p.report(r)
		return
	}
	r.Pass("tarball", "%s, %s (%d files, %d symlinks, %d hardlinks)",
		rel(p.e.Dist, dst), humanBytes(res.Size), res.Files, res.Symlinks, res.Hardlinks)
	r.Pass("sha256", "%s", res.SHA256)
	inspect(r, dst, top, t)
	r.Dur = time.Since(start)
	p.report(r)

	if r.OK() {
		if p.sums[dir] == nil {
			p.sums[dir] = map[string]string{}
		}
		p.sums[dir][filepath.Base(dst)] = res.SHA256
	}
}

// A tarball nobody opened is a claim, not a result.
func inspect(r *ensure.Report, dst, top string, t triple.Triple) {
	a, err := pack.Inspect(dst)
	if err != nil {
		r.Fail("readback", err, "%s", dst)
		return
	}
	if len(a.Tops) == 1 && a.Tops[0] == top {
		r.Pass("top-level-dir", "%s/ (%d entries)", top, len(a.Entries))
	} else {
		r.Failf("top-level-dir", "want exactly one top-level dir %q, got %v", top, a.Tops)
	}
	gcc := top + "/bin/" + t.Raw + "-gcc"
	switch e, ok := a.Has(gcc); {
	case !ok:
		r.Failf("bin/"+t.Raw+"-gcc", "missing from the tarball")
	case !e.Executable():
		r.Failf("bin/"+t.Raw+"-gcc", "present but mode %04o is not executable", e.Mode)
	default:
		r.Pass("bin/"+t.Raw+"-gcc", "mode %04o", e.Mode)
	}
	if e, ok := a.Has(top + "/bin/make"); ok {
		r.Pass("bin/make", "mode %04o", e.Mode)
	} else {
		r.Failf("bin/make", "missing; the deliverable ships make")
	}
}

func failedReport(h, t triple.Triple, err error) *ensure.Report {
	r := ensure.NewReport(fmt.Sprintf("pack host=%s target=%s", h.Raw, t.Raw))
	r.Fail("lock", err, "cannot take a read lease on the artifact")
	return r
}

func (p *packer) report(r *ensure.Report) {
	p.ran++
	fmt.Println(r.String())
	if !r.OK() {
		p.failed++
	}
}

func (p *packer) finish() error {
	if p.ran == 0 {
		if p.skipped > 0 {
			fmt.Fprintf(os.Stderr, "nothing packed: %d requested pair%s skipped, see above.\n",
				p.skipped, plural(p.skipped))
		} else {
			fmt.Fprintf(os.Stderr, "nothing to pack: no canadian toolchains built yet. Run %s first.\n",
				cyan("gccf build --host proven --target proven"))
		}
		return nil
	}
	for _, dir := range sortedKeys(p.sums) {
		n, err := writeSums(dir, p.sums[dir])
		if err != nil {
			return fmt.Errorf("write %s: %w", filepath.Join(dir, "SHA256SUMS"), err)
		}
		fmt.Printf("%s %s (%d tarball%s)\n", green("sums:"),
			rel(p.e.Dist, filepath.Join(dir, "SHA256SUMS")), n, plural(n))
	}
	if p.failed > 0 {
		return fmt.Errorf("%d of %d tarball%s failed verification", p.failed, p.ran, plural(p.ran))
	}
	fmt.Printf("\n%s %d tarball%s in %s\n", green("PASS"), p.ran, plural(p.ran),
		rel(p.e.Dist, filepath.Join(p.e.Dist, "tarballs")))
	return nil
}

// writeSums covers every .tgz in dir, not just this run's, so the file stays a
// complete index after a partial pack.
func writeSums(dir string, known map[string]string) (int, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return 0, err
	}
	var names []string
	for _, ent := range ents {
		if !ent.IsDir() && strings.HasSuffix(ent.Name(), ".tgz") {
			names = append(names, ent.Name())
		}
	}
	sort.Strings(names)
	var b strings.Builder
	for _, name := range names {
		sum, ok := known[name]
		if !ok {
			if sum, err = fileSHA256(filepath.Join(dir, name)); err != nil {
				return 0, err
			}
		}
		fmt.Fprintf(&b, "%s  %s\n", sum, name)
	}
	tmp, err := os.CreateTemp(dir, ".sums-*")
	if err != nil {
		return 0, err
	}
	defer os.Remove(tmp.Name())
	if _, err := io.WriteString(tmp, b.String()); err != nil {
		tmp.Close()
		return 0, err
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return 0, err
	}
	if err := tmp.Close(); err != nil {
		return 0, err
	}
	return len(names), os.Rename(tmp.Name(), filepath.Join(dir, "SHA256SUMS"))
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
