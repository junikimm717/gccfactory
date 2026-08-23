package ensure

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

type Check struct {
	Name    string        `json:"name"`
	OK      bool          `json:"ok"`
	Skipped bool          `json:"skipped,omitempty"`
	Detail  string        `json:"detail,omitempty"`
	Err     error         `json:"-"`
	LogPath string        `json:"log_path,omitempty"`
	Dur     time.Duration `json:"-"`
}

type Report struct {
	Subject string        `json:"subject"`
	Checks  []Check       `json:"checks"`
	Dur     time.Duration `json:"-"`
}

func NewReport(subject string) *Report { return &Report{Subject: subject} }

func (r *Report) Add(c Check) {
	if c.Err != nil && c.LogPath == "" {
		c.LogPath = logPathOf(c.Err)
	}
	r.Checks = append(r.Checks, c)
}

func (r *Report) Pass(name, format string, a ...any) {
	r.Add(Check{Name: name, OK: true, Detail: fmt.Sprintf(format, a...)})
}

func (r *Report) Fail(name string, err error, format string, a ...any) {
	r.Add(Check{Name: name, OK: false, Err: err, Detail: fmt.Sprintf(format, a...)})
}

func (r *Report) Failf(name, format string, a ...any) {
	msg := fmt.Sprintf(format, a...)
	r.Add(Check{Name: name, OK: false, Err: fmt.Errorf("%s", msg)})
}

// Skips do not fail a report but are printed distinctly so nobody mistakes
// them for a pass.
func (r *Report) Skip(name, format string, a ...any) {
	r.Add(Check{Name: name, OK: true, Skipped: true, Detail: fmt.Sprintf(format, a...)})
}

func (r *Report) Absorb(prefix string, sub *Report) {
	if sub == nil {
		return
	}
	for _, c := range sub.Checks {
		if prefix != "" {
			c.Name = prefix + c.Name
		}
		r.Checks = append(r.Checks, c)
	}
}

func (r *Report) OK() bool {
	for _, c := range r.Checks {
		if !c.OK {
			return false
		}
	}
	return true
}

func (r *Report) Counts() (pass, fail, skip int) {
	for _, c := range r.Checks {
		switch {
		case c.Skipped:
			skip++
		case c.OK:
			pass++
		default:
			fail++
		}
	}
	return
}

func (r *Report) Failures() []Check {
	var out []Check
	for _, c := range r.Checks {
		if !c.OK {
			out = append(out, c)
		}
	}
	return out
}

func (r *Report) String() string {
	var b strings.Builder
	pass, fail, skip := r.Counts()
	b.WriteString(r.Subject)
	fmt.Fprintf(&b, ": %d passed, %d failed", pass, fail)
	if skip > 0 {
		fmt.Fprintf(&b, ", %d skipped", skip)
	}
	if r.Dur > 0 {
		fmt.Fprintf(&b, " (%s)", r.Dur.Round(time.Millisecond))
	}
	b.WriteByte('\n')

	width := 0
	for _, c := range r.Checks {
		if n := utf8.RuneCountInString(c.Name); n > width && n <= 44 {
			width = n
		}
	}
	for _, c := range r.Checks {
		mark := "✗"
		switch {
		case c.Skipped:
			mark = "-"
		case c.OK:
			mark = "✓"
		}
		fmt.Fprintf(&b, "  %s %s", mark, pad(c.Name, width))
		if c.OK && c.Detail != "" {
			b.WriteString("  " + firstLine(c.Detail))
		}
		if c.OK && c.Dur >= 100*time.Millisecond {
			fmt.Fprintf(&b, "  (%s)", c.Dur.Round(time.Millisecond))
		}
		b.WriteByte('\n')
		if c.OK {
			continue
		}
		for _, line := range detailLines(c) {
			b.WriteString("      " + line + "\n")
		}
	}
	return b.String()
}

func detailLines(c Check) []string {
	var out []string
	add := func(s string) {
		for _, l := range strings.Split(strings.TrimRight(s, "\n"), "\n") {
			out = append(out, l)
		}
	}
	if c.Detail != "" {
		add(c.Detail)
	}
	if c.Err != nil {
		add(c.Err.Error())
	}
	if c.LogPath != "" && !strings.Contains(strings.Join(out, "\n"), c.LogPath) {
		out = append(out, "log: "+c.LogPath)
	}
	return out
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i] + " ..."
	}
	return s
}

func pad(s string, w int) string {
	if n := utf8.RuneCountInString(s); n < w {
		return s + strings.Repeat(" ", w-n)
	}
	return s
}

type reportError struct{ r *Report }

func (e *reportError) Error() string {
	pass, fail, _ := e.r.Counts()
	var b strings.Builder
	fmt.Fprintf(&b, "%s: %d of %d checks failed\n", e.r.Subject, fail, pass+fail)
	for _, c := range e.r.Failures() {
		b.WriteString("  ✗ " + c.Name + "\n")
		for _, line := range detailLines(c) {
			b.WriteString("      " + line + "\n")
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// Err returns nil if the report passed, else an error listing every failure
// with its evidence and log path.
func (r *Report) Err() error {
	if r.OK() {
		return nil
	}
	return &reportError{r}
}

type jsonCheck struct {
	Check
	Error string `json:"error,omitempty"`
	Ms    int64  `json:"ms,omitempty"`
}

func (r *Report) JSON() ([]byte, error) {
	pass, fail, skip := r.Counts()
	out := struct {
		Subject string      `json:"subject"`
		OK      bool        `json:"ok"`
		Passed  int         `json:"passed"`
		Failed  int         `json:"failed"`
		Skipped int         `json:"skipped"`
		Ms      int64       `json:"ms,omitempty"`
		Checks  []jsonCheck `json:"checks"`
	}{Subject: r.Subject, OK: r.OK(), Passed: pass, Failed: fail, Skipped: skip, Ms: r.Dur.Milliseconds()}
	for _, c := range r.Checks {
		jc := jsonCheck{Check: c, Ms: c.Dur.Milliseconds()}
		if c.Err != nil {
			jc.Error = c.Err.Error()
		}
		out.Checks = append(out.Checks, jc)
	}
	return json.MarshalIndent(out, "", "  ")
}
