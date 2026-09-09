package cli

import (
	"os"
	"strings"
	"testing"

	"github.com/SamuelMolling/godwit/internal/engine"
)

func TestPaletteForResolvesTheTwoSwitches(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		mode    string
		noColor string
		tty     bool
		want    bool
	}{
		{"a tty and nothing set", "", "", true, true},
		{"a pipe", "", "", false, false},
		{"NO_COLOR on a tty", "", "1", true, false},
		{"always in a pipe", "always", "", false, true},
		{"always outranks NO_COLOR", "always", "1", true, true},
		{"never on a tty", "never", "", true, false},
		{"an unknown mode falls back to auto", "yes please", "", true, true},
		{"an unknown mode still writes no escapes into a pipe", "yes please", "", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := paletteFor(tc.mode, tc.noColor, tc.tty).add("x") != "x"
			if got != tc.want {
				t.Fatalf("coloured = %t, want %t", got, tc.want)
			}
		})
	}
}

func TestIsTTYOnlyForACharacterDevice(t *testing.T) {
	t.Parallel()

	if isTTY(&strings.Builder{}) {
		t.Fatal("a buffer is not a terminal")
	}
	f, err := os.CreateTemp(t.TempDir(), "out")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if isTTY(f) {
		t.Fatal("a regular file is not a terminal")
	}
}

func TestColourMarksTheDirectionOfEveryChange(t *testing.T) {
	t.Parallel()

	if got := coloured.change(engine.DirectionUp, "x"); got != "\x1b[32m+ x\x1b[0m" {
		t.Fatalf("applied = %q, want green", got)
	}
	if got := coloured.change(engine.DirectionDown, "x"); got != "\x1b[31m- x\x1b[0m" {
		t.Fatalf("reverted = %q, want red", got)
	}
	for _, tc := range []struct{ line, want string }{
		{"+ table t", "\x1b[32m+ table t\x1b[0m"},
		{"- index i", "\x1b[31m- index i\x1b[0m"},
		{"unchanged", "unchanged"},
	} {
		if got := coloured.diff(tc.line); got != tc.want {
			t.Fatalf("diff(%q) = %q, want %q", tc.line, got, tc.want)
		}
	}
}

func TestColourAddsNothingButEscapes(t *testing.T) {
	t.Setenv("GODWIT_COLOR", "always")
	r := planReport{
		live: true, target: "app", rollout: "direct", validated: true, drift: "+ table public.orders",
		items: []planItem{hazardItem("orders", false)},
	}
	var lit, dark strings.Builder
	writePlanText(&lit, r)
	t.Setenv("GODWIT_COLOR", "never")
	writePlanText(&dark, r)
	if lit.String() == dark.String() {
		t.Fatal("GODWIT_COLOR=always must colour the text renderer")
	}
	if got := strings.ReplaceAll(strings.ReplaceAll(lit.String(), "\x1b[32m", ""), "\x1b[31m", ""); strings.ReplaceAll(got, "\x1b[0m", "") != dark.String() {
		t.Fatalf("stripping the escapes must give back the plain report:\n%s", lit.String())
	}
	if !strings.Contains(dark.String(), "\n+ 20260901120000_orders  ") || !strings.Contains(dark.String(), "\n  + table public.orders\n") {
		t.Fatalf("the marks carry the meaning without colour:\n%s", dark.String())
	}
}

func TestMarkdownIsNeverColoured(t *testing.T) {
	t.Setenv("GODWIT_COLOR", "always")
	var b strings.Builder
	writePlanMarkdown(&b, planReport{live: true, target: "app", rollout: "direct", validated: true, items: []planItem{hazardItem("orders", false)}})
	if strings.Contains(b.String(), "\x1b[") {
		t.Fatalf("a forge paints the diff fence itself; escapes would be posted as text:\n%s", b.String())
	}
	if !strings.Contains(b.String(), "```diff\n+ 20260901120000_orders  1 statement\n```\n") {
		t.Fatalf("the change list is what the fence paints:\n%s", b.String())
	}
}
