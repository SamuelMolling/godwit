package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFiles(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	return dir
}

func TestLoadDir(t *testing.T) {
	t.Parallel()

	dir := writeFiles(t, map[string]string{
		"20260901120000_create_users.up.sql":   "CREATE TABLE users (id int);",
		"20260901120000_create_users.down.sql": "DROP TABLE users;",
		"20260901130000_add_email.up.sql":      "ALTER TABLE users ADD COLUMN email text;",
		"20260901130000_add_email.down.sql":    "ALTER TABLE users DROP COLUMN email;",
	})
	if err := os.Mkdir(filepath.Join(dir, "subdir"), 0o750); err != nil {
		t.Fatal(err)
	}

	migs, err := LoadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(migs) != 2 {
		t.Fatalf("got %d migrations, want 2", len(migs))
	}
	if migs[0].Version != 20260901120000 || migs[1].Version != 20260901130000 {
		t.Fatalf("wrong order: %d, %d", migs[0].Version, migs[1].Version)
	}
	if migs[0].Name != "create_users" || migs[0].Checksum == "" || migs[0].DownSQL == "" {
		t.Fatalf("bad migration: %+v", migs[0])
	}
}

func TestLoadDirErrors(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		files   map[string]string
		wantErr string
	}{
		{
			name:    "unexpected file",
			files:   map[string]string{"notes.txt": "x"},
			wantErr: "unexpected file",
		},
		{
			name:    "empty file",
			files:   map[string]string{"20260901120000_a.up.sql": "  \n"},
			wantErr: "is empty",
		},
		{
			name: "conflicting names",
			files: map[string]string{
				"20260901120000_a.up.sql":   "SELECT 1;",
				"20260901120000_b.down.sql": "SELECT 1;",
			},
			wantErr: "two names",
		},
		{
			name:    "missing down",
			files:   map[string]string{"20260901120000_a.up.sql": "SELECT 1;"},
			wantErr: "missing down file",
		},
		{
			name:    "missing up",
			files:   map[string]string{"20260901120000_a.down.sql": "SELECT 1;"},
			wantErr: "missing up file",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := LoadDir(writeFiles(t, tc.files))
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestLoadDirMissingDir(t *testing.T) {
	t.Parallel()

	if _, err := LoadDir(filepath.Join(t.TempDir(), "nope")); err == nil {
		t.Fatal("want error for missing dir")
	}
}

func TestLoadDirUnreadableFile(t *testing.T) {
	t.Parallel()

	dir := writeFiles(t, map[string]string{"20260901120000_a.up.sql": "SELECT 1;"})
	if err := os.Chmod(filepath.Join(dir, "20260901120000_a.up.sql"), 0o000); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadDir(dir); err == nil {
		t.Fatal("want error for unreadable file")
	}
}
