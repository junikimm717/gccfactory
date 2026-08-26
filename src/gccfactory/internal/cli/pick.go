package cli

import (
	"context"
	"fmt"
	"os"
	"strings"

	"golang.org/x/term"

	"github.com/junikimm717/gccfactory/src/gccfactory/internal/triple"
)

// pickMatrix is the two-column multi-select shown when `build`/`verify` is run
// with no --host/--target on a terminal. Raw ANSI plus x/term for raw mode,
// which is all a container terminal needs.
//
//	tab      switch column        space  toggle
//	up/down  move (or j/k)        a      select all / none in this column
//	p        select `proven`      enter  go             q  quit
func pickMatrix(cmd string) ([]triple.Triple, []triple.Triple, error) {
	restore, err := rawMode()
	if err != nil {
		return nil, nil, fmt.Errorf("cannot switch the terminal to raw mode (%v); pass --host/--target instead", err)
	}
	defer restore()

	p := &picker{cmd: cmd, items: triple.Known}
	p.sel[0] = map[int]bool{}
	p.sel[1] = map[int]bool{}
	// Pre-seed with `proven`; the common case is one keypress away. Seeded
	// per column: `proven` is role-dependent, and seeding hosts from the
	// union would select all 11 as hosts and offer a 121-cell build.
	for i, it := range p.items {
		p.sel[0][i] = contains(triple.ProvenHosts, it)
		p.sel[1][i] = contains(triple.ProvenTargets, it)
	}

	fmt.Print("\x1b[?25l") // hide cursor
	defer fmt.Print("\x1b[?25h")
	p.draw(false)

	buf := make([]byte, 8)
	for {
		n, err := os.Stdin.Read(buf)
		if err != nil || n == 0 {
			p.clear()
			return nil, nil, context.Canceled
		}
		switch k := decodeKey(buf[:n]); k {
		case "q", "esc", "ctrl-c":
			p.clear()
			return nil, nil, context.Canceled
		case "tab", "right", "left", "h", "l":
			p.col = 1 - p.col
		case "up", "k":
			p.cur[p.col] = (p.cur[p.col] - 1 + len(p.items)) % len(p.items)
		case "down", "j":
			p.cur[p.col] = (p.cur[p.col] + 1) % len(p.items)
		case "space":
			i := p.cur[p.col]
			p.sel[p.col][i] = !p.sel[p.col][i]
		case "a":
			all := len(p.sel[p.col]) == len(p.items) && p.allSet(p.col)
			for i := range p.items {
				p.sel[p.col][i] = !all
			}
		case "p":
			proven := triple.ProvenTargets
			if p.col == 0 {
				proven = triple.ProvenHosts
			}
			for i, it := range p.items {
				p.sel[p.col][i] = contains(proven, it)
			}
		case "enter":
			hosts, targets := p.result()
			if len(hosts) == 0 && len(targets) == 0 {
				p.msg = "select at least one triple (space to toggle)"
				break
			}
			p.msg = ""
			p.draw(true)
			return hosts, targets, nil
		}
		p.draw(false)
	}
}

type picker struct {
	cmd   string
	items []string
	sel   [2]map[int]bool
	cur   [2]int
	col   int
	msg   string
	drawn int
}

func (p *picker) allSet(col int) bool {
	for i := range p.items {
		if !p.sel[col][i] {
			return false
		}
	}
	return true
}

func (p *picker) result() (hosts, targets []triple.Triple) {
	for i, it := range p.items {
		if p.sel[0][i] {
			hosts = append(hosts, triple.MustParse(it))
		}
		if p.sel[1][i] {
			targets = append(targets, triple.MustParse(it))
		}
	}
	return
}

const pickColWidth = 26

func (p *picker) draw(final bool) {
	var b strings.Builder
	if p.drawn > 0 {
		fmt.Fprintf(&b, "\x1b[%dA", p.drawn)
	}
	line := func(s string) { b.WriteString("\x1b[2K" + s + "\r\n") }

	hdr := func(i int, title string) string {
		n := 0
		for j := range p.items {
			if p.sel[i][j] {
				n++
			}
		}
		t := fmt.Sprintf("%s (%d)", title, n)
		if p.col == i {
			return bold(cyan("> " + t))
		}
		return dim("  " + t)
	}
	line(bold("gccfactory "+p.cmd) + "  -  choose the matrix")
	line("")
	line(pad(hdr(0, "HOSTS   run the compiler"), pickColWidth) + hdr(1, "TARGETS emit code for"))
	for i, it := range p.items {
		line(pad(p.cell(0, i, it), pickColWidth) + p.cell(1, i, it))
	}
	line("")
	hosts, targets := p.result()
	line(dim("  " + describeMatrix(hosts, targets)))
	if p.msg != "" {
		line("  " + yellow(p.msg))
	} else {
		line("")
	}
	line(dim("  tab switch column   space toggle   a all/none   p proven   enter build   q quit"))

	p.drawn = strings.Count(b.String(), "\r\n")
	fmt.Print(b.String())
	if final {
		p.clear()
	}
}

func (p *picker) cell(col, i int, name string) string {
	mark := " "
	if p.sel[col][i] {
		mark = "x"
	}
	s := fmt.Sprintf("[%s] %s", mark, name)
	if p.cur[col] == i {
		if p.col == col {
			return bold(cyan("> " + s))
		}
		return dim("> ") + s
	}
	return "  " + s
}

// clear erases the picker so the command's own output starts at the top.
func (p *picker) clear() {
	if p.drawn > 0 {
		fmt.Printf("\x1b[%dA\x1b[0J", p.drawn)
		p.drawn = 0
	}
}

func pad(s string, w int) string {
	if n := w - visLen(s); n > 0 {
		return s + strings.Repeat(" ", n)
	}
	return s + " "
}

func contains(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}

func rawMode() (func(), error) {
	fd := int(os.Stdin.Fd())
	state, err := term.MakeRaw(fd)
	if err != nil {
		return nil, err
	}
	return func() { _ = term.Restore(fd, state) }, nil
}

func decodeKey(b []byte) string {
	if len(b) >= 3 && b[0] == 0x1b && b[1] == '[' {
		switch b[2] {
		case 'A':
			return "up"
		case 'B':
			return "down"
		case 'C':
			return "right"
		case 'D':
			return "left"
		case 'Z':
			return "tab"
		}
		return ""
	}
	switch b[0] {
	case 0x1b:
		return "esc"
	case 0x03:
		return "ctrl-c"
	case '\t':
		return "tab"
	case '\r', '\n':
		return "enter"
	case ' ':
		return "space"
	}
	return strings.ToLower(string(b[:1]))
}
