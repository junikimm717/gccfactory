package ensure

import (
	"debug/elf"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/junikimm717/gccfactory/src/gccfactory/internal/triple"
)

// ELF identity constants, spelled out so this file does not depend on
// debug/elf being able to parse the file at all.
const (
	elfClass32 byte = 1
	elfClass64 byte = 2
	elfDataLE  byte = 1
	elfDataBE  byte = 2

	ptInterp = 3
)

// ELFInfo is everything we care about from an ELF file header. It is filled in
// by a hand-rolled reader so that even a half-written or exotic binary yields a
// useful diagnosis rather than a parse error.
type ELFInfo struct {
	Path    string   `json:"path"`
	Class   byte     `json:"class"`   // 1 = ELF32, 2 = ELF64
	Data    byte     `json:"data"`    // 1 = little endian, 2 = big endian
	OSABI   byte     `json:"osabi"`   // EI_OSABI
	Machine uint16   `json:"machine"` // e_machine
	Type    uint16   `json:"type"`    // e_type
	Flags   uint32   `json:"flags"`   // e_flags (ABI bits on arm/mips/riscv)
	Static  bool     `json:"static"`  // no PT_INTERP segment
	Interp  string   `json:"interp"`  // PT_INTERP contents, "" if none
	Needed  []string `json:"needed"`  // DT_NEEDED, best effort
}

// String renders the identity in the compact form used in error messages,
// e.g. "ELF64/LE/EM_X86_64(62) exec dynamic(/lib/ld-musl-x86_64.so.1)".
func (i ELFInfo) String() string {
	s := ELFID(i.Class, i.Data, i.Machine) + " " + elfTypeName(i.Type)
	if i.Static {
		s += " static"
	} else {
		s += " dynamic(" + i.Interp + ")"
	}
	return s
}

func ELFID(class, data byte, machine uint16) string {
	return fmt.Sprintf("%s/%s/%s(%d)", elfClassName(class), elfDataName(data), elfMachineName(machine), machine)
}

func elfClassName(c byte) string {
	switch c {
	case elfClass32:
		return "ELF32"
	case elfClass64:
		return "ELF64"
	}
	return fmt.Sprintf("ELFCLASS?(%d)", c)
}

func elfDataName(d byte) string {
	switch d {
	case elfDataLE:
		return "LE"
	case elfDataBE:
		return "BE"
	}
	return fmt.Sprintf("ELFDATA?(%d)", d)
}

func elfMachineName(m uint16) string {
	s := elf.Machine(m).String()
	if strings.HasPrefix(s, "Machine(") { // unknown to debug/elf
		return "EM_UNKNOWN"
	}
	return s
}

func elfTypeName(t uint16) string {
	switch elf.Type(t) {
	case elf.ET_REL:
		return "rel"
	case elf.ET_EXEC:
		return "exec"
	case elf.ET_DYN:
		return "dyn"
	case elf.ET_CORE:
		return "core"
	}
	return fmt.Sprintf("type(%d)", t)
}

// ReadELF follows symlinks.
func ReadELF(path string) (ELFInfo, error) {
	info := ELFInfo{Path: path}

	st, err := os.Stat(path)
	if err != nil {
		return info, fmt.Errorf("read elf %s: %w", path, err)
	}
	if st.IsDir() {
		return info, fmt.Errorf("read elf %s: is a directory", path)
	}

	f, err := os.Open(path)
	if err != nil {
		return info, fmt.Errorf("read elf %s: %w", path, err)
	}
	defer f.Close()

	var ident [64]byte
	n, _ := io.ReadFull(f, ident[:])
	if n < 4 || string(ident[:4]) != "\x7fELF" {
		return info, fmt.Errorf("read elf %s: not an ELF file (starts with %q, %d bytes)", path, printablePrefix(ident[:n]), st.Size())
	}
	if n < 20 {
		return info, fmt.Errorf("read elf %s: truncated: only %d bytes", path, n)
	}

	info.Class, info.Data, info.OSABI = ident[4], ident[5], ident[7]
	var bo binary.ByteOrder
	switch info.Data {
	case elfDataLE:
		bo = binary.LittleEndian
	case elfDataBE:
		bo = binary.BigEndian
	default:
		return info, fmt.Errorf("read elf %s: bad EI_DATA %d (expected 1=LE or 2=BE)", path, info.Data)
	}
	if info.Class != elfClass32 && info.Class != elfClass64 {
		return info, fmt.Errorf("read elf %s: bad EI_CLASS %d (expected 1=ELF32 or 2=ELF64)", path, info.Class)
	}

	info.Type = bo.Uint16(ident[16:])
	info.Machine = bo.Uint16(ident[18:])

	var phoff int64
	var phentsize, phnum int
	if info.Class == elfClass64 {
		if n < 64 {
			return info, fmt.Errorf("read elf %s: truncated ELF64 header (%d bytes)", path, n)
		}
		info.Flags = bo.Uint32(ident[48:])
		phoff = int64(bo.Uint64(ident[32:]))
		phentsize = int(bo.Uint16(ident[54:]))
		phnum = int(bo.Uint16(ident[56:]))
	} else {
		if n < 52 {
			return info, fmt.Errorf("read elf %s: truncated ELF32 header (%d bytes)", path, n)
		}
		info.Flags = bo.Uint32(ident[36:])
		phoff = int64(bo.Uint32(ident[28:]))
		phentsize = int(bo.Uint16(ident[42:]))
		phnum = int(bo.Uint16(ident[44:]))
	}

	info.Static = true
	if phnum > 0 && phnum != 0xffff && phoff > 0 && phentsize >= 8 {
		interpOff, interpLen, err := findInterp(f, bo, info.Class, phoff, phentsize, phnum)
		if err != nil {
			return info, fmt.Errorf("read elf %s: %w", path, err)
		}
		if interpLen > 0 {
			if interpLen > 4096 {
				interpLen = 4096
			}
			buf := make([]byte, interpLen)
			if _, err := f.ReadAt(buf, interpOff); err != nil && err != io.EOF {
				return info, fmt.Errorf("read elf %s: PT_INTERP at %#x: %w", path, interpOff, err)
			}
			info.Interp = string(trimNUL(buf))
			info.Static = false
		}
	}

	// Best effort: DT_NEEDED via the stdlib parser. Never fatal.
	if fe, err := elf.Open(path); err == nil {
		if libs, err := fe.ImportedLibraries(); err == nil {
			info.Needed = libs
		}
		fe.Close()
	}
	return info, nil
}

func findInterp(f *os.File, bo binary.ByteOrder, class byte, phoff int64, phentsize, phnum int) (off int64, size int64, err error) {
	buf := make([]byte, phentsize)
	for i := 0; i < phnum; i++ {
		at := phoff + int64(i)*int64(phentsize)
		if _, err := f.ReadAt(buf, at); err != nil && err != io.EOF {
			return 0, 0, fmt.Errorf("program header %d at %#x: %w", i, at, err)
		}
		if bo.Uint32(buf) != ptInterp {
			continue
		}
		if class == elfClass64 {
			if phentsize < 40 {
				return 0, 0, fmt.Errorf("program header %d: e_phentsize %d too small for ELF64", i, phentsize)
			}
			return int64(bo.Uint64(buf[8:])), int64(bo.Uint64(buf[32:])), nil
		}
		if phentsize < 20 {
			return 0, 0, fmt.Errorf("program header %d: e_phentsize %d too small for ELF32", i, phentsize)
		}
		return int64(bo.Uint32(buf[4:])), int64(bo.Uint32(buf[16:])), nil
	}
	return 0, 0, nil
}

func trimNUL(b []byte) []byte {
	for i, c := range b {
		if c == 0 {
			return b[:i]
		}
	}
	return b
}

func printablePrefix(b []byte) string {
	if len(b) > 16 {
		b = b[:16]
	}
	out := make([]rune, 0, len(b))
	for _, c := range b {
		if c >= 0x20 && c < 0x7f {
			out = append(out, rune(c))
		} else {
			out = append(out, '.')
		}
	}
	return string(out)
}

// ExpectELF verifies that path is an ELF binary for t. If wantStatic is
// non-nil the link mode must match it. When the file is dynamically linked its
// PT_INTERP must be musl's loader for t, which is what catches a toolchain that
// silently produced a glibc-linked binary.
func ExpectELF(path string, t triple.Triple, wantStatic *bool) error {
	machine, class, data, ok := t.ELF()
	if !ok {
		return fmt.Errorf("%s: no known ELF identity for triple %q", path, t)
	}
	info, err := ReadELF(path)
	if err != nil {
		return err
	}
	if info.Class != class || info.Data != data || info.Machine != machine {
		return fmt.Errorf("%s: expected %s, got %s", path,
			ELFID(class, data, machine), ELFID(info.Class, info.Data, info.Machine))
	}
	if err := expectLink(path, info, wantStatic); err != nil {
		return err
	}
	if info.Interp != "" && info.Interp != t.DynamicLinker() {
		return fmt.Errorf("%s: expected interpreter %s (musl for %s), got %s",
			path, t.DynamicLinker(), t, info.Interp)
	}
	return nil
}

// ExpectELFLike verifies path has the same machine/class/endianness as ref.
// It is how we assert "this binary is for the BUILD machine" without having a
// triple.Triple for the build system.
func ExpectELFLike(path string, ref ELFInfo, wantStatic *bool) error {
	info, err := ReadELF(path)
	if err != nil {
		return err
	}
	if info.Class != ref.Class || info.Data != ref.Data || info.Machine != ref.Machine {
		return fmt.Errorf("%s: expected %s (same as %s), got %s", path,
			ELFID(ref.Class, ref.Data, ref.Machine), ref.Path,
			ELFID(info.Class, info.Data, info.Machine))
	}
	return expectLink(path, info, wantStatic)
}

func expectLink(path string, info ELFInfo, wantStatic *bool) error {
	if wantStatic == nil || *wantStatic == info.Static {
		return nil
	}
	if *wantStatic {
		return fmt.Errorf("%s: expected a static binary (no PT_INTERP), got dynamic with interpreter %s", path, info.Interp)
	}
	return fmt.Errorf("%s: expected a dynamic binary (PT_INTERP present), got static", path)
}

// SelfELF returns the ELF identity of the running process's executable, used
// as the reference for "is this a BUILD-machine binary". It fails on platforms
// whose executables are not ELF (e.g. macOS), where callers should skip the
// check rather than fail it.
func SelfELF() (ELFInfo, error) {
	exe, err := os.Executable()
	if err != nil {
		return ELFInfo{}, fmt.Errorf("locate own executable: %w", err)
	}
	return ReadELF(exe)
}
