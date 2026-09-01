package cli

import (
	"strings"
	"testing"
)

func TestServeBadMasterKey(t *testing.T) {
	t.Setenv("GODWIT_MASTER_KEY", "not-hex")
	code, _, errOut := runCLI("serve", "--store-dsn", "postgres://x")
	if code != 1 || !strings.Contains(errOut, "GODWIT_MASTER_KEY") {
		t.Fatalf("code = %d, stderr = %s", code, errOut)
	}
}

func TestServeUnreachableStore(t *testing.T) {
	t.Setenv("GODWIT_MASTER_KEY", strings.Repeat("ab", 32))
	t.Setenv("GODWIT_TOKENS", "t1,t2")
	code, _, _ := runCLI("serve", "--store-dsn", "postgres://bad:bad@127.0.0.1:1/x", "--listen", "127.0.0.1:0")
	if code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
}
