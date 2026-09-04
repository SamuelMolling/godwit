package creds

import (
	"bytes"
	"crypto/cipher"
	"errors"
	"testing"
)

func TestSealNonceReaderFails(t *testing.T) {
	orig := randReader
	randReader = bytes.NewReader(nil) // EOF
	defer func() { randReader = orig }()

	if _, err := sealGCM(bytes.Repeat([]byte("k"), 32), nil, "x"); err == nil {
		t.Fatal("want error")
	}
	if _, err := randomDataKey(); err == nil {
		t.Fatal("want error")
	}
}

func TestNewGCMFails(t *testing.T) {
	orig := newAEAD
	newAEAD = func(cipher.Block) (cipher.AEAD, error) { return nil, errors.New("boom") }
	defer func() { newAEAD = orig }()

	if _, err := sealGCM(bytes.Repeat([]byte("k"), 32), nil, "x"); err == nil {
		t.Fatal("want error")
	}
	if _, err := openGCM(bytes.Repeat([]byte("k"), 32), nil, nil); err == nil {
		t.Fatal("want error")
	}
}

func TestGCMRejectsAShortKey(t *testing.T) {
	t.Parallel()

	if _, err := sealGCM([]byte("short"), nil, "x"); err == nil {
		t.Fatal("want error")
	}
}

func TestOpenGCMShortCiphertext(t *testing.T) {
	t.Parallel()

	if _, err := openGCM(bytes.Repeat([]byte("k"), 32), nil, []byte("short")); err == nil {
		t.Fatal("want error")
	}
}

func TestBlobFraming(t *testing.T) {
	t.Parallel()

	blob, err := joinBlob([]byte("wrapped"), []byte("inner"))
	if err != nil {
		t.Fatal(err)
	}
	wrapped, inner, err := splitBlob(blob)
	if err != nil || string(wrapped) != "wrapped" || string(inner) != "inner" {
		t.Fatalf("wrapped = %q, inner = %q, err = %v", wrapped, inner, err)
	}
	if _, err := joinBlob(make([]byte, maxWrappedKey), nil); err == nil {
		t.Fatal("oversized wrapped key must fail")
	}
	if _, _, err := splitBlob([]byte{1}); err == nil {
		t.Fatal("want error")
	}
	if _, _, err := splitBlob([]byte{0, 9, 1}); err == nil {
		t.Fatal("want error")
	}
}
