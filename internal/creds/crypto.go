package creds

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
)

// Injection points for otherwise unreachable error branches.
var (
	randReader io.Reader = rand.Reader
	newAEAD              = cipher.NewGCM
)

const dataKeyBytes = 32

func sealGCM(key, aad []byte, plaintext string) ([]byte, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(randReader, nonce); err != nil {
		return nil, fmt.Errorf("nonce: %w", err)
	}

	return gcm.Seal(nonce, nonce, []byte(plaintext), aad), nil
}

func openGCM(key, aad, raw []byte) (string, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return "", err
	}
	if len(raw) < gcm.NonceSize() {
		return "", errors.New("ciphertext too short")
	}
	plain, err := gcm.Open(nil, raw[:gcm.NonceSize()], raw[gcm.NonceSize():], aad)
	if err != nil {
		return "", fmt.Errorf("decrypt: %w", err)
	}

	return string(plain), nil
}

func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("cipher: %w", err)
	}
	gcm, err := newAEAD(block)
	if err != nil {
		return nil, fmt.Errorf("gcm: %w", err)
	}

	return gcm, nil
}

func randomDataKey() ([]byte, error) {
	key := make([]byte, dataKeyBytes)
	if _, err := io.ReadFull(randReader, key); err != nil {
		return nil, fmt.Errorf("data key: %w", err)
	}

	return key, nil
}
