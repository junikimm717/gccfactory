package cli

import (
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/junikimm717/gccfactory/src/gccfactory/internal/core"
)

// Stale files (dead pid, not refreshed) are dropped, so callers can treat
// presence as "somebody is building this right now".
func readHeartbeats(e *core.Env) map[string]*core.Heartbeat {
	out := map[string]*core.Heartbeat{}
	ents, err := os.ReadDir(filepath.Join(e.Dist, core.DirState, "heartbeats"))
	if err != nil {
		return out
	}
	for _, ent := range ents {
		if ent.IsDir() || !strings.HasSuffix(ent.Name(), ".json") {
			continue
		}
		slug := strings.TrimSuffix(ent.Name(), ".json")
		h, err := core.ReadHeartbeat(e, slug)
		if err != nil || !h.Live() {
			continue
		}
		if h.Slug == "" {
			h.Slug = slug
		}
		out[h.Slug] = h
	}
	return out
}

func who(h *core.Heartbeat) string {
	if h == nil {
		return ""
	}
	host := h.Host
	if i := strings.IndexByte(host, '.'); i > 0 {
		host = host[:i]
	}
	return strconv.Itoa(h.PID) + "@" + host
}

// A present manifest means "something was published here"; whether it is
// still current is core.Plan's answer, not this one's.
func artifactManifest(dir string) (*core.Manifest, bool) {
	m, err := core.ReadManifest(dir)
	if err != nil {
		return nil, false
	}
	return m, true
}

func dirSize(root string) int64 {
	var total int64
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if info, err := d.Info(); err == nil && info.Mode().IsRegular() {
			total += info.Size()
		}
		return nil
	})
	return total
}

// Used only for reaping abandoned scratch dirs, never for correctness.
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return p.Signal(syscall.Signal(0)) == nil
}
