package cli

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
)

var cmdShell = &command{
	Name:     "shell",
	Short:    "drop into a shell with a job's environment and build directory",
	Synopsis: "gccfactory shell <job-slug> [--print] [--step NAME]",
	Long: `Reconstructs the exact environment a job's commands ran under - the cwd and
every variable the recipe overlaid (PATH, CC, CFLAGS, DESTDIR, ...) - and execs
your $SHELL there. This is the fastest way to re-run a failing configure or make
by hand: the log's ` + "`# cmd:`" + ` line is a copy-pasteable argv.

The environment is read from the job's most recent attempt directory under
dist/logs/jobs/<job-slug>/ (its commands.sh, or the header of a step log), so it
works even after the process that ran the job is gone.

The build tree itself lives in dist/work/<job-slug>.<pid>.<rand>/ and is deleted
when a job succeeds. If it is gone, rebuild with ` + "`gccfactory build --keep-work`" + `
to keep it.

FLAGS
  --print       print the environment as shell exports instead of exec'ing
  --step NAME   use the environment of this step rather than the last one`,
	Run: runShell,
}

func runShell(g *Global, args []string) error {
	fs := g.flagSet("shell")
	printOnly := fs.Bool("print", false, "print exports instead of exec'ing a shell")
	step := fs.String("step", "", "take the environment from this step")
	if err := parse(fs, args); err != nil {
		return finish("shell", err)
	}
	if err := g.resolve(); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		return usagef("need a job slug.%s", knownSlugsHint(g.Dist))
	}
	slug := fs.Arg(0)

	base := filepath.Join(g.Dist, "logs", "jobs", slug)
	dir, err := latestAttempt(base)
	if err != nil {
		return fmt.Errorf("no recorded environment for job %q: %w", slug, err)
	}
	cwd, env, src, err := jobEnv(dir, *step)
	if err != nil {
		return err
	}

	work, workNote := resolveWorkDir(g.Dist, slug, cwd)

	if *printOnly {
		fmt.Printf("# from %s\ncd %s\n", src, shQuote(work))
		for _, k := range sortedKeys(env) {
			fmt.Printf("export %s=%s\n", k, shQuote(env[k]))
		}
		return nil
	}

	sh := os.Getenv("SHELL")
	if sh == "" {
		sh = "/bin/bash"
	}
	fmt.Fprintf(os.Stderr, "%s %s\n", bold("gccfactory shell:"), slug)
	fmt.Fprintf(os.Stderr, "  env from  %s  (%d variable%s overlaid)\n", src, len(env), plural(len(env)))
	fmt.Fprintf(os.Stderr, "  cwd       %s\n", work)
	if workNote != "" {
		fmt.Fprintf(os.Stderr, "  %s %s\n", yellow("note:"), workNote)
	}
	fmt.Fprintf(os.Stderr, "  %s\n", dim("`gccf logs "+slug+" --failed` shows the command that failed; exit to return"))
	fmt.Fprintln(os.Stderr)

	full := mergeEnv(nil, env)
	full = append(full, "GCCFACTORY_JOB="+slug)
	if err := os.Chdir(work); err != nil {
		return fmt.Errorf("cd %s: %w", work, err)
	}
	bin, err := exec.LookPath(sh)
	if err != nil {
		return fmt.Errorf("cannot find $SHELL (%s): %w", sh, err)
	}
	return syscall.Exec(bin, []string{filepath.Base(bin), "-i"}, full)
}

func jobEnv(dir, step string) (cwd string, env map[string]string, src string, err error) {
	if c := filepath.Join(dir, "commands.sh"); fileExists(c) {
		cwd, env = parseRecordedEnv(c)
		if cwd != "" || len(env) > 0 {
			return cwd, env, c, nil
		}
	}
	logs, err := listLogs(dir)
	if err != nil || len(logs) == 0 {
		return "", nil, "", fmt.Errorf("no commands.sh or step logs under %s", dir)
	}
	pick := logs[len(logs)-1]
	if step != "" {
		found := false
		for _, l := range logs {
			if strings.Contains(strings.ToLower(stepName(l)), strings.ToLower(step)) {
				pick, found = l, true
			}
		}
		if !found {
			return "", nil, "", fmt.Errorf("no step matching %q; steps are:\n  %s", step, strings.Join(stepNames(logs), "\n  "))
		}
	}
	cwd, env = parseRecordedEnv(pick)
	return cwd, env, pick, nil
}

// parseRecordedEnv understands the log header written by core.Runner
//
//	# cwd: /path
//	# env: KEY=VAL
//
// and the equivalent shell form used by commands.sh (`cd /path`, `export K=V`).
func parseRecordedEnv(path string) (string, map[string]string) {
	env := map[string]string{}
	cwd := ""
	f, err := os.Open(path)
	if err != nil {
		return "", env
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for n := 0; sc.Scan() && n < 4000; n++ {
		line := strings.TrimSpace(sc.Text())
		switch {
		case strings.HasPrefix(line, "# cwd:"):
			cwd = strings.TrimSpace(strings.TrimPrefix(line, "# cwd:"))
		case strings.HasPrefix(line, "# env:"):
			if k, v, ok := strings.Cut(strings.TrimSpace(strings.TrimPrefix(line, "# env:")), "="); ok {
				env[k] = unquote(v)
			}
		case strings.HasPrefix(line, "cd ") && cwd == "":
			cwd = unquote(strings.TrimSpace(strings.TrimPrefix(line, "cd ")))
		case strings.HasPrefix(line, "export "):
			if k, v, ok := strings.Cut(strings.TrimPrefix(line, "export "), "="); ok {
				env[strings.TrimSpace(k)] = unquote(v)
			}
		case strings.HasPrefix(line, "# cmd:"):
			// header is over once the argv shows up
			if cwd != "" || len(env) > 0 {
				return cwd, env
			}
		}
	}
	return cwd, env
}

func resolveWorkDir(dist, slug, recorded string) (string, string) {
	if recorded != "" && dirExists(recorded) {
		return recorded, ""
	}
	matches, _ := filepath.Glob(filepath.Join(dist, "work", slug+".*"))
	sort.Strings(matches)
	for i := len(matches) - 1; i >= 0; i-- {
		if dirExists(matches[i]) {
			note := ""
			if recorded != "" {
				note = "recorded cwd " + recorded + " is gone; using the surviving build tree"
			}
			return matches[i], note
		}
	}
	note := "the build tree is gone (jobs delete it on success). " +
		"Re-run with `gccf build --keep-work` to keep it; landing in dist/ instead."
	return dist, note
}

func fileExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && !fi.IsDir()
}

func dirExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

func unquote(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 && (s[0] == '\'' || s[0] == '"') && s[len(s)-1] == s[0] {
		return s[1 : len(s)-1]
	}
	return s
}

func shQuote(s string) string {
	if s != "" && !strings.ContainsAny(s, " \t\n'\"$`\\*?[]{}()|&;<>#~") {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
