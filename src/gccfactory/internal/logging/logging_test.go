package logging

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// Every event must land in run.jsonl in a form a later tool can read back.
func TestJSONLStream(t *testing.T) {
	root := t.TempDir()
	var stderr bytes.Buffer
	no := false
	l, err := New(Options{RunsRoot: root, Stderr: &stderr, Level: LevelDebug, Color: &no})
	if err != nil {
		t.Fatal(err)
	}
	l.Named("cross_x86_64-linux-musl").Info("building", "step", "configure", "key", "abc123")
	l.Warn("careful")
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}

	b, err := os.ReadFile(filepath.Join(l.RunDir(), "run.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 events, got %d:\n%s", len(lines), b)
	}
	var ev Event
	if err := json.Unmarshal([]byte(lines[0]), &ev); err != nil {
		t.Fatalf("run.jsonl is not valid JSONL: %v", err)
	}
	if ev.Job != "cross_x86_64-linux-musl" || ev.Step != "configure" || ev.Msg != "building" {
		t.Fatalf("event lost structure: %+v", ev)
	}
	if ev.Fields["key"] != "abc123" {
		t.Fatalf("extra fields dropped: %+v", ev.Fields)
	}
	if !strings.Contains(stderr.String(), "cross_x86_64-linux-musl building") {
		t.Fatalf("human stream unreadable:\n%s", stderr.String())
	}
	if strings.Contains(stderr.String(), "\x1b[") {
		t.Fatal("colors emitted despite Color=false")
	}
}

func TestLevelGate(t *testing.T) {
	var stderr bytes.Buffer
	no := false
	l, err := New(Options{Stderr: &stderr, Level: LevelWarn, Color: &no})
	if err != nil {
		t.Fatal(err)
	}
	l.Debug("invisible")
	l.Info("invisible")
	l.Error("visible")
	if l.Enabled(LevelDebug) {
		t.Fatal("debug should be disabled at LevelWarn")
	}
	if got := stderr.String(); strings.Contains(got, "invisible") || !strings.Contains(got, "visible") {
		t.Fatalf("level gate leaked:\n%s", got)
	}
}

// The logger is shared by every worker goroutine; derived loggers must not
// alias each other's fields.
func TestConcurrentUseAndFieldIsolation(t *testing.T) {
	var stderr bytes.Buffer
	no := false
	l, _ := New(Options{RunsRoot: t.TempDir(), Stderr: &stderr, Level: LevelDebug, Color: &no})
	defer l.Close()

	a := l.Named("a").With("x", 1)
	b := l.Named("b")
	if _, ok := b.fields["x"]; ok {
		t.Fatal("With mutated the parent logger's fields")
	}

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			a.Info("from a", "i", i)
			b.Info("from b", "i", i)
		}(i)
	}
	wg.Wait()
}
