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

func hasPatchSet(s Source) bool {
	slug := s.Slug()
	for _, d := range PatchSets() {
		if d == slug {
			return true
		}
	}
	return false
}

// Patches sorts by filename, which is the order they must be applied in. A
// source with no patch directory returns nil. Architecture subdirectories are
// skipped; ArchPatches reads those.
func Patches(s Source) ([]Patch, error) {
	if !hasPatchSet(s) {
		return nil, nil
	}
	return readPatchDir(path.Join("patches", s.Slug()))
}

// PatchArches lists the architecture subdirectories of a source's patch set.
// A patch under <slug>/<arch>/ applies only to builds for that architecture,
// so editing it rebuilds that architecture instead of the whole matrix.
func PatchArches(s Source) []string {
	if !hasPatchSet(s) {
		return nil
	}
	ents, err := patchFS.ReadDir(path.Join("patches", s.Slug()))
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

// ArchPatches returns the patches scoped to one architecture, sorted by
// filename. An architecture with no subdirectory returns nil, which is the
// common case.
func ArchPatches(s Source, arch string) ([]Patch, error) {
	if arch == "" || !hasPatchSet(s) {
		return nil, nil
	}
	dir := path.Join("patches", s.Slug(), arch)
	if _, err := patchFS.ReadDir(dir); err != nil {
		return nil, nil
	}
	return readPatchDir(dir)
}

func readPatchDir(dir string) ([]Patch, error) {
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
