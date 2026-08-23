package cli

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

var colorOn bool

func setColor(when string) {
	switch when {
	case "always":
		colorOn = true
	case "never":
		colorOn = false
	default:
		colorOn = isTTY(os.Stdout) && os.Getenv("TERM") != "dumb" && os.Getenv("NO_COLOR") == ""
	}
}

func paint(code, s string) string {
	if !colorOn || s == "" {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}

func bold(s string) string   { return paint("1", s) }
func dim(s string) string    { return paint("2", s) }
func red(s string) string    { return paint("31", s) }
func green(s string) string  { return paint("32", s) }
func yellow(s string) string { return paint("33", s) }
func blue(s string) string   { return paint("34", s) }
func cyan(s string) string   { return paint("36", s) }

var ansiRE = regexp.MustCompile(`\x1b\[[0-9;]*[a-zA-Z]`)

func visLen(s string) int { return len([]rune(ansiRE.ReplaceAllString(s, ""))) }

// isTTY is a real terminal test (an ioctl), not a char-device test: /dev/null
// is a char device, and treating it as a terminal would make `build` with no
// flags hang on a picker nobody can see.
func isTTY(f *os.File) bool {
	var t syscall.Termios
	_, _, errno := syscall.Syscall6(syscall.SYS_IOCTL, f.Fd(),
		uintptr(ioctlGetTermios), uintptr(unsafe.Pointer(&t)), 0, 0, 0)
	return errno == 0
}

func termSize() (cols, rows int) {
	cols, rows = 80, 24
	if v := os.Getenv("COLUMNS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cols = n
		}
	}
	out, err := sttyOutput(os.Stdin, "size")
	if err != nil {
		if out, err = sttyOutput(os.Stdout, "size"); err != nil {
			return cols, rows
		}
	}
	var r, c int
	if _, err := fmt.Sscanf(strings.TrimSpace(string(out)), "%d %d", &r, &c); err == nil && c > 0 {
		return c, r
	}
	return cols, rows
}

func sttyOutput(tty *os.File, args ...string) ([]byte, error) {
	cmd := exec.Command("stty", args...)
	cmd.Stdin = tty
	return cmd.Output()
}

type table struct {
	head  []string
	rows  [][]string
	right map[int]bool
}

func newTable(head ...string) *table {
	return &table{head: head, right: map[int]bool{}}
}

func (t *table) rightAlign(cols ...int) *table {
	for _, c := range cols {
		t.right[c] = true
	}
	return t
}

func (t *table) add(cells ...string) { t.rows = append(t.rows, cells) }

func (t *table) render(w io.Writer) {
	n := len(t.head)
	for _, r := range t.rows {
		if len(r) > n {
			n = len(r)
		}
	}
	width := make([]int, n)
	measure := func(r []string) {
		for i, c := range r {
			if l := visLen(c); l > width[i] {
				width[i] = l
			}
		}
	}
	measure(t.head)
	for _, r := range t.rows {
		measure(r)
	}
	line := func(cells []string, style func(string) string) {
		var b strings.Builder
		for i := 0; i < n; i++ {
			cell := ""
			if i < len(cells) {
				cell = cells[i]
			}
			pad := strings.Repeat(" ", width[i]-visLen(cell))
			if t.right[i] {
				b.WriteString(pad + style(cell))
			} else {
				b.WriteString(style(cell))
				if i != n-1 {
					b.WriteString(pad)
				}
			}
			if i != n-1 {
				b.WriteString("  ")
			}
		}
		fmt.Fprintln(w, strings.TrimRight(b.String(), " "))
	}
	if len(t.head) > 0 {
		line(t.head, func(s string) string { return bold(s) })
	}
	for _, r := range t.rows {
		line(r, func(s string) string { return s })
	}
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit && exp < 4; x /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%c", float64(n)/float64(div), "KMGTP"[exp])
}

func humanDur(d time.Duration) string {
	switch {
	case d < 0:
		return "-"
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	default:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
}

func humanAgo(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return humanDur(time.Since(t)) + " ago"
}

func truncate(s string, n int) string {
	r := []rune(s)
	if n <= 1 || len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
