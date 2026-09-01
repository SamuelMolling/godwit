package creds

import (
	"bytes"
	"crypto/cipher"
	"errors"
	"testing"
)

func TestEncryptNonceReaderFails(t *testing.T) {
	orig := randReader
	randReader = bytes.NewReader(nil) // EOF
	defer func() { randReader = orig }()

	if _, err := Encrypt(bytes.Repeat([]byte("k"), 32), "x"); err == nil {
		t.Fatal("want error")
	}
}

func TestNewGCMFails(t *testing.T) {
	orig := newAEAD
	newAEAD = func(cipher.Block) (cipher.AEAD, error) { return nil, errors.New("boom") }
	defer func() { newAEAD = orig }()

	if _, err := Encrypt(bytes.Repeat([]byte("k"), 32), "x"); err == nil {
		t.Fatal("want error")
	}
}
