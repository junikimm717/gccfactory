// binfmt.go reads the kernel's binfmt_misc table, which is the actual
// authority on whether a foreign-arch ELF can be exec'd — not the presence of
// a qemu-<arch>-static file on disk.
package ensure

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/junikimm717/gccfactory/src/gccfactory/internal/triple"
)

// BinfmtDir is the mount point, overridable in tests.
var BinfmtDir = "/proc/sys/fs/binfmt_misc"

type BinfmtEntry struct {
	Name        string
	Enabled     bool
	Interpreter string
	Flags       string
	Offset      int
	Magic       []byte
	Mask        []byte
}

// Binfmt reads BinfmtDir. A kernel with binfmt_misc absent or unmounted
// returns (nil, nil): "no foreign exec" is a fact to report, not an error.
func Binfmt() ([]BinfmtEntry, error) {
	des, err := os.ReadDir(BinfmtDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", BinfmtDir, err)
	}
	var out []BinfmtEntry
	for _, de := range des {
		if de.IsDir() || de.Name() == "register" || de.Name() == "status" {
			continue
		}
		b, err := os.ReadFile(filepath.Join(BinfmtDir, de.Name()))
		if err != nil {
			continue
		}
		if e, ok := parseBinfmtEntry(de.Name(), b); ok {
			out = append(out, e)
		}
	}
	return out, nil
}

func parseBinfmtEntry(name string, b []byte) (BinfmtEntry, bool) {
	lines := strings.Split(string(b), "\n")
	if len(lines) == 0 {
		return BinfmtEntry{}, false
	}
	e := BinfmtEntry{Name: name}
	switch strings.TrimSpace(lines[0]) {
	case "enabled":
		e.Enabled = true
	case "disabled":
		e.Enabled = false
	default:
		return BinfmtEntry{}, false
	}

	var magicHex, maskHex string
	haveMagic := false
	for _, line := range lines[1:] {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		key, val, _ := strings.Cut(line, " ")
		key = strings.TrimSuffix(key, ":")
		val = strings.TrimSpace(val)
		switch key {
		case "interpreter":
			e.Interpreter = val
		case "flags":
			e.Flags = val
		case "offset":
			n, err := strconv.Atoi(val)
			if err != nil {
				return BinfmtEntry{}, false
			}
			e.Offset = n
		case "magic":
			magicHex, haveMagic = val, true
		case "mask":
			maskHex = val
		}
	}
	// extension-registered entries have no magic and can never match an ELF.
	if !haveMagic {
		return BinfmtEntry{}, false
	}
	magic, err := hex.DecodeString(magicHex)
	if err != nil {
		return BinfmtEntry{}, false
	}
	e.Magic = magic

	if maskHex == "" {
		mask := make([]byte, len(magic))
		for i := range mask {
			mask[i] = 0xff
		}
		e.Mask = mask
	} else {
		mask, err := hex.DecodeString(maskHex)
		if err != nil {
			return BinfmtEntry{}, false
		}
		e.Mask = mask
	}
	return e, true
}

// Matches applies the kernel's own rule:
// (hdr[Offset+i] & Mask[i]) == (Magic[i] & Mask[i]) for every i.
func (e BinfmtEntry) Matches(hdr []byte) bool {
	if len(e.Magic) == 0 || len(e.Mask) < len(e.Magic) {
		return false
	}
	if e.Offset < 0 || e.Offset+len(e.Magic) > len(hdr) {
		return false
	}
	for i := range e.Magic {
		if hdr[e.Offset+i]&e.Mask[i] != e.Magic[i]&e.Mask[i] {
			return false
		}
	}
	return true
}

// ELFHeaders returns the candidate ELF header prefixes for t. They differ only
// in e_type: a registration may be written to match ET_EXEC, ET_DYN or both,
// so a caller tries all of them.
func ELFHeaders(t triple.Triple) [][]byte {
	machine, class, data, ok := t.ELF()
	if !ok {
		return nil
	}
	// e_type/e_machine are read by the kernel in the target's own byte order.
	var bo binary.ByteOrder = binary.LittleEndian
	if data == elfDataBE {
		bo = binary.BigEndian
	}
	build := func(etype uint16) []byte {
		h := make([]byte, 64)
		h[0], h[1], h[2], h[3] = 0x7f, 'E', 'L', 'F'
		h[4], h[5], h[6] = class, data, 1
		bo.PutUint16(h[16:18], etype)
		bo.PutUint16(h[18:20], machine)
		return h
	}
	const etExec, etDyn = 2, 3
	return [][]byte{build(etExec), build(etDyn)}
}

// BinfmtFor returns the enabled entry that would execute a binary for t.
func BinfmtFor(t triple.Triple, entries []BinfmtEntry) (BinfmtEntry, bool) {
	hdrs := ELFHeaders(t)
	if hdrs == nil {
		return BinfmtEntry{}, false
	}
	for _, e := range entries {
		if !e.Enabled {
			continue
		}
		for _, h := range hdrs {
			if e.Matches(h) {
				return e, true
			}
		}
	}
	return BinfmtEntry{}, false
}
