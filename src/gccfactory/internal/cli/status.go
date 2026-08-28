package cli

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/junikimm717/gccfactory/src/gccfactory/internal/core"
	"github.com/junikimm717/gccfactory/src/gccfactory/internal/recipe"
	"github.com/junikimm717/gccfactory/src/gccfactory/internal/triple"
)

var cmdStatus = &command{
	Name:     "status",
	Short:    "what exists, what's stale, what's building right now",
	Synopsis: "gccfactory status [--host LIST] [--target LIST] [--all] [--watch [--interval DUR]]",
	Long: `Surveys dist/ and prints, for every job in the matrix, whether its artifact is
current (its content key matches), stale (an artifact exists but its inputs
changed), being built right now (by this or any other process sharing dist/),
or missing.

By default the whole ` + "`all` x `all`" + ` matrix is surveyed but only interesting rows
are listed, plus a compact grid of the 11x11 canadian matrix. Use --all to list
every job as a row, or --host/--target to narrow the survey.

Safe to run at any time, including while a build is in progress and before
anything has ever been built.

  STATES   ` + "`ok`" + ` current   ` + "`stale`" + ` inputs changed   ` + "`building`" + ` live heartbeat   ` + "`missing`" + `

WATCHING A BUILD
  --watch redraws in place until you ctrl-c, which is the thing to run in a
  second terminal while a build works. The header carries a clock, so a screen
  that is not changing still visibly proves the view is live. Default refresh
  is 1s; --interval 500ms gives 2Hz.

  Only the states move between frames -- artifact sizes are cached on the
  publish timestamp, so watching does not rewalk gigabytes of dist/ per frame.

  A build in its verification pass has no jobs left to report and shows as
  ` + "`building 0`" + `; verification is not a job. Follow it with
  ` + "`gccfactory logs`" + ` or the run's run.jsonl instead.`,
	Run: runStatus,
}

const (
	watchMinInterval = 100 * time.Millisecond
	watchDefaultRate = time.Second
)

func runStatus(g *Global, args []string) error {
	fs := g.flagSet("status")
	host := fs.String("host", "all", tripleFlagHelp)
	target := fs.String("target", "all", tripleFlagHelp)
	all := fs.Bool("all", false, "list every job as a row instead of summarizing")
	watch := fs.Bool("watch", false, "redraw in place until interrupted")
	interval := fs.Duration("interval", watchDefaultRate, "refresh period for --watch (e.g. 500ms for 2Hz)")
	if err := parse(fs, args); err != nil {
		return finish("status", err)
	}
	hosts, err := parseTriples("host", *host)
	if err != nil {
		return err
	}
	targets, err := parseTriples("target", *target)
	if err != nil {
		return err
	}
	if *interval < watchMinInterval {
		return finish("status", fmt.Errorf("--interval %s is below the %s floor; a faster redraw costs more than it shows",
			*interval, watchMinInterval))
	}

	e, done, err := g.env(defaultJobs, defaultWorkers)
	if err != nil {
		return err
	}
	defer done()

	sizes := newSizeCache()
	if !*watch {
		return renderStatus(os.Stdout, e, hosts, targets, *all, sizes, 0)
	}
	return watchStatus(*interval, func(w io.Writer) error {
		return renderStatus(w, e, hosts, targets, *all, sizes, *interval)
	})
}

func renderStatus(w io.Writer, e *core.Env, hosts, targets []triple.Triple, all bool, sizes *sizeCache, watching time.Duration) error {
	roots := rootJobs(hosts, targets)
	nodes, err := core.Plan(e, roots)
	if err != nil {
		return err
	}
	type row struct {
		node  core.PlanNode
		state string
	}
	byslug := map[string]row{}
	var counts struct{ ok, stale, building, missing int }
	for _, n := range nodes {
		st := "missing"
		if n.Valid {
			st = "ok"
		} else if _, exists := artifactManifest(n.Job.ArtifactDir(e)); exists {
			st = "stale"
		}
		if liveHeartbeat(e, n.Job.Slug()) != nil {
			st = "building"
		}
		switch st {
		case "ok":
			counts.ok++
		case "stale":
			counts.stale++
		case "building":
			counts.building++
		default:
			counts.missing++
		}
		byslug[n.Job.Slug()] = row{n, st}
	}

	fmt.Fprintf(w, "%s %s", bold("dist:"), e.Dist)
	if watching > 0 {
		fmt.Fprintf(w, "   %s", dim(fmt.Sprintf("every %s · %s · ctrl-c to stop",
			watching, time.Now().Format("15:04:05"))))
	}
	fmt.Fprintln(w)
	fmt.Fprintf(w, "  %s %d   %s %d   %s %d   %s %d   (of %d jobs)\n\n",
		green("ok"), counts.ok, yellow("stale"), counts.stale,
		blue("building"), counts.building, dim("missing"), counts.missing, len(nodes))

	t := newTable("STATE", "SLUG", "KEY", "SIZE", "BUILT", "BY")
	shown := 0
	for _, n := range nodes {
		r := byslug[n.Job.Slug()]
		if !all && r.state == "missing" {
			continue
		}
		if !all && n.Job.Name() == "canadian" {
			continue // covered by the grid below
		}
		shown++
		size, built, by := "-", "-", "-"
		if r.state == "ok" || r.state == "stale" {
			dir := n.Job.ArtifactDir(e)
			if m, ok := artifactManifest(dir); ok {
				built, by = humanAgo(m.CompletedAt), m.BuiltBy
				size = humanBytes(sizes.of(dir, m.CompletedAt))
			} else {
				size = humanBytes(dirSize(dir))
			}
		}
		if h := liveHeartbeat(e, n.Job.Slug()); h != nil {
			by = who(h)
			built = h.Step
			if built == "" {
				built = "…"
			}
		}
		t.add(colorState(r.state), n.Job.Slug(), dim(shortKey(n.Key)), size, built, dim(by))
	}
	if shown > 0 {
		t.rightAlign(3).render(w)
		fmt.Fprintln(w)
	}

	if !all {
		printGrid(w, hosts, targets, func(slug string) string {
			if r, ok := byslug[slug]; ok {
				return r.state
			}
			return "missing"
		})
	}

	if counts.ok == 0 && counts.building == 0 {
		fmt.Fprintf(w, "\nNothing built yet. Try %s\n", cyan("gccf build --host proven --target proven"))
	}
	return nil
}

// dirSize walks an entire artifact tree, which for a toolchain is gigabytes.
// Published artifacts are immutable, so the publish timestamp is a sound cache
// key and a redraw costs no disk.
type sizeCache struct {
	seen map[string]sizeEntry
}

type sizeEntry struct {
	at   time.Time
	size int64
}

func newSizeCache() *sizeCache { return &sizeCache{seen: map[string]sizeEntry{}} }

func (c *sizeCache) of(dir string, publishedAt time.Time) int64 {
	if e, ok := c.seen[dir]; ok && e.at.Equal(publishedAt) {
		return e.size
	}
	n := dirSize(dir)
	c.seen[dir] = sizeEntry{at: publishedAt, size: n}
	return n
}

func watchStatus(interval time.Duration, frame func(io.Writer) error) error {
	if !isTTY(os.Stdout) {
		return finish("status", fmt.Errorf("--watch needs a terminal; redirect-friendly alternative: watch -n %g gccfactory status",
			interval.Seconds()))
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sig)

	out := bufio.NewWriter(os.Stdout)
	// Restored on every exit path, including ctrl-c: a hidden cursor left
	// behind makes the shell look hung.
	out.WriteString("\x1b[?25l")
	out.Flush()
	defer func() { os.Stdout.WriteString("\x1b[?25h") }()

	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		var buf bytes.Buffer
		if err := frame(&buf); err != nil {
			return err
		}
		paintFrame(out, buf.String())
		select {
		case <-sig:
			out.WriteString("\r\n")
			out.Flush()
			return nil
		case <-tick.C:
		}
	}
}

// A full-screen clear between frames is what makes a watch flicker, so lines
// are cleared individually. Oversized frames are cut rather than allowed to
// scroll, which would desync the next frame's cursor-home.
func paintFrame(out *bufio.Writer, frame string) {
	_, rows := termSize()
	lines := strings.Split(strings.TrimRight(frame, "\n"), "\n")
	budget := rows - 1
	cut := false
	if budget > 0 && len(lines) > budget {
		lines, cut = lines[:budget-1], true
	}
	out.WriteString("\x1b[H")
	for _, ln := range lines {
		out.WriteString(ln)
		out.WriteString("\x1b[K\r\n")
	}
	if cut {
		out.WriteString(dim("… cut to fit the terminal; --host/--target narrows the survey"))
		out.WriteString("\x1b[K\r\n")
	}
	out.WriteString("\x1b[J")
	out.Flush()
}

func colorState(s string) string {
	switch s {
	case "ok":
		return green("ok")
	case "stale":
		return yellow("stale")
	case "building":
		return blue("building")
	}
	return dim("missing")
}

var gridGlyph = map[string]string{
	"ok":       green("#"),
	"stale":    yellow("~"),
	"building": blue("*"),
	"missing":  dim("."),
}

// printGrid draws the canadian matrix as host-rows x target-columns, which
// stays readable at 11x11 where 121 table rows would not.
func printGrid(w io.Writer, hosts, targets []triple.Triple, state func(slug string) string) {
	if len(hosts) == 0 || len(targets) == 0 {
		return
	}
	fmt.Fprintln(w, bold("CANADIAN MATRIX")+dim("  rows = host (runs on), columns = target (emits for)"))
	var hdr strings.Builder
	hdr.WriteString(strings.Repeat(" ", 25))
	for i := range targets {
		fmt.Fprintf(&hdr, "%-3d", i+1)
	}
	fmt.Fprintln(w, dim(hdr.String()))
	for _, h := range hosts {
		fmt.Fprintf(w, "  %-23s", h.Raw)
		for _, t := range targets {
			fmt.Fprintf(w, "%s  ", gridGlyph[state(recipe.Canadian(h, t).Slug())])
		}
		fmt.Fprintln(w)
	}
	fmt.Fprintln(w)
	for i, t := range targets {
		fmt.Fprintf(w, "%s", dim(fmt.Sprintf("  %2d %-24s", i+1, t.Raw)))
		if i%2 == 1 || i == len(targets)-1 {
			fmt.Fprintln(w)
		}
	}
	fmt.Fprintf(w, "%s\n", dim("  legend: # ok   ~ stale   * building   . missing"))
}
