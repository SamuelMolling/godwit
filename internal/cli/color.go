package cli

import (
	"io"
	"os"
	"strings"

	"github.com/SamuelMolling/godwit/internal/engine"
)

type palette struct{ add, del func(string) string }

func ansi(code string) func(string) string {
	return func(s string) string {
		return "\x1b[" + code + "m" + s + "\x1b[0m"
	}
}

var (
	mono     = palette{plain, plain}
	coloured = palette{ansi("32"), ansi("31")}
)

func (p palette) change(d engine.Direction, line string) string {
	if d == engine.DirectionDown {
		return p.del("- " + line)
	}

	return p.add("+ " + line)
}

func (p palette) diff(line string) string {
	switch {
	case strings.HasPrefix(line, "+"):
		return p.add(line)
	case strings.HasPrefix(line, "-"):
		return p.del(line)
	}

	return line
}

func colors(w io.Writer) palette {
	return paletteFor(os.Getenv("GODWIT_COLOR"), os.Getenv("NO_COLOR"), isTTY(w))
}

// paletteFor: an explicit GODWIT_COLOR outranks the ambient NO_COLOR, and an unknown value falls back to auto.
func paletteFor(mode, noColor string, tty bool) palette {
	switch mode {
	case "always":
		return coloured
	case "never":
		return mono
	}
	if noColor != "" || !tty {
		return mono
	}

	return coloured
}

func isTTY(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	st, err := f.Stat()

	return err == nil && st.Mode()&os.ModeCharDevice != 0
}
