package ensure

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/junikimm717/gccfactory/src/gccfactory/internal/triple"
)

func withBinfmtDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	old := BinfmtDir
	BinfmtDir = dir
	t.Cleanup(func() { BinfmtDir = old })
	return dir
}

func writeBinfmt(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Real registrations pulled from a running kernel's binfmt_misc, used to
// verify the endianness handling against ground truth rather than a
// hand-derived example.
const (
	aarch64Entry = "enabled\n" +
		"interpreter /usr/bin/qemu-aarch64-static\n" +
		"flags: F\n" +
		"offset 0\n" +
		"magic 7f454c460201010000000000000000000200b700\n" +
		"mask ffffffffffffff00fffffffffffffffffeffffff\n"
	s390xEntry = "enabled\n" +
		"interpreter /usr/bin/qemu-s390x-static\n" +
		"flags: F\n" +
		"offset 0\n" +
		"magic 7f454c4602020100000000000000000000020016\n" +
		"mask ffffffffffffff00fffffffffffffffffffeffff\n"
	ppc64Entry = "enabled\n" +
		"interpreter /usr/bin/qemu-ppc64-static\n" +
		"flags: F\n" +
		"offset 0\n" +
		"magic 7f454c4602020100000000000000000000020015\n" +
		"mask ffffffffffffff00fffffffffffffffffffeffff\n"
	mips64Entry = "enabled\n" +
		"interpreter /usr/bin/qemu-mips64-static\n" +
		"flags: F\n" +
		"offset 0\n" +
		"magic 7f454c4602020100000000000000000000020008\n" +
		"mask ffffffffffffff0000fffffffffffffffffeffff\n"
)

func TestBinfmtParse(t *testing.T) {
	dir := withBinfmtDir(t)
	writeBinfmt(t, dir, "qemu-aarch64", aarch64Entry)
	writeBinfmt(t, dir, "qemu-arm", "disabled\n"+
		"interpreter /usr/bin/qemu-arm-static\n"+
		"flags: F\n"+
		"offset 0\n"+
		"magic 7f454c4601010100000000000000000002002800\n") // no mask line
	writeBinfmt(t, dir, "qemu-empty-flags", "enabled\n"+
		"interpreter /usr/bin/qemu-riscv32-static\n"+
		"flags:\n"+
		"offset 0\n"+
		"magic 7f454c460101010000000000000000000200f300\n"+
		"mask ffffffffffffff00fffffffffffffffffeffffff\n")
	writeBinfmt(t, dir, "python3.11", "enabled\ninterpreter /usr/bin/python3.11\nflags: \nextension py\n")
	writeBinfmt(t, dir, "register", "should not be parsed")
	writeBinfmt(t, dir, "status", "enabled")
	writeBinfmt(t, dir, "garbage", "this is not a binfmt_misc entry at all\n")

	entries, err := Binfmt()
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]BinfmtEntry{}
	for _, e := range entries {
		byName[e.Name] = e
	}
	if _, ok := byName["register"]; ok {
		t.Error("register must be skipped")
	}
	if _, ok := byName["status"]; ok {
		t.Error("status must be skipped")
	}
	if _, ok := byName["garbage"]; ok {
		t.Error("a garbage file must be skipped")
	}
	if _, ok := byName["python3.11"]; ok {
		t.Error("an extension entry (no magic) must be skipped")
	}

	aarch64, ok := byName["qemu-aarch64"]
	if !ok {
		t.Fatal("qemu-aarch64 missing")
	}
	if !aarch64.Enabled || aarch64.Interpreter != "/usr/bin/qemu-aarch64-static" || aarch64.Flags != "F" || aarch64.Offset != 0 {
		t.Errorf("qemu-aarch64 parsed wrong: %+v", aarch64)
	}

	arm, ok := byName["qemu-arm"]
	if !ok {
		t.Fatal("qemu-arm missing")
	}
	if arm.Enabled {
		t.Error("qemu-arm should be disabled")
	}
	for i, b := range arm.Mask {
		if b != 0xff {
			t.Fatalf("missing mask must default to all-0xff, got byte %d = %#x", i, b)
		}
	}
	if len(arm.Mask) != len(arm.Magic) {
		t.Fatalf("default mask length %d != magic length %d", len(arm.Mask), len(arm.Magic))
	}

	empty, ok := byName["qemu-empty-flags"]
	if !ok {
		t.Fatal("qemu-empty-flags missing")
	}
	if empty.Flags != "" {
		t.Errorf("empty flags: should parse as \"\", got %q", empty.Flags)
	}
}

func TestBinfmtAbsentDir(t *testing.T) {
	old := BinfmtDir
	BinfmtDir = filepath.Join(t.TempDir(), "does-not-exist")
	t.Cleanup(func() { BinfmtDir = old })

	entries, err := Binfmt()
	if err != nil || entries != nil {
		t.Fatalf("expected (nil, nil) for an absent binfmt_misc, got (%v, %v)", entries, err)
	}
}

func hdrFor(t *testing.T, tr triple.Triple, etype byte) []byte {
	t.Helper()
	hdrs := ELFHeaders(tr)
	if len(hdrs) != 2 {
		t.Fatalf("expected 2 candidate headers, got %d", len(hdrs))
	}
	// etExec = 2, etDyn = 3, matching the offsets ELFHeaders builds them at.
	if etype == 2 {
		return hdrs[0]
	}
	return hdrs[1]
}

func TestBinfmtMatchesLittleEndian(t *testing.T) {
	e, ok := parseBinfmtEntry("qemu-aarch64", []byte(aarch64Entry))
	if !ok {
		t.Fatal("failed to parse fixture entry")
	}
	tr := triple.MustParse("aarch64-linux-musl")
	if !e.Matches(hdrFor(t, tr, 2)) {
		t.Error("qemu-aarch64 registration should match an ET_EXEC aarch64 header")
	}
	if !e.Matches(hdrFor(t, tr, 3)) {
		t.Error("qemu-aarch64 registration should match an ET_DYN aarch64 header")
	}
	other := triple.MustParse("x86_64-linux-musl")
	if e.Matches(hdrFor(t, other, 2)) {
		t.Error("qemu-aarch64 registration must not match an x86_64 header")
	}
}

// This is the case a naive little-endian-only implementation gets wrong:
// e_type/e_machine in the candidate header must be big-endian too.
func TestBinfmtMatchesBigEndian(t *testing.T) {
	for _, tc := range []struct {
		name, entry, triple string
	}{
		{"qemu-s390x", s390xEntry, "s390x-linux-musl"},
		{"qemu-ppc64", ppc64Entry, "powerpc64-linux-musl"},
		{"qemu-mips64", mips64Entry, "mips64-linux-musl"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, ok := parseBinfmtEntry(tc.name, []byte(tc.entry))
			if !ok {
				t.Fatal("failed to parse fixture entry")
			}
			tr := triple.MustParse(tc.triple)
			if !e.Matches(hdrFor(t, tr, 2)) {
				t.Errorf("%s registration should match its own big-endian ET_EXEC header", tc.name)
			}
			if !e.Matches(hdrFor(t, tr, 3)) {
				t.Errorf("%s registration should match its own big-endian ET_DYN header", tc.name)
			}
			// A little-endian header of the same machine must not match.
			leHdr := append([]byte(nil), hdrFor(t, tr, 2)...)
			leHdr[5] = elfDataLE
			if e.Matches(leHdr) {
				t.Errorf("%s registration must not match a little-endian header", tc.name)
			}
		})
	}
}

func TestBinfmtForSkipsDisabled(t *testing.T) {
	dir := withBinfmtDir(t)
	writeBinfmt(t, dir, "qemu-aarch64", "disabled\n"+
		"interpreter /usr/bin/qemu-aarch64-static\n"+
		"flags: F\n"+
		"offset 0\n"+
		"magic 7f454c460201010000000000000000000200b700\n"+
		"mask ffffffffffffff00fffffffffffffffffeffffff\n")

	entries, err := Binfmt()
	if err != nil {
		t.Fatal(err)
	}
	tr := triple.MustParse("aarch64-linux-musl")
	if _, ok := BinfmtFor(tr, entries); ok {
		t.Error("a disabled entry must not be selected by BinfmtFor")
	}
}

func TestBinfmtForEnabled(t *testing.T) {
	dir := withBinfmtDir(t)
	writeBinfmt(t, dir, "qemu-s390x", s390xEntry)
	writeBinfmt(t, dir, "qemu-aarch64", aarch64Entry)

	entries, err := Binfmt()
	if err != nil {
		t.Fatal(err)
	}
	tr := triple.MustParse("s390x-linux-musl")
	got, ok := BinfmtFor(tr, entries)
	if !ok || got.Name != "qemu-s390x" {
		t.Fatalf("expected qemu-s390x, got %+v ok=%v", got, ok)
	}
}

// ELFHeaders must report failure, not panic, for a triple it has no ELF
// identity for.
func TestELFHeadersUnknownTriple(t *testing.T) {
	if got := ELFHeaders(triple.Triple{Raw: "bogus-linux-musl"}); got != nil {
		t.Fatalf("expected nil for an unknown triple, got %v", got)
	}
}

// TestBinfmtSurvey reads the REAL binfmt_misc table on the machine running
// the test and reports, for every known triple, how it would execute. It
// asserts nothing about which entries exist — that is a property of this
// machine, not of the code.
func TestBinfmtSurvey(t *testing.T) {
	if _, err := os.Stat("/proc/sys/fs/binfmt_misc"); err != nil {
		t.Skip("no /proc/sys/fs/binfmt_misc on this machine")
	}
	old := BinfmtDir
	BinfmtDir = "/proc/sys/fs/binfmt_misc"
	t.Cleanup(func() { BinfmtDir = old })

	entries, err := Binfmt()
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range triple.Known {
		tr := triple.MustParse(raw)
		if e, ok := BinfmtFor(tr, entries); ok {
			t.Logf("%-24s -> %-16s via %s", raw, e.Name, e.Interpreter)
		} else {
			t.Logf("%-24s -> no binfmt_misc registration would execute this", raw)
		}
	}
}
