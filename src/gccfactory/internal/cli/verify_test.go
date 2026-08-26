package cli

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/junikimm717/gccfactory/src/gccfactory/internal/core"
	"github.com/junikimm717/gccfactory/src/gccfactory/internal/ensure"
)

func fakeVerifier(t *testing.T, n, workers int, exec func(verifyTask) verifyResult) *verifier {
	t.Helper()
	v := &verifier{e: &core.Env{MaxWorkers: workers}, ctx: context.Background(), exec: exec}
	for i := 0; i < n; i++ {
		v.add(fmt.Sprintf("task-%d", i), fmt.Sprintf("t%d", i), nil)
	}
	return v
}

func passing(subject string) *ensure.Report {
	r := ensure.NewReport(subject)
	r.Pass("probe", "ok")
	return r
}

func indexOfSlug(t *testing.T, slug string) int {
	t.Helper()
	var i int
	if _, err := fmt.Sscanf(slug, "verify_t%d", &i); err != nil {
		t.Fatalf("unexpected slug %q", slug)
	}
	return i
}

// The last task finishes first and the first task finishes last, which also
// requires all of them to be in flight at once -- a serial run would deadlock.
func TestVerifyReportsPrintInMatrixOrderWhateverFinishesFirst(t *testing.T) {
	const n = 5
	finished := make([]chan struct{}, n)
	for i := range finished {
		finished[i] = make(chan struct{})
	}
	var mu sync.Mutex
	var completion []int

	v := fakeVerifier(t, n, n, func(task verifyTask) verifyResult {
		i := indexOfSlug(t, task.slug)
		if i+1 < n {
			<-finished[i+1]
		}
		mu.Lock()
		completion = append(completion, i)
		mu.Unlock()
		close(finished[i])
		return verifyResult{report: passing(fmt.Sprintf("subject-%d", i))}
	})

	var err error
	out := captureStdout(t, func() { err = v.finish() })
	if err != nil {
		t.Fatalf("all tasks passed, got %v", err)
	}

	if completion[0] != n-1 || completion[n-1] != 0 {
		t.Fatalf("test did not force out-of-order completion: %v", completion)
	}
	var seen []int
	for _, line := range strings.Split(out, "\n") {
		var i int
		if _, err := fmt.Sscanf(line, "task-%d", &i); err == nil {
			seen = append(seen, i)
		}
	}
	if len(seen) != n {
		t.Fatalf("want %d headers, got %v\n%s", n, seen, out)
	}
	for i, got := range seen {
		if got != i {
			t.Fatalf("reports printed out of order: %v\n%s", seen, out)
		}
	}
	if !strings.Contains(out, "all 5 toolchains pass") {
		t.Errorf("missing summary line:\n%s", out)
	}
}

func TestVerifyOneFailureFailsTheRun(t *testing.T) {
	v := fakeVerifier(t, 3, 3, func(task verifyTask) verifyResult {
		if indexOfSlug(t, task.slug) != 1 {
			return verifyResult{report: passing(task.slug)}
		}
		r := ensure.NewReport(task.slug)
		r.Failf("probe", "boom")
		return verifyResult{report: r}
	})

	var err error
	out := captureStdout(t, func() { err = v.finish() })
	if err == nil || err.Error() != "1 of 3 toolchains failed verification" {
		t.Fatalf("got %v, want the documented failure summary", err)
	}
	if strings.Contains(out, "PASS") {
		t.Errorf("a failing run must not print a pass summary:\n%s", out)
	}
}

// A harness that cannot even start (no log dir, no probe workspace) has no
// report to print, but must still fail the run.
func TestVerifyHarnessErrorFailsTheRun(t *testing.T) {
	v := fakeVerifier(t, 2, 2, func(task verifyTask) verifyResult {
		if indexOfSlug(t, task.slug) == 0 {
			return verifyResult{errText: "error: cannot open log"}
		}
		return verifyResult{report: passing(task.slug)}
	})
	var err error
	captureStdout(t, func() { err = v.finish() })
	if err == nil || !strings.Contains(err.Error(), "failed verification") {
		t.Fatalf("got %v, want a failure", err)
	}
}

func TestVerifyCancellationStopsPromptly(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var mu sync.Mutex
	started := 0
	v := fakeVerifier(t, 8, 2, func(task verifyTask) verifyResult {
		mu.Lock()
		started++
		mu.Unlock()
		return verifyResult{report: passing(task.slug)}
	})
	v.ctx = ctx

	var err error
	timedOut := false
	captureStdout(t, func() {
		errc := make(chan error, 1)
		go func() { errc <- v.finish() }()
		select {
		case err = <-errc:
		case <-time.After(10 * time.Second):
			timedOut = true
		}
	})
	if timedOut {
		t.Fatal("finish did not return after cancellation")
	}
	if err == nil {
		t.Fatal("a cancelled run must not report success")
	}
	if started != 0 {
		t.Errorf("%d tasks ran after cancellation", started)
	}
}

// Two verifications running at once must not share a probe workspace: the
// pid in the name is per-process, so uniqueness rests on MkdirTemp's suffix.
func TestScratchDirIsUniquePerCall(t *testing.T) {
	e := &core.Env{Dist: t.TempDir()}
	const n = 24
	var mu sync.Mutex
	seen := map[string]bool{}
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d, err := scratchDir(e, "verify_same_slug")
			if err != nil {
				t.Error(err)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			if seen[d] {
				t.Errorf("two verifications got the same work dir %s", d)
			}
			seen[d] = true
		}()
	}
	wg.Wait()
	if len(seen) != n {
		t.Fatalf("got %d distinct work dirs, want %d", len(seen), n)
	}
}
