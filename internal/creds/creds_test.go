package creds_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SamuelMolling/godwit/internal/creds"
	"github.com/SamuelMolling/godwit/internal/creds/credstest"
)

var key = bytes.Repeat([]byte("k"), 32)

func TestEncryptDecryptRoundTrip(t *testing.T) {
	t.Parallel()

	enc, err := creds.Encrypt(key, "postgres://u:p@h/db")
	if err != nil {
		t.Fatal(err)
	}
	if enc == "postgres://u:p@h/db" {
		t.Fatal("not encrypted")
	}
	dec, err := creds.Decrypt(key, enc)
	if err != nil || dec != "postgres://u:p@h/db" {
		t.Fatalf("dec = %q, err = %v", dec, err)
	}
}

func TestCryptoErrors(t *testing.T) {
	t.Parallel()

	if _, err := creds.Encrypt([]byte("short"), "x"); err == nil {
		t.Fatal("bad key must fail")
	}
	if _, err := creds.Decrypt([]byte("short"), "x"); err == nil {
		t.Fatal("bad key must fail")
	}
	if _, err := creds.Decrypt(key, "!!!not-base64!!!"); err == nil {
		t.Fatal("bad base64 must fail")
	}
	if _, err := creds.Decrypt(key, "c2hvcnQ="); err == nil {
		t.Fatal("short ciphertext must fail")
	}
	enc, _ := creds.Encrypt(key, "x")
	other := bytes.Repeat([]byte("o"), 32)
	if _, err := creds.Decrypt(other, enc); err == nil {
		t.Fatal("wrong key must fail")
	}
}

func TestStaticConformance(t *testing.T) {
	t.Parallel()

	enc, err := creds.Encrypt(key, "postgres://static")
	if err != nil {
		t.Fatal(err)
	}
	credstest.Conformance(t, creds.Static{Key: key}, map[string]string{"dsn": enc}, "postgres://static")
}

func TestStaticBadCiphertext(t *testing.T) {
	t.Parallel()

	if _, err := (creds.Static{Key: key}).DSN(context.Background(), map[string]string{"dsn": "junk"}); err == nil {
		t.Fatal("bad ciphertext must fail")
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

	reg := creds.Registry(key)
	if _, ok := reg["static"]; !ok {
		t.Fatal("missing static")
	}
	if _, ok := reg["kubernetes"]; !ok {
		t.Fatal("missing kubernetes")
	}
}
