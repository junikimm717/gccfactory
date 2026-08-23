package cli

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

var cmdClean = &command{
	Name:     "clean",
	Short:    "remove scratch dirs, abandoned builds, logs or artifacts",
	Synopsis: "gccfactory clean [--stale] [--work] [--logs] [--all] [--sources] [--dry-run] [--yes]",
	Long: `With no flags this does the safe thing: --stale.

  --stale     (default) remove build/staging dirs whose owning process is dead
              and that have not been touched for 10 minutes, plus everything
              waiting in dist/.trash/. Safe to run during a build.
  --work      remove ALL of dist/work/ and dist/.staging/, except directories
              belonging to a live process. Frees the most space; costs you the
              partial progress of any interrupted build.
  --logs      remove dist/logs/. Artifacts are untouched.
  --all       remove work, staging, trash, logs, state AND every built
              toolchain. Downloaded tarballs in dist/src/ are kept, because
              they are checksum-verified and expensive to re-fetch.
  --sources   also remove dist/src/ (the verified tarball cache).
  --dry-run   list what would be removed and how much it would free.
  --yes       skip the confirmation prompt for destructive modes.

Lock files under dist/locks/ are never removed: deleting them would break flock
identity for a concurrently running gccfactory.`,
	Run: runClean,
}

func runClean(g *Global, args []string) error {
	fs := g.flagSet("clean")
	stale := fs.Bool("stale", false, "remove abandoned scratch dirs and trash (default)")
	work := fs.Bool("work", false, "remove all build/staging dirs not owned by a live process")
	logsFlag := fs.Bool("logs", false, "remove dist/logs")
	all := fs.Bool("all", false, "remove scratch, logs, state and built toolchains")
	srcs := fs.Bool("sources", false, "also remove the downloaded tarball cache")
	dryRun := fs.Bool("dry-run", false, "list what would be removed")
	yes := fs.Bool("yes", false, "skip the confirmation prompt")
	if err := parse(fs, args); err != nil {
		return finish("clean", err)
	}
	if err := g.resolve(); err != nil {
		return err
	}
	if !*stale && !*work && !*logsFlag && !*all && !*srcs {
		*stale = true
	}

	var victims []string
	add := func(paths ...string) {
		for _, p := range paths {
			if _, err := os.Lstat(p); err == nil {
				victims = append(victims, p)
			}
		}
	}
	scratch := []string{filepath.Join(g.Dist, "work"), filepath.Join(g.Dist, ".staging")}

	if *stale || *all {
		for _, root := range scratch {
			for _, d := range abandoned(root, 10*time.Minute) {
				add(d)
			}
		}
		add(children(filepath.Join(g.Dist, ".trash"))...)
	}
	if *work || *all {
		for _, root := range scratch {
			for _, d := range children(root) {
				if pid, ok := ownerPID(d); ok && pidAlive(pid) {
					fmt.Fprintf(os.Stderr, "%s keeping %s (pid %d is alive)\n", dim("skip:"), filepath.Base(d), pid)
					continue
				}
				add(d)
			}
		}
	}
	if *logsFlag || *all {
		add(filepath.Join(g.Dist, "logs"))
	}
	if *all {
		add(filepath.Join(g.Dist, "toolchains"), filepath.Join(g.Dist, "state"))
	}
	if *srcs {
		add(filepath.Join(g.Dist, "src"))
	}

	victims = dedupe(victims)
	if len(victims) == 0 {
		fmt.Println("nothing to clean")
		return nil
	}

	var total int64
	t := newTable("SIZE", "PATH")
	for _, v := range victims {
		n := dirSize(v)
		total += n
		t.add(humanBytes(n), rel(g.Dist, v))
	}
	t.rightAlign(0).render(os.Stdout)
	fmt.Printf("\n%s would free %s\n", bold(fmt.Sprintf("%d path%s,", len(victims), plural(len(victims)))), humanBytes(total))

	if *dryRun {
		return nil
	}
	if (*all || *srcs) && !*yes {
		if !confirm(fmt.Sprintf("Delete these %d paths?", len(victims))) {
			fmt.Println("aborted")
			return nil
		}
	}
	for _, v := range victims {
		if err := os.RemoveAll(v); err != nil {
			return fmt.Errorf("remove %s: %w", v, err)
		}
	}
	fmt.Printf("%s freed %s\n", green("cleaned:"), humanBytes(total))
	return nil
}

func children(dir string) []string {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(ents))
	for _, ent := range ents {
		out = append(out, filepath.Join(dir, ent.Name()))
	}
	return out
}

func abandoned(root string, age time.Duration) []string {
	var out []string
	for _, d := range children(root) {
		fi, err := os.Stat(d)
		if err != nil || time.Since(fi.ModTime()) < age {
			continue
		}
		if pid, ok := ownerPID(d); ok && pidAlive(pid) {
			continue
		}
		out = append(out, d)
	}
	return out
}

func ownerPID(path string) (int, bool) {
	parts := strings.Split(filepath.Base(path), ".")
	if len(parts) < 3 {
		return 0, false
	}
	pid, err := strconv.Atoi(parts[len(parts)-2])
	return pid, err == nil
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	out := in[:0]
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

func confirm(prompt string) bool {
	if !isTTY(os.Stdin) {
		fmt.Fprintln(os.Stderr, "refusing to delete without a terminal; pass --yes")
		return false
	}
	fmt.Printf("%s [y/N] ", prompt)
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	line = strings.ToLower(strings.TrimSpace(line))
	return line == "y" || line == "yes"
}
