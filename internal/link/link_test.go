package link_test

import (
	"testing"

	"github.com/SamuelMolling/godwit/internal/link"
)

func TestLinksAreEmptyWithoutABaseOrAnID(t *testing.T) {
	t.Parallel()

	if got := link.Run("", "r1"); got != "" {
		t.Fatalf("no public URL, no link: %q", got)
	}
	if got := link.Plan("https://godwit.example.com", ""); got != "" {
		t.Fatalf("no id, no link: %q", got)
	}
}

func TestLinksTakeOneSlashFromTheBase(t *testing.T) {
	t.Parallel()

	if got := link.Run("https://godwit.example.com/", "r1"); got != "https://godwit.example.com/ui/runs/r1" {
		t.Fatalf("run link = %q", got)
	}
	if got := link.Plan("https://godwit.example.com", "p1"); got != "https://godwit.example.com/ui/plans/p1" {
		t.Fatalf("plan link = %q", got)
	}
}
