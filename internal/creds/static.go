package creds

import (
	"context"
	"errors"
)

// Static decrypts a DSN stored (AES-GCM) in the target config.
type Static struct {
	Key []byte
}

// DSN implements Provider.
func (p Static) DSN(_ context.Context, config map[string]string) (string, error) {
	enc, ok := config["dsn"]
	if !ok {
		return "", errors.New(`static target config missing "dsn"`)
	}

	return Decrypt(p.Key, enc)
}
