// Package schemasource turns a description of the desired database into PostgreSQL DDL without a database.
package schemasource

import (
	"context"
	"os"
)

// Source yields the DDL of the whole desired database.
type Source interface {
	Load(ctx context.Context) (string, error)
}

// File is a Source over a file that already holds plain DDL.
type File struct {
	Path string
}

// Load returns the file body.
func (f File) Load(context.Context) (string, error) {
	body, err := os.ReadFile(f.Path)
	if err != nil {
		return "", err
	}

	return string(body), nil
}
