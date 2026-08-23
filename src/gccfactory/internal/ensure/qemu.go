package ensure

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/junikimm717/gccfactory/src/gccfactory/internal/triple"
)

// QemuNames lists the qemu-user binary names that can execute t, most
// preferred first.
func QemuNames(t triple.Triple) []string {
	n := t.QemuName()
	return []string{"qemu-" + n, "qemu-" + n + "-static"}
}

// QemuFor locates a qemu-user binary able to run t. searchPaths entries may be
// directories to scan or a direct path to a qemu binary; PATH is searched
// last. The error names everything that was tried.
func QemuFor(t triple.Triple, searchPaths []string) (string, error) {
	names := QemuNames(t)
	for _, p := range searchPaths {
		if p == "" {
			continue
		}
		st, err := os.Stat(p)
		if err != nil {
			continue
		}
		if !st.IsDir() {
			if isExec(st) && contains(names, filepath.Base(p)) {
				return abs(p), nil
			}
			continue
		}
		for _, n := range names {
			c := filepath.Join(p, n)
			if st, err := os.Stat(c); err == nil && !st.IsDir() && isExec(st) {
				return abs(c), nil
			}
		}
	}
	for _, n := range names {
		if p, err := exec.LookPath(n); err == nil {
			return abs(p), nil
		}
	}
	where := "$PATH"
	if len(searchPaths) > 0 {
		where = strings.Join(searchPaths, ", ") + ", " + where
	}
	return "", fmt.Errorf("no qemu-user binary for %s: looked for %s in %s",
		t, strings.Join(names, " or "), where)
}

func isExec(st os.FileInfo) bool { return st.Mode()&0o111 != 0 }

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

func abs(p string) string {
	if a, err := filepath.Abs(p); err == nil {
		return a
	}
	return p
}
