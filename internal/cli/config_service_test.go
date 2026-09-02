package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	godwitv1 "github.com/SamuelMolling/godwit/gen/godwit/v1"
)

func TestConfigFileDrivesServiceCommands(t *testing.T) {
	for _, k := range []string{"GODWIT_DIR", "GODWIT_TARGET", "GODWIT_ROLLOUT", "GODWIT_SERVER"} {
		t.Setenv(k, "")
	}
	stub := &stubService{events: []*godwitv1.Run{run("r1", godwitv1.RunState_RUN_STATE_SUCCEEDED, 1)}}
	url := startStub(t, stub)
	root := t.TempDir()
	if err := os.Rename(goodMigs(t), filepath.Join(root, "db")); err != nil {
		t.Fatal(err)
	}
	yaml := fmt.Sprintf("dir: db\ntarget: orders\nrollout: expand-contract\nserver: %s\n", url)
	if err := os.WriteFile(filepath.Join(root, "godwit.yaml"), []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)

	if code, _, errOut := runCLI("migrate"); code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, errOut)
	}
	c := stub.created
	if c.Target != "orders" || c.Rollout != "expand-contract" || len(c.Files) != 2 {
		t.Fatalf("request = %v", c)
	}
	if code, _, errOut := runCLI("migrate", "--target", "other", "--rollout", "direct"); code != 0 || stub.created.Target != "other" || stub.created.Rollout != "direct" {
		t.Fatalf("flags must beat the file: code = %d, stderr = %s, request = %v", code, errOut, stub.created)
	}
	if code, _, errOut := runCLI("runs"); code != 0 || stub.listed.Target != "" {
		t.Fatalf("runs: code = %d, stderr = %s, filter = %q", code, errOut, stub.listed.Target)
	}
	if code, out, errOut := runCLI("run", "confirm", "--latest", "--allow-none"); code != 0 || out != "target orders: no run awaiting contract\n" {
		t.Fatalf("confirm: code = %d, out = %q, stderr = %s", code, out, errOut)
	}
	if code, _, errOut := runCLI("lint"); code != 1 || !strings.Contains(errOut, "blocking") {
		t.Fatalf("lint must read dir from the file: code = %d, stderr = %s", code, errOut)
	}
}

func TestMigrateRequiresTarget(t *testing.T) {
	t.Parallel()

	code, _, errOut := runCLI("migrate", "--server", "http://127.0.0.1:1")
	if code != 1 || !strings.Contains(errOut, "--target (or target in godwit.yaml) is required") {
		t.Fatalf("code = %d, stderr = %s", code, errOut)
	}
}
