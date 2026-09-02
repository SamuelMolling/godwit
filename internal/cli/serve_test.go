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
	code, _, _ := runCLI("serve", "--store-dsn", "postgres://bad:bad@127.0.0.1:1/x", "--listen", "127.0.0.1:0",
		"--lease-ttl", "5s", "--tick-interval", "500ms", "--max-attempts", "2")
	if code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
}

func TestServeBadSchedulerFlags(t *testing.T) {
	t.Setenv("GODWIT_MASTER_KEY", strings.Repeat("ab", 32))
	code, _, errOut := runCLI("serve", "--store-dsn", "postgres://x", "--max-attempts", "many")
	if code != 1 || !strings.Contains(errOut, "max-attempts") {
		t.Fatalf("code = %d, stderr = %s", code, errOut)
	}
}

func TestServeBadLogFlags(t *testing.T) {
	t.Setenv("GODWIT_LOG_FORMAT", "text")
	code, _, errOut := runCLI("serve", "--store-dsn", "postgres://x", "--log-level", "loud")
	if code != 1 || !strings.Contains(errOut, `log level "loud"`) {
		t.Fatalf("code = %d, stderr = %s", code, errOut)
	}

	t.Setenv("GODWIT_LOG_FORMAT", "yaml")
	code, _, errOut = runCLI("serve", "--store-dsn", "postgres://x")
	if code != 1 || !strings.Contains(errOut, `log format "yaml"`) {
		t.Fatalf("code = %d, stderr = %s", code, errOut)
	}
}

func TestServeLogFormatText(t *testing.T) {
	t.Setenv("GODWIT_MASTER_KEY", strings.Repeat("ab", 32))
	t.Setenv("GODWIT_LOG_LEVEL", "debug")
	code, _, errOut := runCLI("serve", "--store-dsn", "postgres://bad:bad@127.0.0.1:1/x", "--listen", "127.0.0.1:0", "--log-format", "text")
	if code != 1 || strings.Contains(errOut, "log format") {
		t.Fatalf("code = %d, stderr = %s", code, errOut)
	}
}
