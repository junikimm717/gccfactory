// Package triple parses and describes the linux-musl target triples this
// factory supports. It is the shared vocabulary for every other package.
package triple

import (
	"fmt"
	"sort"
	"strings"
)

var Known = []string{
	"aarch64-linux-musl",
	"arm-linux-musleabi",
	"arm-linux-musleabihf",
	"i386-linux-musl",
	"mips64-linux-musl",
	"powerpc64-linux-musl",
	"powerpc64le-linux-musl",
	"riscv32-linux-musl",
	"riscv64-linux-musl",
	"s390x-linux-musl",
	"x86_64-linux-musl",
}

// "proven" means something different for the two roles: we prove two hosts but
// four targets. Everything in Known is buildable; these are what `build
// --host proven --target proven` and `verify` exercise end-to-end.
var (
	ProvenHosts = []string{
		"aarch64-linux-musl",
		"x86_64-linux-musl",
	}
	ProvenTargets = []string{
		"aarch64-linux-musl",
		"riscv32-linux-musl",
		"riscv64-linux-musl",
		"x86_64-linux-musl",
	}
)

// Proven is the union, for contexts with no host/target role (the picker's
// default selection, `help`). Prefer ProvenHosts/ProvenTargets when the role
// is known.
var Proven = union(ProvenHosts, ProvenTargets)

func union(lists ...[]string) []string {
	seen := map[string]bool{}
	var out []string
	for _, l := range lists {
		for _, s := range l {
			if !seen[s] {
				seen[s] = true
				out = append(out, s)
			}
		}
	}
	sort.Strings(out)
	return out
}

type Triple struct {
	Raw  string // "arm-linux-musleabihf"
	Arch string // "arm"
	OS   string // "linux"
	Libc string // "musl" | "musleabi" | "musleabihf"
}

type spec struct {
	machine uint16 // ELF e_machine
	class   byte   // 1 = ELF32, 2 = ELF64
	data    byte   // 1 = little endian, 2 = big endian
	qemu    string // qemu-user suffix
	ldso    string // musl dynamic linker basename suffix
	oci     string // OCI/docker platform, "" where none is defined
	gcc     []string
}

// ELF e_machine constants we care about.
const (
	emI386    = 3
	emMIPS    = 8
	emPPC64   = 21
	emS390    = 22
	emARM     = 40
	emX86_64  = 62
	emAARCH64 = 183
	emRISCV   = 243
)

var specs = map[string]spec{
	"aarch64-linux-musl":     {emAARCH64, 2, 1, "aarch64", "aarch64", "linux/arm64", nil},
	"x86_64-linux-musl":      {emX86_64, 2, 1, "x86_64", "x86_64", "linux/amd64", nil},
	"i386-linux-musl":        {emI386, 1, 1, "i386", "i386", "linux/386", []string{"--with-arch=i486", "--with-tune=generic"}},
	"arm-linux-musleabi":     {emARM, 1, 1, "arm", "arm", "linux/arm/v6", []string{"--with-arch=armv4t", "--with-float=soft"}},
	"arm-linux-musleabihf":   {emARM, 1, 1, "arm", "armhf", "linux/arm/v7", []string{"--with-arch=armv7-a", "--with-fpu=vfpv3-d16", "--with-float=hard", "--with-mode=thumb"}},
	"mips64-linux-musl":      {emMIPS, 2, 2, "mips64", "mips64", "linux/mips64", []string{"--with-arch=mips64r2", "--with-abi=64", "--with-float=hard"}},
	"powerpc64-linux-musl":   {emPPC64, 2, 2, "ppc64", "powerpc64", "linux/ppc64", []string{"--with-abi=elfv2", "--enable-secureplt", "--enable-decimal-float=no"}},
	"powerpc64le-linux-musl": {emPPC64, 2, 1, "ppc64le", "powerpc64le", "linux/ppc64le", []string{"--with-abi=elfv2", "--enable-secureplt", "--enable-decimal-float=no"}},
	"riscv32-linux-musl":     {emRISCV, 1, 1, "riscv32", "riscv32", "", []string{"--with-arch=rv32gc", "--with-abi=ilp32d"}},
	"riscv64-linux-musl":     {emRISCV, 2, 1, "riscv64", "riscv64", "linux/riscv64", []string{"--with-arch=rv64gc", "--with-abi=lp64d"}},
	"s390x-linux-musl":       {emS390, 2, 2, "s390x", "s390x", "linux/s390x", []string{"--with-arch=z196", "--with-tune=zEC12", "--with-long-double-128"}},
}

func Parse(s string) (Triple, error) {
	s = strings.TrimSpace(s)
	if _, ok := specs[s]; !ok {
		return Triple{}, fmt.Errorf("unknown triple %q (supported: %s)", s, strings.Join(Known, ", "))
	}
	parts := strings.SplitN(s, "-", 3)
	return Triple{Raw: s, Arch: parts[0], OS: parts[1], Libc: parts[2]}, nil
}

// MustParse is Parse for package-level initialization and tests.
func MustParse(s string) Triple {
	t, err := Parse(s)
	if err != nil {
		panic(err)
	}
	return t
}

// Role says whether a list is being parsed for --host or --target, which is
// what "proven" resolves against.
type Role int

const (
	RoleAny Role = iota
	RoleHost
	RoleTarget
)

func (r Role) proven() []string {
	switch r {
	case RoleHost:
		return ProvenHosts
	case RoleTarget:
		return ProvenTargets
	default:
		return Proven
	}
}

// ParseList expands a comma-separated list for an unspecified role.
func ParseList(s string) ([]Triple, error) { return ParseListFor(RoleAny, s) }

func ParseListFor(role Role, s string) ([]Triple, error) {
	var names []string
	for _, f := range strings.Split(s, ",") {
		switch f = strings.TrimSpace(f); f {
		case "":
		case "all":
			names = append(names, Known...)
		case "proven":
			names = append(names, role.proven()...)
		default:
			names = append(names, f)
		}
	}
	seen := map[string]bool{}
	var out []Triple
	for _, n := range names {
		if seen[n] {
			continue
		}
		seen[n] = true
		t, err := Parse(n)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Raw < out[j].Raw })
	return out, nil
}

func (t Triple) String() string { return t.Raw }

func (t Triple) ELF() (machine uint16, class, data byte, ok bool) {
	s, ok := specs[t.Raw]
	return s.machine, s.class, s.data, ok
}

// QemuName is the qemu-user binary suffix, e.g. "x86_64" for qemu-x86_64.
func (t Triple) QemuName() string { return specs[t.Raw].qemu }

// Platform is the OCI/docker platform that can run t, for --exec-wrapper
// templates. riscv32 has none: OCI never defined one, so a wrapper for it has
// to name its own runner.
func (t Triple) Platform() string { return specs[t.Raw].oci }

// DynamicLinker is the absolute path (inside the sysroot) of musl's ld.so.
func (t Triple) DynamicLinker() string {
	return "/lib/ld-musl-" + specs[t.Raw].ldso + ".so.1"
}

func (t Triple) GCCConfig() []string {
	s := specs[t.Raw]
	out := make([]string, len(s.gcc))
	copy(out, s.gcc)
	return out
}

func (t Triple) Bits() int {
	if specs[t.Raw].class == 1 {
		return 32
	}
	return 64
}

func (t Triple) BigEndian() bool { return specs[t.Raw].data == 2 }
