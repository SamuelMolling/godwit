package report

import (
	"io"
	"os"
	"strings"

	"github.com/SamuelMolling/godwit/internal/engine"
)

// Palette marks a line in a text report by what it does: added, destroyed, or changed in place.
type Palette struct{ add, del, mod func(string) string }

func ansi(code string) func(string) string {
	return func(s string) string {
		return "\x1b[" + code + "m" + s + "\x1b[0m"
	}
}

// The two palettes a report renders with: Mono writes the marks alone, Coloured adds ANSI escapes.
var (
	Mono     = Palette{plain, plain, plain}
	Coloured = Palette{ansi("32"), ansi("31"), ansi("33")}
)

func (p Palette) op(operation, line string) string {
	switch operation {
	case engine.OpCreate:
		return p.add(line)
	case engine.OpDestroy:
		return p.del(line)
	default:
		return p.mod(line)
	}
}

func (p Palette) change(d engine.Direction, line string) string {
	if d == engine.DirectionDown {
		return p.del("- " + line)
	}

	return p.add("+ " + line)
}

func (p Palette) diff(line string) string {
	switch {
	case strings.HasPrefix(line, "+"):
		return p.add(line)
	case strings.HasPrefix(line, "-"):
		return p.del(line)
	}

	return line
}

// Colors is the palette a report renders with when it is written to w: none at all unless w is a terminal
// that has not asked to go without.
func Colors(w io.Writer) Palette {
	return paletteFor(os.Getenv("GODWIT_COLOR"), os.Getenv("NO_COLOR"), isTTY(w))
}

// paletteFor: an explicit GODWIT_COLOR outranks the ambient NO_COLOR, and an unknown value falls back to auto.
func paletteFor(mode, noColor string, tty bool) Palette {
	switch mode {
	case "always":
		return Coloured
	case "never":
		return Mono
	}
	if noColor != "" || !tty {
		return Mono
	}

	return Coloured
}

func isTTY(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	st, err := f.Stat()

	return err == nil && st.Mode()&os.ModeCharDevice != 0
}
