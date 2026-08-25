package ensure

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/junikimm717/gccfactory/src/gccfactory/internal/triple"
)

// An Emulator is how ensure executes a program built for an architecture the
// build machine cannot run itself. It is an argv prefix and nothing more,
// which is all the harness ever needed.
//
// qemu-user is the default and needs no configuration, but it exists only on
// Linux: linux-user mode translates Linux syscalls into host syscalls, so it
// needs a Linux kernel underneath and cannot be built for macOS. A wrapper is
// the escape hatch -- typically `docker run` into a Linux VM -- and keeps the
// choice of sandbox out of this package entirely.
type Emulator struct {
	Argv []string // argv prefix, already resolved for one architecture
	Qemu bool     // Argv[0] is qemu-user, so -L and QEMU_LD_PREFIX apply
}

// QemuEmulator is the default: a qemu-user binary taking -L <sysroot>.
func QemuEmulator(bin string) Emulator {
	if bin == "" {
		return Emulator{}
	}
	return Emulator{Argv: []string{bin}, Qemu: true}
}

// WrapperEmulator is any command that makes its argument runnable. It is told
// nothing about sysroots; a dynamic program reaches its libraries through the
// Loader retry instead.
func WrapperEmulator(argv []string) Emulator {
	if len(argv) == 0 {
		return Emulator{}
	}
	return Emulator{Argv: append([]string(nil), argv...)}
}

// The zero Emulator cannot, and every caller must say so rather than
// silently skipping.
func (e Emulator) Usable() bool { return len(e.Argv) > 0 }

func (e Emulator) Name() string {
	if !e.Usable() {
		return ""
	}
	return filepath.Base(e.Argv[0])
}

func (e Emulator) String() string {
	if !e.Usable() {
		return "<none>"
	}
	return shJoin(e.Argv)
}

// Launch is the prefix for running a program built for t whose shared
// libraries live under sysroot.
//
// It must work for a static program and a dynamic one alike: one prefix runs
// every probe, and staticness is not known here. qemu is told the sysroot with
// -L, which it ignores for a static binary. A wrapper is told nothing -- a
// loader prepended here would run a static binary as if it were dynamic
// ("Not a valid dynamic program") -- so its dynamic programs go through the
// Loader retry instead.
func (e Emulator) Launch(sysroot string, t triple.Triple, static bool) ([]string, map[string]string) {
	if !e.Usable() {
		return nil, nil
	}
	argv := append([]string(nil), e.Argv...)
	if e.Qemu && !static && sysroot != "" {
		return append(argv, "-L", sysroot), map[string]string{"QEMU_LD_PREFIX": sysroot}
	}
	return argv, nil
}

// Loader starts a program through musl's loader instead of by name. A wrapper
// is also told where the sysroot's libraries are, since nothing else will
// resolve libstdc++ for it -- via --library-path and not LD_LIBRARY_PATH,
// which would apply to the wrapper process too (docker is not a musl program).
func (e Emulator) Loader(sysroot string, t triple.Triple) []string {
	ld := LoaderPath(sysroot, t)
	if !e.Usable() || ld == "" {
		return nil
	}
	argv := append(append([]string(nil), e.Argv...), ld)
	if e.Qemu {
		return argv
	}
	return append(argv, "--library-path", filepath.Join(sysroot, "lib"))
}

// LoaderForDynamic reports whether a dynamic program has to be started through
// its loader rather than by name. qemu resolves the interpreter itself once it
// has -L; a wrapper cannot be told a sysroot, so nothing would resolve the
// program's absolute PT_INTERP inside it.
func (e Emulator) LoaderForDynamic() bool { return e.Usable() && !e.Qemu }

// FallbackNote explains a Loader retry. For qemu it means `-L <sysroot>` could
// not follow the loader symlink, which is a real degradation worth surfacing.
// For a wrapper it is simply how every dynamic program starts, so it is not
// worth a word on all of them.
func (e Emulator) FallbackNote() string {
	if !e.Qemu {
		return ""
	}
	return "DEGRADED: qemu -L <sysroot> could not load the interpreter; ran via "
}

// NoEmulator is the (check name, message) pair every caller must report when
// Usable is false.
// It names both ways out, because a user hitting this on macOS has no reason
// to guess that qemu-user is Linux-only.
func NoEmulator(t triple.Triple) (string, string) {
	return "emulator", "no way to run " + t.Raw + " binaries: pass --qemu-dir" +
		" (qemu-user, Linux only) or --exec-wrapper (any command that can run" +
		" them, e.g. `docker run` on macOS)"
}

// EmulatorSpec is the emulator configuration in its unresolved form: what the
// user typed, carried through core.Env, turned into an Emulator per
// architecture at the point of use. Wrapper wins over Qemu when both are set.
type EmulatorSpec struct {
	Qemu    string   // printf template with one %s for QemuName, e.g. /usr/bin/qemu-%s-static
	Wrapper []string // argv template; see expand for the placeholders
	Dist    string   // substituted for {dist}
}

func (s EmulatorSpec) For(t triple.Triple) (Emulator, error) {
	if len(s.Wrapper) > 0 {
		argv, err := s.expand(t)
		if err != nil {
			return Emulator{}, err
		}
		return WrapperEmulator(argv), nil
	}
	if s.Qemu == "" {
		return Emulator{}, nil
	}
	return QemuEmulator(fmt.Sprintf(s.Qemu, t.QemuName())), nil
}

// Placeholders are deliberately few and named, so an --exec-wrapper can be
// copied from the help text and edited without reading anything else.
func (s EmulatorSpec) expand(t triple.Triple) ([]string, error) {
	rep := map[string]string{
		"{triple}":   t.Raw,
		"{arch}":     t.QemuName(),
		"{platform}": t.Platform(),
		"{dist}":     s.Dist,
	}
	out := make([]string, 0, len(s.Wrapper))
	for _, a := range s.Wrapper {
		for k, v := range rep {
			if strings.Contains(a, k) {
				if v == "" {
					return nil, fmt.Errorf("--exec-wrapper %s: %s is undefined for %s", k, k, t)
				}
				a = strings.ReplaceAll(a, k, v)
			}
		}
		out = append(out, a)
	}
	return out, nil
}
