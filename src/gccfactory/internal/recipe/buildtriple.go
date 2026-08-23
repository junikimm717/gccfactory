package recipe

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

// The BUILD triple must come from the gcc source tree's own config.guess.
//
// `gcc -dumpmachine` is NOT equivalent: Debian's gcc reports the multiarch form
// `aarch64-linux-gnu` while config.guess reports `aarch64-unknown-linux-gnu`.
// gcc's toplevel compares --build against --host textually to decide whether it
// is doing a cross or a canadian build, so feeding it the wrong spelling makes
// it build the wrong thing.
//
// It is the same string for every job in a run, and running it costs a process,
// so the first job to need it resolves it and the rest read the cache.

var buildTriple struct {
	sync.Mutex
	val string
}

// gcc's copy is the canonical one: other packages ship older config.guess
// revisions that can disagree.
func (b *builder) resolveBuildTriple(ctx context.Context) (string, error) {
	buildTriple.Lock()
	defer buildTriple.Unlock()
	if buildTriple.val != "" {
		return buildTriple.val, nil
	}
	guess := filepath.Join(srcTreeJob(pkgGCC).ArtifactDir(b.e), srcTreeSubdir, "config.guess")
	out := filepath.Join(b.cfg.Work, "build-triple.txt")
	b.step("config-guess")
	if err := b.sh(ctx, "config-guess", b.cfg.Work, nil, shQuote(guess)+" > "+shQuote(out)); err != nil {
		return "", err
	}
	raw, err := os.ReadFile(out)
	if err != nil {
		return "", fmt.Errorf("recipe: read config.guess output: %w", err)
	}
	g := strings.TrimSpace(string(raw))
	if g == "" || strings.ContainsAny(g, " \t\n") {
		return "", fmt.Errorf("recipe: config.guess printed %q, which is not a triple", g)
	}
	buildTriple.val = g
	b.e.Log.Info("resolved build triple", "build", g)
	return g, nil
}

// buildPlatform is the key's stand-in for the BUILD triple. See keyCfg.
func buildPlatform() string { return runtime.GOOS + "/" + runtime.GOARCH }

func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
