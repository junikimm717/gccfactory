package cli

import (
	"fmt"
	"os"
	"strings"

	"github.com/junikimm717/gccfactory/src/gccfactory/internal/core"
	"github.com/junikimm717/gccfactory/src/gccfactory/internal/recipe"
	"github.com/junikimm717/gccfactory/src/gccfactory/internal/triple"
)

var cmdStatus = &command{
	Name:     "status",
	Short:    "what exists, what's stale, what's building right now",
	Synopsis: "gccfactory status [--host LIST] [--target LIST] [--all]",
	Long: `Surveys dist/ and prints, for every job in the matrix, whether its artifact is
current (its content key matches), stale (an artifact exists but its inputs
changed), being built right now (by this or any other process sharing dist/),
or missing.

By default the whole ` + "`all` x `all`" + ` matrix is surveyed but only interesting rows
are listed, plus a compact grid of the 11x11 canadian matrix. Use --all to list
every job as a row, or --host/--target to narrow the survey.

Safe to run at any time, including while a build is in progress and before
anything has ever been built.

  STATES   ` + "`ok`" + ` current   ` + "`stale`" + ` inputs changed   ` + "`building`" + ` live heartbeat   ` + "`missing`",
	Run: runStatus,
}

func runStatus(g *Global, args []string) error {
	fs := g.flagSet("status")
	host := fs.String("host", "all", tripleFlagHelp)
	target := fs.String("target", "all", tripleFlagHelp)
	all := fs.Bool("all", false, "list every job as a row instead of summarizing")
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

	e, done, err := g.env(defaultJobs, defaultWorkers)
	if err != nil {
		return err
	}
	defer done()

	roots := rootJobs(hosts, targets)
	nodes, err := core.Plan(e, roots)
	if err != nil {
		return err
	}
	hb := readHeartbeats(e)

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
		if _, ok := hb[n.Job.Slug()]; ok {
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

	fmt.Printf("%s %s\n", bold("dist:"), e.Dist)
	fmt.Printf("  %s %d   %s %d   %s %d   %s %d   (of %d jobs)\n\n",
		green("ok"), counts.ok, yellow("stale"), counts.stale,
		blue("building"), counts.building, dim("missing"), counts.missing, len(nodes))

	t := newTable("STATE", "SLUG", "KEY", "SIZE", "BUILT", "BY")
	shown := 0
	for _, n := range nodes {
		r := byslug[n.Job.Slug()]
		if !*all && r.state == "missing" {
			continue
		}
		if !*all && n.Job.Name() == "canadian" {
			continue // covered by the grid below
		}
		shown++
		size, built, by := "-", "-", "-"
		if r.state == "ok" || r.state == "stale" {
			size = humanBytes(dirSize(n.Job.ArtifactDir(e)))
			if m, ok := artifactManifest(n.Job.ArtifactDir(e)); ok {
				built = humanAgo(m.CompletedAt)
				by = m.BuiltBy
			}
		}
		if h, ok := hb[n.Job.Slug()]; ok {
			by = who(h)
			built = h.Step
			if built == "" {
				built = "…"
			}
		}
		t.add(colorState(r.state), n.Job.Slug(), dim(shortKey(n.Key)), size, built, dim(by))
	}
	if shown > 0 {
		t.rightAlign(3).render(os.Stdout)
		fmt.Println()
	}

	if !*all {
		printGrid(e, hosts, targets, func(slug string) string {
			if r, ok := byslug[slug]; ok {
				return r.state
			}
			return "missing"
		})
	}

	if counts.ok == 0 && counts.building == 0 {
		fmt.Printf("\nNothing built yet. Try %s\n", cyan("gccf build --host proven --target proven"))
	}
	return nil
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
func printGrid(e *core.Env, hosts, targets []triple.Triple, state func(slug string) string) {
	if len(hosts) == 0 || len(targets) == 0 {
		return
	}
	fmt.Println(bold("CANADIAN MATRIX") + dim("  rows = host (runs on), columns = target (emits for)"))
	var hdr strings.Builder
	hdr.WriteString(strings.Repeat(" ", 25))
	for i := range targets {
		fmt.Fprintf(&hdr, "%-3d", i+1)
	}
	fmt.Println(dim(hdr.String()))
	for _, h := range hosts {
		fmt.Printf("  %-23s", h.Raw)
		for _, t := range targets {
			fmt.Printf("%s  ", gridGlyph[state(recipe.Canadian(h, t).Slug())])
		}
		fmt.Println()
	}
	fmt.Println()
	for i, t := range targets {
		fmt.Printf("%s", dim(fmt.Sprintf("  %2d %-24s", i+1, t.Raw)))
		if i%2 == 1 || i == len(targets)-1 {
			fmt.Println()
		}
	}
	fmt.Printf("%s\n", dim("  legend: # ok   ~ stale   * building   . missing"))
}
