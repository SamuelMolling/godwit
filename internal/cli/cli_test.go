package cli

import (
	"strings"
	"testing"
)

func TestMainVersion(t *testing.T) {
	t.Parallel()

	var out, errOut strings.Builder
	if code := Main([]string{"version"}, &out, &errOut); code != 0 {
		t.Fatalf("Main() = %d, want 0; stderr: %s", code, errOut.String())
	}

	if got, want := out.String(), "dev (none)\n"; got != want {
		t.Fatalf("version output = %q, want %q", got, want)
	}
}

func TestMainNoArgsPrintsHelp(t *testing.T) {
	t.Parallel()

	var out, errOut strings.Builder
	if code := Main(nil, &out, &errOut); code != 0 {
		t.Fatalf("Main() = %d, want 0; stderr: %s", code, errOut.String())
	}

	if !strings.Contains(out.String(), "godwit") || !strings.Contains(out.String(), "version") {
		t.Fatalf("help output missing expected content: %q", out.String())
	}
}

func TestRunNoArgsPrintsHelp(t *testing.T) {
	t.Parallel()

	code, out, errOut := runCLI("run")
	if code != 0 || !strings.Contains(out, "confirm") {
		t.Fatalf("run = %d, out %q, stderr %q", code, out, errOut)
	}
}

func TestMainUnknownCommandFails(t *testing.T) {
	t.Parallel()

	var out, errOut strings.Builder
	if code := Main([]string{"bogus"}, &out, &errOut); code != 1 {
		t.Fatalf("Main() = %d, want 1", code)
	}

	if !strings.Contains(errOut.String(), "godwit:") {
		t.Fatalf("stderr missing error prefix: %q", errOut.String())
	}
}
