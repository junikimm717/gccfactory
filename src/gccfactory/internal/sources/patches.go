package sources

import (
	"embed"
	"fmt"
	"path"
	"sort"
	"strings"
)

// patchFS holds the vendored musl-cross-make patch sets, one directory per
// "<name>-<version>". They live under this package because go:embed cannot
// reach outside the embedding package's directory.
//
//go:embed patches
var patchFS embed.FS

// Patch is one unified diff, applied with `patch -p1 -i <file>` from the
// source root.
type Patch struct {
	Name string // filename, e.g. "0003-j2.diff"
	Data []byte
}

// PatchSets lists the "<name>-<version>" directories that ship patches.
func PatchSets() []string {
	ents, err := patchFS.ReadDir("patches")
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range ents {
		if e.IsDir() {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}

// Patches sorts by filename, which is the order they must be applied in. A
// source with no patch directory returns nil.
func Patches(s Source) ([]Patch, error) {
	slug := s.Slug()
	found := false
	for _, d := range PatchSets() {
		if d == slug {
			found = true
			break
		}
	}
	if !found {
		return nil, nil
	}
	dir := path.Join("patches", slug)
	ents, err := patchFS.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("sources: reading embedded patch dir %s: %w", dir, err)
	}
	names := make([]string, 0, len(ents))
	for _, e := range ents {
		if e.IsDir() || !isDiff(e.Name()) {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)

	out := make([]Patch, 0, len(names))
	for _, n := range names {
		b, err := patchFS.ReadFile(path.Join(dir, n))
		if err != nil {
			return nil, fmt.Errorf("sources: reading embedded patch %s/%s: %w", dir, n, err)
		}
		out = append(out, Patch{Name: n, Data: b})
	}
	return out, nil
}

func isDiff(n string) bool {
	return strings.HasSuffix(n, ".diff") || strings.HasSuffix(n, ".patch")
}
