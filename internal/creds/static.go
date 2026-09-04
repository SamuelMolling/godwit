package creds

import (
	"context"
	"errors"
	"fmt"
)

// Static opens a DSN sealed in the target config by the configured key provider.
type Static struct {
	Keys Keyring
}

// DSN implements Provider.
func (p Static) DSN(ctx context.Context, config map[string]string) (string, error) {
	enc, ok := config["dsn"]
	if !ok {
		return "", errors.New(`static target config missing "dsn"`)
	}
	dsn, err := p.Keys.Open(ctx, enc)
	if err != nil {
		return "", fmt.Errorf("static target: %w", err)
	}

	return dsn, nil
}
