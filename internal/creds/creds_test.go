package creds_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SamuelMolling/godwit/internal/creds"
	"github.com/SamuelMolling/godwit/internal/creds/credstest"
)

func TestStaticConformance(t *testing.T) {
	t.Parallel()

	ring := envRing(key)
	enc, err := ring.Seal(context.Background(), "postgres://static")
	if err != nil {
		t.Fatal(err)
	}
	credstest.Conformance(t, creds.Static{Keys: ring}, map[string]string{"dsn": enc}, "postgres://static")
}

func TestStaticBadCiphertext(t *testing.T) {
	t.Parallel()

	_, err := creds.Static{Keys: envRing(key)}.DSN(context.Background(), map[string]string{"dsn": "junk"})
	if err == nil || !strings.Contains(err.Error(), "static target") {
		t.Fatalf("err = %v", err)
	}
}

func TestStaticWithoutAKey(t *testing.T) {
	t.Parallel()

	_, err := creds.Static{}.DSN(context.Background(), map[string]string{"dsn": "AAAA"})
	if !errors.Is(err, creds.ErrNoKey) {
		t.Fatalf("err = %v", err)
	}
}

func TestKubernetesConformance(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "dsn")
	if err := os.WriteFile(path, []byte("postgres://k8s\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	credstest.Conformance(t, creds.Kubernetes{}, map[string]string{"path": path}, "postgres://k8s")
}

func TestKubernetesMissingFile(t *testing.T) {
	t.Parallel()

	_, err := creds.Kubernetes{}.DSN(context.Background(), map[string]string{"path": "/nope"})
	if err == nil || !strings.Contains(err.Error(), "read secret") {
		t.Fatalf("err = %v", err)
	}
}

func TestRegistry(t *testing.T) {
	t.Parallel()

	reg := creds.Registry(creds.Keyring{})
	for _, name := range []string{"static", "kubernetes", "vault"} {
		if _, ok := reg[name]; !ok {
			t.Fatalf("missing %s", name)
		}
	}
}
