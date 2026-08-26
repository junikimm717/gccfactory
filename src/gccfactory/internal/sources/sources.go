// Package sources is the pinned, checksum-verified source database for
// gccfactory. Every tarball the build farm consumes is described by exactly one
// Source entry in sources.json, which is embedded into the binary at compile
// time so a built gccfactory needs no external metadata.
//
// This package deliberately does not import internal/core: it takes the dist
// directory as a plain string so that core (which builds jobs out of sources)
// can depend on it without a cycle.
package sources

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"sync"
)

//go:embed sources.json
var raw []byte

type Source struct {
	Name    string   `json:"name"`    // short key, e.g. "gcc"
	Version string   `json:"version"` // e.g. "16.2.0"
	File    string   `json:"file"`    // local filename, e.g. "gcc-16.2.0.tar.xz"
	URLs    []string `json:"urls"`    // mirrors, tried in order
	SHA256  string   `json:"sha256"`  // lowercase hex, 64 chars
	TopDir  string   `json:"topdir"`  // single top-level dir inside the archive; "" for raw files
	// Strip is the number of leading path components dropped on extract.
	// Zero means 1 (every archive we ship has a single top-level directory).
	Strip int  `json:"strip,omitempty"`
	Raw   bool `json:"raw,omitempty"` // true for plain files such as config.sub
}

func (s Source) StripComponents() int {
	if s.Strip == 0 {
		return 1
	}
	return s.Strip
}

// Slug is used for patch directories and log names. Sources without a dotted
// version (config.sub) use their raw version.
func (s Source) Slug() string { return s.Name + "-" + s.Version }

var (
	loadOnce sync.Once
	loaded   []Source
	byName   map[string]Source
	loadErr  error
)

var sha256Re = regexp.MustCompile(`^[0-9a-f]{64}$`)

func load() {
	loadOnce.Do(func() {
		var list []Source
		if err := json.Unmarshal(raw, &list); err != nil {
			loadErr = fmt.Errorf("sources: parsing embedded sources.json: %w", err)
			return
		}
		byName = make(map[string]Source, len(list))
		for _, s := range list {
			if _, dup := byName[s.Name]; dup {
				loadErr = fmt.Errorf("sources: duplicate entry %q in sources.json", s.Name)
				return
			}
			byName[s.Name] = s
		}
		sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
		loaded = list
	})
}

// All returns every pinned source, sorted by name.
func All() []Source {
	load()
	if loadErr != nil {
		panic(loadErr)
	}
	out := make([]Source, len(loaded))
	copy(out, loaded)
	return out
}

func Get(name string) (Source, error) {
	load()
	if loadErr != nil {
		return Source{}, loadErr
	}
	s, ok := byName[name]
	if !ok {
		known := make([]string, 0, len(byName))
		for k := range byName {
			known = append(known, k)
		}
		sort.Strings(known)
		return Source{}, fmt.Errorf("sources: unknown source %q (known: %v)", name, known)
	}
	return s, nil
}

// MustGet is for names that are compiled into recipes: an unpinned name is
// always a programming error.
func MustGet(name string) Source {
	s, err := Get(name)
	if err != nil {
		panic(err)
	}
	return s
}

// Validate is called by the tests and by the updater's -check mode.
func Validate(list []Source) error {
	seen := map[string]bool{}
	for _, s := range list {
		switch {
		case s.Name == "":
			return fmt.Errorf("sources: entry with empty name")
		case seen[s.Name]:
			return fmt.Errorf("sources: duplicate entry %q", s.Name)
		case s.Version == "":
			return fmt.Errorf("sources: %s: empty version", s.Name)
		case s.File == "":
			return fmt.Errorf("sources: %s: empty file", s.Name)
		case len(s.URLs) == 0:
			return fmt.Errorf("sources: %s: no urls", s.Name)
		case !sha256Re.MatchString(s.SHA256):
			return fmt.Errorf("sources: %s: sha256 %q is not 64 lowercase hex chars", s.Name, s.SHA256)
		case !s.Raw && s.TopDir == "":
			return fmt.Errorf("sources: %s: archive has no topdir recorded", s.Name)
		case s.Raw && s.TopDir != "":
			return fmt.Errorf("sources: %s: raw file must not have a topdir", s.Name)
		}
		for _, u := range s.URLs {
			if u == "" {
				return fmt.Errorf("sources: %s: empty url", s.Name)
			}
		}
		seen[s.Name] = true
	}
	return nil
}
