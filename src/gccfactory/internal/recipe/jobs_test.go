package recipe

import (
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/junikimm717/gccfactory/src/gccfactory/internal/core"
	"github.com/junikimm717/gccfactory/src/gccfactory/internal/triple"
)

// closure walks the whole DAG from a set of roots, returning every slug.
func closure(roots []core.Job) map[string]core.Job {
	out := map[string]core.Job{}
	var visit func(core.Job)
	visit = func(j core.Job) {
		if _, seen := out[j.Slug()]; seen {
			return
		}
		out[j.Slug()] = j
		for _, d := range j.Deps() {
			visit(d)
		}
	}
	for _, r := range roots {
		visit(r)
	}
	return out
}

func slugsOf(m map[string]core.Job) []string {
	out := make([]string, 0, len(m))
	for s := range m {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// The slug is the artifact path, the lock path and the log directory. If two
// jobs ever collided, they would share all three.
func TestSlugsAreStableAndUniqueAcrossTheWholeMatrix(t *testing.T) {
	hosts := []triple.Triple{triple.MustParse("x86_64-linux-musl"), triple.MustParse("aarch64-linux-musl")}
	targets := allTriples(t)

	all := closure(Matrix(hosts, targets))
	if len(all) == 0 {
		t.Fatal("empty DAG")
	}
	for slug, j := range all {
		if slug == "" || strings.ContainsAny(slug, "/\\ \t") {
			t.Errorf("slug %q is not filesystem-safe", slug)
		}
		if got := j.Slug(); got != slug {
			t.Errorf("Slug() is not stable: %q then %q", slug, got)
		}
	}

	// 11 targets + 2 hosts, deduped -> 11 cross jobs.
	want := map[string]bool{
		"cross_x86_64-linux-musl":                            true,
		"cross_riscv32-linux-musl":                           true,
		"hostmake_x86_64-linux-musl":                         true,
		"hostmake_aarch64-linux-musl":                        true,
		"canadian_x86_64-linux-musl__powerpc64le-linux-musl": true,
		"canadian_aarch64-linux-musl__arm-linux-musleabihf":  true,
		"srctree_gcc-14.2.0":                                 true,
		"srctree_binutils-2.44":                              true,
	}
	for s := range want {
		if _, ok := all[s]; !ok {
			t.Errorf("expected slug %q in the DAG; have:\n  %s", s, strings.Join(slugsOf(all), "\n  "))
		}
	}

	var crossN, canadianN, hostmakeN int
	for _, j := range all {
		switch j.Name() {
		case "cross":
			crossN++
		case "canadian":
			canadianN++
		case "hostmake":
			hostmakeN++
		}
	}
	if crossN != len(targets) {
		t.Errorf("want %d cross jobs (one per triple, hosts dedupe into targets), got %d", len(targets), crossN)
	}
	if canadianN != len(hosts)*len(targets) {
		t.Errorf("want %d canadian jobs, got %d", len(hosts)*len(targets), canadianN)
	}
	if hostmakeN != len(hosts) {
		t.Errorf("want %d hostmake jobs, got %d", len(hosts), hostmakeN)
	}
}

// A canadian toolchain needs the host's compiler, the target's compiler, and a
// make for the host. Missing any of the three is a silent wrong-arch build.
func TestCanadianDependsOnBothCrossToolchainsAndHostMake(t *testing.T) {
	h := triple.MustParse("x86_64-linux-musl")
	tt := triple.MustParse("aarch64-linux-musl")
	dag := closure([]core.Job{Canadian(h, tt)})

	for _, want := range []string{
		"cross_x86_64-linux-musl",
		"cross_aarch64-linux-musl",
		"hostmake_x86_64-linux-musl",
		"srctree_gcc-14.2.0",
		"srctree_gmp-6.3.0",
		"srctree_isl-0.27",
	} {
		if _, ok := dag[want]; !ok {
			t.Errorf("Canadian(%s, %s) closure is missing %q; have:\n  %s",
				h, tt, want, strings.Join(slugsOf(dag), "\n  "))
		}
	}

	// The canadian job compiles no target code, so it must not pull musl or
	// the kernel headers in directly; it inherits them from cross:<T>.
	for _, d := range Canadian(h, tt).Deps() {
		if strings.HasPrefix(d.Slug(), "srctree_musl") || strings.HasPrefix(d.Slug(), "srctree_linux") {
			t.Errorf("canadian must not depend directly on %s: target code comes from cross:<T>", d.Slug())
		}
	}
}

func TestCrossDependsOnEveryInTreeGCCLibrary(t *testing.T) {
	deps := map[string]bool{}
	for _, d := range Cross(triple.MustParse("s390x-linux-musl")).Deps() {
		deps[d.Slug()] = true
	}
	for _, want := range []string{
		"srctree_binutils-2.44", "srctree_gcc-14.2.0", "srctree_musl-1.2.5",
		"srctree_gmp-6.3.0", "srctree_mpfr-4.2.2", "srctree_mpc-1.3.1",
		"srctree_isl-0.27", "srctree_linux-headers-4.19.88-2",
	} {
		if !deps[want] {
			t.Errorf("cross job is missing dep %q (gmp/mpfr/mpc/isl are built in-tree, so their sources are required)", want)
		}
	}
}

func TestMatrixOneSidedForms(t *testing.T) {
	h := triple.MustParse("x86_64-linux-musl")
	tt := triple.MustParse("riscv64-linux-musl")

	targetsOnly := Matrix(nil, []triple.Triple{tt})
	if len(targetsOnly) != 1 || targetsOnly[0].Slug() != "cross_riscv64-linux-musl" {
		t.Errorf("--target alone should build the cross toolchain, got %v", slugsOf(closure(targetsOnly)))
	}
	hostsOnly := closure(Matrix([]triple.Triple{h}, nil))
	for _, want := range []string{"cross_x86_64-linux-musl", "hostmake_x86_64-linux-musl"} {
		if _, ok := hostsOnly[want]; !ok {
			t.Errorf("--host alone should build %q", want)
		}
	}
	if len(Matrix(nil, nil)) != 0 {
		t.Error("an empty matrix should produce no jobs")
	}
}

// KeyInputs feeds the content key, so it has to be a function of the recipe and
// nothing else -- in particular not of the pid-tagged work directory.
func TestKeyInputsAreDeterministicAndPathIndependent(t *testing.T) {
	h := triple.MustParse("x86_64-linux-musl")
	tt := triple.MustParse("arm-linux-musleabi")
	for _, j := range []core.Job{Cross(tt), HostMake(h), Canadian(h, tt), srcTreeJob(pkgGCC)} {
		a, b := j.KeyInputs(), j.KeyInputs()
		if !reflect.DeepEqual(a, b) {
			t.Errorf("%s: KeyInputs is not stable across calls:\n%v\n%v", j.Slug(), a, b)
		}
		if a["recipe_version"] == "" {
			t.Errorf("%s: KeyInputs must carry the recipe version", j.Slug())
		}
		for k, v := range a {
			for _, leak := range []string{"/tmp/", "/Users/", "/var/folders/", "dist/work/"} {
				if strings.Contains(v, leak) {
					t.Errorf("%s: KeyInputs[%s] leaks a build-machine path (%s): %s", j.Slug(), k, leak, v)
				}
			}
		}
	}

	// The sentinel is what makes that true: the work dir appears in
	// --with-debug-prefix-map and in every *_FOR_TARGET path.
	in := Cross(tt).KeyInputs()
	if !strings.Contains(in["gcc_config"], keyWork) {
		t.Errorf("expected the work-dir sentinel %q in the keyed gcc config: %s", keyWork, in["gcc_config"])
	}
}

// A changed configure flag must change the key, or a stale toolchain silently
// survives an edit to this package.
func TestKeyInputsFollowConfigureFlags(t *testing.T) {
	tt := triple.MustParse("x86_64-linux-musl")
	before := Cross(tt).KeyInputs()

	saved := makeFlags
	makeFlags = append(append([]string{}, saved...), "SOMETHING_NEW=1")
	after := Cross(tt).KeyInputs()
	makeFlags = saved

	if reflect.DeepEqual(before, after) {
		t.Fatal("adding a shared make flag did not change the key inputs")
	}
	if restored := Cross(tt).KeyInputs(); !reflect.DeepEqual(before, restored) {
		t.Fatal("key inputs did not return to their original value")
	}
}

// Two different targets must never share a key, or one toolchain's artifact
// would validate the other's.
func TestKeyInputsDifferPerTriple(t *testing.T) {
	seen := map[string]string{}
	for _, tt := range allTriples(t) {
		in := Cross(tt).KeyInputs()
		fp := strings.Join([]string{in["target"], in["gcc_config"], in["binutils_config"], in["musl_config"]}, "\x00")
		if prev, dup := seen[fp]; dup {
			t.Errorf("cross:%s and cross:%s have identical key inputs", prev, tt)
		}
		seen[fp] = tt.Raw
	}
}

// Keys are Merkle hashes, so this also proves every dependency key resolves.
func TestEveryJobInTheMatrixHasAUniqueKey(t *testing.T) {
	hosts := []triple.Triple{triple.MustParse("x86_64-linux-musl"), triple.MustParse("aarch64-linux-musl")}
	all := closure(Matrix(hosts, allTriples(t)))
	keys := map[string]string{}
	for slug, j := range all {
		k, err := core.Key(j)
		if err != nil {
			t.Fatalf("core.Key(%s): %v", slug, err)
		}
		if prev, dup := keys[k]; dup {
			t.Errorf("%s and %s hash to the same key", prev, slug)
		}
		keys[k] = slug
	}
}

// The fallback ladder is recorded in the manifest, but only after a build has
// chosen a rung; before that the key must not depend on it.
func TestCanadianRungIsRecordedWithoutDestabilizingTheKey(t *testing.T) {
	h := triple.MustParse("aarch64-linux-musl")
	tt := triple.MustParse("s390x-linux-musl")
	j := Canadian(h, tt).(*canadian)

	if _, ok := j.KeyInputs()["host_stage_used"]; ok {
		t.Fatal("host_stage_used must be absent until a build picks a rung")
	}
	if lad := j.KeyInputs()["fallback_ladder"]; !strings.Contains(lad, "all-host") {
		t.Errorf("the ladder itself must be keyed, got %q", lad)
	}

	j.mu.Lock()
	j.rung = "all-host"
	j.mu.Unlock()
	defer func() { j.mu.Lock(); j.rung = ""; j.mu.Unlock() }()

	if got := j.KeyInputs()["host_stage_used"]; got != "all-host" {
		t.Errorf("after a build the manifest inputs must name the rung, got %q", got)
	}
}

func TestArtifactPathsAreDistinct(t *testing.T) {
	e := &core.Env{Dist: "/dist"}
	h := triple.MustParse("x86_64-linux-musl")
	tt := triple.MustParse("aarch64-linux-musl")
	got := map[string]string{
		Cross(tt).ArtifactDir(e):          "cross",
		HostMake(h).ArtifactDir(e):        "hostmake",
		Canadian(h, tt).ArtifactDir(e):    "canadian",
		srcTreeJob(pkgGCC).ArtifactDir(e): "srctree",
	}
	if len(got) != 4 {
		t.Errorf("artifact directories collide: %v", got)
	}
	if d := Canadian(h, tt).ArtifactDir(e); d != "/dist/toolchains/out/x86_64-linux-musl/aarch64-linux-musl" {
		t.Errorf("canadian artifact dir = %q", d)
	}
}

// The diagonal is a weaker test and a different gcc configuration, so it must
// not be the job that aborts a matrix run.
func TestMatrixSchedulesOffDiagonalFirst(t *testing.T) {
	hosts := []triple.Triple{triple.MustParse("aarch64-linux-musl"), triple.MustParse("x86_64-linux-musl")}
	targets := []triple.Triple{
		triple.MustParse("aarch64-linux-musl"), triple.MustParse("riscv32-linux-musl"),
		triple.MustParse("riscv64-linux-musl"), triple.MustParse("x86_64-linux-musl"),
	}
	jobs := Matrix(hosts, targets)
	if got := jobs[0].Slug(); got != "canadian_aarch64-linux-musl__riscv32-linux-musl" {
		t.Errorf("first job = %s, want the aarch64->riscv32 cell", got)
	}
	seenDiagonal := false
	for _, j := range jobs {
		h, tt, _ := strings.Cut(strings.TrimPrefix(j.Slug(), "canadian_"), "__")
		if h == tt {
			seenDiagonal = true
		} else if seenDiagonal {
			t.Errorf("%s runs after a diagonal job", j.Slug())
		}
	}
}

// The diagonal ships a different sysroot layout, so it must not share a key
// with a build that did not apply it.
func TestDiagonalKeyDiffersFromOffDiagonalLayout(t *testing.T) {
	a, r := triple.MustParse("aarch64-linux-musl"), triple.MustParse("riscv32-linux-musl")
	diag := Canadian(a, a).KeyInputs()["sysroot_layout"]
	off := Canadian(a, r).KeyInputs()["sysroot_layout"]
	if diag == off {
		t.Fatalf("diagonal and off-diagonal share sysroot_layout %q", diag)
	}
	if !strings.HasPrefix(diag, off) {
		t.Errorf("diagonal layout %q should extend the base layout %q", diag, off)
	}
}
