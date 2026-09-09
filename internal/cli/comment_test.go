package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func bodyFile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "body.md")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	return path
}

func TestCommentParse(t *testing.T) {
	t.Parallel()

	code, out, errOut := runCLI("comment", "parse", "--command", "revert", "--body-file",
		bodyFile(t, "godwit revert 0123456 --ack H002,H009 --allow-data-loss --force\n"))
	if code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, errOut)
	}
	want := "command=revert\nsha=0123456\nack=H002,H009\nallow-data-loss=true\nforce=true\nrollout=\n"
	if out != want {
		t.Fatalf("out = %q, want %q", out, want)
	}
}

func TestCommentParseSilence(t *testing.T) {
	t.Parallel()

	code, out, errOut := runCLI("comment", "parse", "--command", "apply", "--body-file",
		bodyFile(t, "To deploy, comment:\n\ngodwit apply"))
	if code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, errOut)
	}
	want := "command=\nsha=\nack=\nallow-data-loss=\nforce=\nrollout=\n"
	if out != want {
		t.Fatalf("out = %q, want %q", out, want)
	}
}

func TestCommentParsePlanRollout(t *testing.T) {
	t.Parallel()

	code, out, _ := runCLI("comment", "parse", "--command", "plan", "--body-file",
		bodyFile(t, "/godwit plan --rollout expand-contract"))
	if code != 0 || !strings.Contains(out, "command=plan\n") || !strings.Contains(out, "rollout=expand-contract\n") {
		t.Fatalf("code = %d, out = %q", code, out)
	}
}

func TestCommentParseRefuses(t *testing.T) {
	t.Parallel()

	code, _, errOut := runCLI("comment", "parse", "--command", "apply", "--body-file",
		bodyFile(t, "godwit apply --force"))
	if code != ExitActionRefused || !strings.Contains(errOut, "godwit apply does not take --force") {
		t.Fatalf("code = %d, stderr = %q", code, errOut)
	}
}

func TestCommentParseUnreadableBody(t *testing.T) {
	t.Parallel()

	code, _, errOut := runCLI("comment", "parse", "--body-file", filepath.Join(t.TempDir(), "missing.md"))
	if code != 1 || !strings.Contains(errOut, "missing.md") {
		t.Fatalf("code = %d, stderr = %q", code, errOut)
	}
}
