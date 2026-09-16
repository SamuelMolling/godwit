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

func TestCommitOfIgnoresProvenanceItCannotRead(t *testing.T) {
	t.Parallel()

	for _, source := range []string{"", "db/migrations", "github.com/acme/app@notasha", "acme/app@0419cdd1c2f"} {
		if got := link.CommitOf(source); got != (link.Commit{}) {
			t.Fatalf("CommitOf(%q) = %+v, want nothing", source, got)
		}
	}
}

func TestCommitOfReadsTheHostRepositoryAndDirectory(t *testing.T) {
	t.Parallel()

	got := link.CommitOf("ghe.acme.com/team/app@0419cdd1c2f")
	want := link.Commit{Repo: "ghe.acme.com/team/app", Short: "0419cdd", Href: "https://ghe.acme.com/team/app/commit/0419cdd1c2f"}
	if got != want {
		t.Fatalf("CommitOf on an enterprise host = %+v", got)
	}

	got = link.CommitOf("github.com/acme/orders@0419cdd1c2f3a4b5c6d7e8f90123456789abcdef:db/migrations")
	want = link.Commit{
		Repo: "github.com/acme/orders", Short: "0419cdd", Dir: "db/migrations",
		Href: "https://github.com/acme/orders/commit/0419cdd1c2f3a4b5c6d7e8f90123456789abcdef",
	}
	if got != want {
		t.Fatalf("CommitOf with a directory = %+v", got)
	}
}
