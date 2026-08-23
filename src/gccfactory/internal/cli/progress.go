package cli

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/junikimm717/gccfactory/src/gccfactory/internal/core"
)

const (
	stQueued   = "queued"
	stBuilding = "building"
	stOK       = "ok"
	stCached   = "cached"
	stFailed   = "failed"
)

type jobState struct {
	slug    string
	key     string
	state   string
	step    string
	since   time.Time
	elapsed time.Duration
	who     string
}

// progress is a poller, not a subscriber: it re-runs core.Plan (cheap: manifest
// stat + parse) and reads dist/state/heartbeats/ every tick. That deliberately
// makes the display correct for jobs being built by a *different* gccfactory
// process sharing the same dist/, which an in-process event stream could not
// show.
type progress struct {
	e     *core.Env
	roots []core.Job
	tty   bool
	out   *os.File

	mu      sync.Mutex
	order   []string
	jobs    map[string]*jobState
	start   time.Time
	drawn   int
	lastOut map[string]string // non-tty: last line printed per slug
	done    chan struct{}
	wg      sync.WaitGroup
}

func newProgress(e *core.Env, roots []core.Job, plain bool) *progress {
	return &progress{
		e: e, roots: roots,
		tty:     !plain && isTTY(os.Stderr),
		out:     os.Stderr,
		jobs:    map[string]*jobState{},
		lastOut: map[string]string{},
		start:   time.Now(),
		done:    make(chan struct{}),
	}
}

func (p *progress) Start() {
	p.refresh(true)
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		t := time.NewTicker(500 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-p.done:
				return
			case <-t.C:
				p.refresh(false)
			}
		}
	}()
}

func (p *progress) Stop() {
	close(p.done)
	p.wg.Wait()
	p.refresh(false)
	p.mu.Lock()
	defer p.mu.Unlock()
	ok, cached, fail, pending := 0, 0, 0, 0
	for _, s := range p.jobs {
		switch s.state {
		case stOK:
			ok++
		case stCached:
			cached++
		case stFailed:
			fail++
		default:
			pending++
		}
	}
	fmt.Fprintf(p.out, "\n%s %d built, %d already current, %d not reached  (%s)\n",
		bold("summary:"), ok, cached, pending+fail, humanDur(time.Since(p.start)))
}

// initial marks pre-existing artifacts as "cached" rather than "ok" so the
// user can tell what this run actually did.
func (p *progress) refresh(initial bool) {
	nodes, err := core.Plan(p.e, p.roots)
	if err != nil {
		return
	}

	p.mu.Lock()
	for _, n := range nodes {
		slug := n.Job.Slug()
		s, ok := p.jobs[slug]
		if !ok {
			s = &jobState{slug: slug, key: n.Key, state: stQueued}
			p.jobs[slug] = s
			p.order = append(p.order, slug)
			if initial && n.Valid {
				s.state = stCached
			}
		}
		s.key = n.Key
		switch {
		case n.Valid && s.state != stCached:
			s.state = stOK
			s.step = ""
			if !s.since.IsZero() && s.elapsed == 0 {
				s.elapsed = time.Since(s.since)
			}
		case !n.Valid:
			if h := liveHeartbeat(p.e, slug); h != nil {
				s.state = stBuilding
				s.step = h.Step
				s.who = who(h)
				if !h.StartedAt.IsZero() {
					s.since = h.StartedAt
				} else if s.since.IsZero() {
					s.since = time.Now()
				}
			} else if s.state == stBuilding {
				// heartbeat vanished without the artifact appearing
				s.state = stFailed
			}
		}
	}
	lines := p.renderLocked()
	p.mu.Unlock()

	p.emit(lines)
}

func (p *progress) renderLocked() []string {
	out := make([]string, 0, len(p.order))
	for _, slug := range p.order {
		s := p.jobs[slug]
		var state, extra string
		switch s.state {
		case stOK:
			state = green("ok")
		case stCached:
			state = dim("cached")
		case stFailed:
			state = red("failed")
		case stBuilding:
			state = yellow("building")
			extra = s.step
			if s.who != "" {
				extra += dim(" [" + s.who + "]")
			}
		default:
			state = dim("queued")
		}
		el := ""
		if s.state == stBuilding && !s.since.IsZero() {
			el = humanDur(time.Since(s.since))
		} else if s.elapsed > 0 {
			el = humanDur(s.elapsed)
		}
		out = append(out, fmt.Sprintf("  %-10s %6s  %-46s %s", state, dim(el), slug, extra))
	}
	return out
}

func (p *progress) emit(lines []string) {
	if !p.tty {
		p.mu.Lock()
		defer p.mu.Unlock()
		for i, slug := range p.order {
			l := strings.TrimSpace(ansiRE.ReplaceAllString(lines[i], ""))
			if p.lastOut[slug] != l {
				p.lastOut[slug] = l
				fmt.Fprintln(p.out, l)
			}
		}
		return
	}

	_, rows := termSize()
	if max := rows - 3; max > 0 && len(lines) > max {
		lines = condense(lines, max)
	}
	var b strings.Builder
	if p.drawn > 0 {
		fmt.Fprintf(&b, "\x1b[%dA", p.drawn)
	}
	for _, l := range lines {
		b.WriteString("\x1b[2K" + l + "\n")
	}
	p.drawn = len(lines)
	fmt.Fprint(p.out, b.String())
}

// condense keeps the interesting rows (anything not finished) when the job list
// is taller than the terminal.
func condense(lines []string, max int) []string {
	var keep []string
	hidden := 0
	for _, l := range lines {
		if strings.Contains(l, "cached") || strings.Contains(l, "ok ") {
			hidden++
			continue
		}
		keep = append(keep, l)
	}
	if len(keep) > max-1 {
		hidden += len(keep) - (max - 1)
		keep = keep[len(keep)-(max-1):]
	}
	return append(keep, dim(fmt.Sprintf("  ... %d finished/hidden", hidden)))
}

func runWithProgress(ctx context.Context, e *core.Env, roots []core.Job, plain bool) error {
	p := newProgress(e, roots, plain)
	p.Start()
	err := core.Run(ctx, e, roots)
	p.Stop()
	return err
}
