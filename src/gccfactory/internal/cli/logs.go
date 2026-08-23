package cli

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

var cmdLogs = &command{
	Name:     "logs",
	Short:    "read a job's command-by-command transcript",
	Synopsis: "gccfactory logs <job-slug> [--step NAME] [--failed] [--follow] [-n LINES] [--attempts]",
	Long: `Every command a job runs is recorded under
dist/logs/jobs/<job-slug>/<attempt>/<NNN>-<step>.log, each log beginning with a
header giving the cwd, the overlaid environment and the exact argv - enough to
re-run the step by hand.

With no flags this prints the index of steps in the most recent attempt plus the
tail of the last one, which is almost always where the failure is.

FLAGS
  --step NAME    print, in full, every step whose name contains NAME
  --failed       print the tail of the step the job died on
  --follow       tail the newest log as it is written, rolling onto the next
                 step's log automatically (works while another process builds)
  -n LINES       how many lines of tail to show (default 40; 0 means all)
  --attempts     list every recorded attempt for this job instead

Run ` + "`gccfactory logs`" + ` with no arguments to see which jobs have logs.
Job slugs also appear in ` + "`gccfactory status`" + ` and in build output.`,
	Run: runLogs,
}

func runLogs(g *Global, args []string) error {
	fs := g.flagSet("logs")
	step := fs.String("step", "", "print steps whose name contains this")
	failed := fs.Bool("failed", false, "print the tail of the step the job died on")
	follow := fs.Bool("follow", false, "tail the newest log as it is written")
	attempts := fs.Bool("attempts", false, "list recorded attempts instead")
	n := fs.Int("n", 40, "tail length in lines (0 = whole file)")
	if err := parse(fs, args); err != nil {
		return finish("logs", err)
	}
	if err := g.resolve(); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		return usagef("need a job slug.%s", knownSlugsHint(g.Dist))
	}
	slug := fs.Arg(0)
	base := filepath.Join(g.Dist, "logs", "jobs", slug)
	if _, err := os.Stat(base); err != nil {
		return fmt.Errorf("no logs for job %q under %s.%s", slug, base, knownSlugsHint(g.Dist))
	}

	if *attempts {
		return listAttempts(base)
	}
	dir, err := latestAttempt(base)
	if err != nil {
		return err
	}
	logs, err := listLogs(dir)
	if err != nil {
		return err
	}
	if len(logs) == 0 {
		return fmt.Errorf("no step logs yet under %s", dir)
	}

	switch {
	case *follow:
		return followLogs(dir)
	case *step != "":
		matched := 0
		for _, l := range logs {
			if strings.Contains(strings.ToLower(stepName(l)), strings.ToLower(*step)) {
				matched++
				printLog(l, 0)
			}
		}
		if matched == 0 {
			return fmt.Errorf("no step matching %q; steps are:\n  %s", *step, strings.Join(stepNames(logs), "\n  "))
		}
		return nil
	case *failed:
		printLog(logs[len(logs)-1], *n)
		return nil
	default:
		fmt.Printf("%s %s\n%s\n\n", bold("job:"), slug, dim(dir))
		t := newTable("STEP", "SIZE", "WHEN")
		for _, l := range logs {
			fi, err := os.Stat(l)
			if err != nil {
				continue
			}
			t.add(filepath.Base(l), humanBytes(fi.Size()), humanAgo(fi.ModTime()))
		}
		t.rightAlign(1).render(os.Stdout)
		fmt.Println()
		printLog(logs[len(logs)-1], *n)
		fmt.Printf("\n%s %s\n", dim("full step:"), cyan(fmt.Sprintf("gccf logs %s --step %s", slug, stepName(logs[len(logs)-1]))))
		return nil
	}
}

func latestAttempt(base string) (string, error) {
	if fi, err := os.Stat(filepath.Join(base, "latest")); err == nil && fi.IsDir() {
		return filepath.Join(base, "latest"), nil
	}
	ents, err := os.ReadDir(base)
	if err != nil {
		return "", err
	}
	var dirs []string
	flat := false
	for _, ent := range ents {
		switch {
		case ent.IsDir() && ent.Name() != "latest":
			dirs = append(dirs, ent.Name())
		case strings.HasSuffix(ent.Name(), ".log"):
			flat = true
		}
	}
	if len(dirs) > 0 {
		sort.Strings(dirs)
		return filepath.Join(base, dirs[len(dirs)-1]), nil
	}
	if flat {
		return base, nil
	}
	return "", fmt.Errorf("no logs recorded under %s yet", base)
}

func listAttempts(base string) error {
	ents, err := os.ReadDir(base)
	if err != nil {
		return err
	}
	t := newTable("ATTEMPT", "STEPS", "WHEN")
	found := false
	for _, ent := range ents {
		if !ent.IsDir() || ent.Name() == "latest" {
			continue
		}
		found = true
		logs, _ := listLogs(filepath.Join(base, ent.Name()))
		when := "-"
		if info, err := ent.Info(); err == nil {
			when = humanAgo(info.ModTime())
		}
		t.add(ent.Name(), fmt.Sprint(len(logs)), when)
	}
	if !found {
		return fmt.Errorf("this job's logs are stored flat (no per-attempt directories) under %s", base)
	}
	t.render(os.Stdout)
	return nil
}

func listLogs(dir string) ([]string, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, ent := range ents {
		if !ent.IsDir() && strings.HasSuffix(ent.Name(), ".log") {
			out = append(out, filepath.Join(dir, ent.Name()))
		}
	}
	sort.Strings(out) // NNN- prefix makes lexical order chronological
	return out, nil
}

func stepName(path string) string {
	b := strings.TrimSuffix(filepath.Base(path), ".log")
	if i := strings.IndexByte(b, '-'); i > 0 && isDigits(b[:i]) {
		return b[i+1:]
	}
	return b
}

func isDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}

func stepNames(logs []string) []string {
	out := make([]string, len(logs))
	for i, l := range logs {
		out[i] = stepName(l)
	}
	return out
}

func printLog(path string, n int) {
	fmt.Printf("%s %s\n", bold("---"), cyan(path))
	f, err := os.Open(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "  %v\n", err)
		return
	}
	defer f.Close()
	if n <= 0 {
		_, _ = io.Copy(os.Stdout, f)
		return
	}
	ring := make([]string, 0, n)
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	total := 0
	for sc.Scan() {
		total++
		if len(ring) == n {
			ring = ring[1:]
		}
		ring = append(ring, sc.Text())
	}
	if total > n {
		fmt.Println(dim(fmt.Sprintf("... %d earlier lines omitted (-n 0 for all)", total-n)))
	}
	for _, l := range ring {
		fmt.Println(l)
	}
}

func followLogs(dir string) error {
	var (
		cur string
		f   *os.File
		rd  *bufio.Reader
	)
	defer func() {
		if f != nil {
			f.Close()
		}
	}()
	for {
		logs, _ := listLogs(dir)
		if len(logs) > 0 && logs[len(logs)-1] != cur {
			if f != nil {
				_, _ = io.Copy(os.Stdout, f)
				f.Close()
			}
			cur = logs[len(logs)-1]
			nf, err := os.Open(cur)
			if err != nil {
				return err
			}
			f, rd = nf, bufio.NewReader(nf)
			fmt.Printf("\n%s %s\n", bold("==>"), cyan(cur))
		}
		if rd == nil {
			time.Sleep(300 * time.Millisecond)
			continue
		}
		nbytes, err := io.Copy(os.Stdout, rd)
		if err != nil {
			return err
		}
		if nbytes == 0 {
			time.Sleep(300 * time.Millisecond)
		}
	}
}

func knownSlugsHint(dist string) string {
	ents, err := os.ReadDir(filepath.Join(dist, "logs", "jobs"))
	if err != nil || len(ents) == 0 {
		return "\nNo jobs have logged anything yet under " + filepath.Join(dist, "logs", "jobs") + "."
	}
	var slugs []string
	for _, ent := range ents {
		if ent.IsDir() {
			slugs = append(slugs, ent.Name())
		}
	}
	sort.Strings(slugs)
	if len(slugs) > 20 {
		slugs = append(slugs[:20], fmt.Sprintf("... and %d more", len(slugs)-20))
	}
	return "\n\nJobs with logs:\n  " + strings.Join(slugs, "\n  ")
}
