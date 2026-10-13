package main

import (
	"fmt"
	"io"
	"os"
	"strings"
)

var logoPixels = []string{
	".......######........",
	".....##########......",
	"...##############....",
	"...#####....######...",
	"..####........####...",
	".####..........##....",
	".###.................",
	"####.................",
	"####............ggg..",
	"###.......oooooggggg.",
	"###.......oooooggggg.",
	"####............ggg..",
	"####.................",
	".###.................",
	".####..........##....",
	"..####........####...",
	"...#####....######...",
	"...##############....",
	".....##########......",
	".......######........",
}

type palette struct {
	on     bool
	track  string
	signal string
	dim    string
}

func newPalette() palette {
	if os.Getenv("NO_COLOR") != "" || os.Getenv("TERM") == "dumb" {
		return palette{}
	}

	if ct := os.Getenv("COLORTERM"); ct == "truecolor" || ct == "24bit" {
		return palette{on: true, track: "74;74;74", signal: "74;222;128", dim: "2"}
	}

	return palette{on: true, track: "5;239", signal: "5;78", dim: "2"}
}

func (p palette) color(fg bool, c byte) string {
	code := map[byte]string{'o': p.track, 'g': p.signal}[c]
	if code == "" {
		if fg {
			return "39"
		}

		return "49"
	}

	kind := "38;"
	if !fg {
		kind = "48;"
	}

	if strings.HasPrefix(code, "5;") {
		return kind + code
	}

	return kind + "2;" + code
}

// The C is drawn in the terminal's own foreground colour so it shows on light and dark themes alike.
func renderLogo(p palette) []string {
	lines := make([]string, 0, len(logoPixels)/2)

	for y := 0; y+1 < len(logoPixels); y += 2 {
		var b strings.Builder

		for x := range len(logoPixels[y]) {
			top, bot := logoPixels[y][x], logoPixels[y+1][x]
			glyph, fg, bg := "▀", top, bot

			switch {
			case top == '.' && bot == '.':
				b.WriteString(p.reset() + " ")

				continue
			case top == bot:
				glyph, bg = "█", '.'
			case top == '.' || (bot == '#' && top != '#'):
				glyph, fg, bg = "▄", bot, top
			}

			if !p.on {
				b.WriteString(map[bool]string{true: "█", false: glyph}[top != '.' && bot != '.'])

				continue
			}

			fmt.Fprintf(&b, "\x1b[%s;%sm%s", p.color(true, fg), p.color(false, bg), glyph)
		}

		lines = append(lines, b.String()+p.reset())
	}

	return lines
}

func (p palette) reset() string {
	if !p.on {
		return ""
	}

	return "\x1b[0m"
}

func (p palette) paint(code, s string) string {
	if !p.on {
		return s
	}

	return "\x1b[" + code + "m" + s + "\x1b[0m"
}

func printBanner(w io.Writer, home string) {
	p := newPalette()

	fmt.Fprintln(w)

	for _, line := range renderLogo(p) {
		fmt.Fprintln(w, "  "+line)
	}

	fmt.Fprintln(w)
	fmt.Fprintf(w, "  %s  %s\n\n", p.paint("1", "csm"), p.paint(p.dim, "v"+version))

	for _, r := range bannerRows(home) {
		fmt.Fprintf(w, "  %s %s\n", p.paint(p.dim, fmt.Sprintf("%-10s", r[0])), r[1])
	}

	fmt.Fprintln(w)
	fmt.Fprintf(w, "  %s\n\n", p.paint(p.dim, "csm claude · csm status · csm issues · csm help for every command"))
}

func bannerRows(home string) [][2]string {
	var rows [][2]string

	s, err := loadState(home)

	switch {
	case err != nil || len(s.Accounts) == 0:
		rows = append(rows, [2]string{"accounts", "none yet · csm account add <name>"})
	default:
		var active []string

		for _, id := range s.providersInUse() {
			if name := s.active(id); name != "" {
				active = append(active, providerName(id)+": "+name)
			}
		}

		rows = append(rows, [2]string{"accounts", strings.Join(active, " · ")})
		rows = append(rows, [2]string{"switching", strings.SplitN(describePolicy(s.Config.policy()), " (", 2)[0]})
	}

	service := "not running · csm serve --detach"
	if dialable(socketPath(home)) {
		service = "running"
	}

	rows = append(rows, [2]string{"service", service})

	sentry := "not connected · csm sentry connect"
	if cfg := loadAutofixConfig(home); cfg.Sentry != nil && cfg.Sentry.Token != "" {
		sentry = cfg.Sentry.Org
	}

	rows = append(rows, [2]string{"sentry", sentry})

	if cwd, err := os.Getwd(); err == nil {
		rows = append(rows, [2]string{"cwd", displayPath(cwd)})
	}

	return rows
}
