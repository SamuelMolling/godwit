package creds

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
)

// Kubernetes reads the DSN from a mounted secret file.
type Kubernetes struct{}

// DSN implements Provider.
func (Kubernetes) DSN(_ context.Context, config map[string]string) (string, error) {
	path, ok := config["path"]
	if !ok {
		return "", errors.New(`kubernetes target config missing "path"`)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read secret: %w", err)
	}

	return strings.TrimSpace(string(body)), nil
}
