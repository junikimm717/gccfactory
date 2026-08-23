package sources

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// Extract untars archive into dst so that the contents of the archive's single
// top-level directory land directly in dst (i.e. --strip-components=1).
//
// dst must not already exist; it is created. On any failure dst is removed, so
// a half-extracted tree is never left behind for a later run to mistake for a
// good one.
//
// We shell out to GNU tar rather than using archive/tar: this is a Linux-only
// build system, GNU tar auto-detects gz/xz/bz2, and it is several times faster
// than a pure-Go xz decoder on a 90 MB gcc tarball.
func Extract(ctx context.Context, archive, dst string, s Source) error {
	if s.Raw {
		return fmt.Errorf("sources: %s is a raw file, not an archive; use the path from Fetch directly", s.Name)
	}
	if _, err := os.Stat(dst); err == nil {
		return fmt.Errorf("sources: extract destination %s already exists", dst)
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	if err := os.Mkdir(dst, 0o755); err != nil {
		return fmt.Errorf("sources: creating %s: %w", dst, err)
	}
	ok := false
	defer func() {
		if !ok {
			os.RemoveAll(dst)
		}
	}()

	args := []string{"-x", "-f", archive, "-C", dst,
		"--strip-components=" + strconv.Itoa(s.StripComponents()),
		"--no-same-owner"}
	cmd := exec.CommandContext(ctx, "tar", args...)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("sources: tar %s: %w\n%s", strings.Join(args, " "), err, tail(out.String()))
	}

	ents, err := os.ReadDir(dst)
	if err != nil {
		return err
	}
	if len(ents) == 0 {
		return fmt.Errorf("sources: extracting %s into %s produced an empty tree", archive, dst)
	}
	ok = true
	return nil
}

func tail(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > 30 {
		lines = lines[len(lines)-30:]
	}
	return strings.Join(lines, "\n")
}
