package cli

import (
	"bufio"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeFile(t *testing.T, dir, name string, size int) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), make([]byte, size), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A cached size that never refreshes would report a rebuilt artifact at its old
// size forever, so the publish timestamp has to be the key -- not the path.
func TestSizeCacheRefreshesOnRepublish(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "a", 1000)
	c := newSizeCache()

	first := time.Now()
	if got := c.of(dir, first); got != 1000 {
		t.Fatalf("first read = %d, want 1000", got)
	}

	// Same publish timestamp: the artifact is immutable, so the walk must be
	// skipped even though the directory grew underneath us.
	writeFile(t, dir, "b", 500)
	if got := c.of(dir, first); got != 1000 {
		t.Errorf("same timestamp re-walked the tree: got %d, want the cached 1000", got)
	}

	// A republish moves the timestamp and must invalidate.
	if got := c.of(dir, first.Add(time.Second)); got != 1500 {
		t.Errorf("republish did not invalidate: got %d, want 1500", got)
	}
}

func TestSizeCacheSeparatesDirs(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	writeFile(t, a, "f", 10)
	writeFile(t, b, "f", 20)
	c := newSizeCache()
	at := time.Now()
	if c.of(a, at) != 10 || c.of(b, at) != 20 {
		t.Error("two artifacts sharing a publish timestamp collided in the cache")
	}
}

func renderFrame(t *testing.T, frame string) string {
	t.Helper()
	var buf bytes.Buffer
	w := bufio.NewWriter(&buf)
	paintFrame(w, frame)
	return buf.String()
}

// Clearing the whole screen between frames is what makes a watch flicker; the
// redraw must clear per line instead.
func TestPaintFrameNeverClearsWholeScreen(t *testing.T) {
	out := renderFrame(t, "one\ntwo\nthree\n")
	if strings.Contains(out, "\x1b[2J") {
		t.Error("frame used a full-screen clear")
	}
	if !strings.HasPrefix(out, "\x1b[H") {
		t.Error("frame did not home the cursor first")
	}
	for _, want := range []string{"one\x1b[K", "two\x1b[K", "three\x1b[K"} {
		if !strings.Contains(out, want) {
			t.Errorf("line not cleared to end of line: %q missing", want)
		}
	}
	if !strings.HasSuffix(out, "\x1b[J") {
		t.Error("frame did not clear the rows below it")
	}
}

// A frame taller than the terminal would scroll, which desyncs the next
// frame's cursor-home and turns the view into confetti.
func TestPaintFrameCutsOversizedFrames(t *testing.T) {
	t.Setenv("LINES", "10")
	t.Setenv("COLUMNS", "80")
	rowsBefore := func() int { _, r := termSize(); return r }()

	var tall strings.Builder
	for i := 0; i < rowsBefore*3; i++ {
		tall.WriteString("line\n")
	}
	out := renderFrame(t, tall.String())
	got := strings.Count(out, "\x1b[K")
	if got >= rowsBefore*3 {
		t.Errorf("emitted %d lines for a %d-row terminal; frame was not cut", got, rowsBefore)
	}
	if !strings.Contains(out, "cut to fit") {
		t.Error("cut frame did not say it was cut")
	}
}
