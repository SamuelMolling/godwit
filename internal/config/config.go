// Package config loads the godwit.yaml project file with env overrides.
package config

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// FileName is the project file looked up from the working directory upwards.
const FileName = "godwit.yaml"

// Kinds accepted in schema_source.kind.
const (
	SourceFile    = "file"
	SourcePrisma  = "prisma"
	SourceGorm    = "gorm"
	SourceDjango  = "django"
	SourceAlembic = "alembic"
	SourceRails   = "rails"
	SourceDrizzle = "drizzle"
	SourceCommand = "command"
)

var sourceKinds = []string{SourceFile, SourcePrisma, SourceGorm, SourceDjango, SourceAlembic, SourceRails, SourceDrizzle, SourceCommand}

// Config holds the per-project defaults shared by every CLI command.
type Config struct {
	Dir              string        `yaml:"dir"`
	Target           string        `yaml:"target"`
	Rollout          string        `yaml:"rollout"`
	Server           string        `yaml:"server"`
	LockTimeout      time.Duration `yaml:"lock_timeout"`
	StatementTimeout time.Duration `yaml:"statement_timeout"`
	AllowOutOfOrder  bool          `yaml:"allow_out_of_order"`
	SchemaSource     *SchemaSource `yaml:"schema_source"`
}

// SchemaSource says how the desired schema of this directory is rendered to DDL.
type SchemaSource struct {
	Kind    string   `yaml:"kind"`
	Path    string   `yaml:"path"`
	Bin     string   `yaml:"bin"`
	Command []string `yaml:"command"`
	Lint    *bool    `yaml:"lint"`
}

// LintEnabled reports whether lint treats a stale generated migration as an error rather than a warning.
func (s *SchemaSource) LintEnabled() bool {
	return s == nil || s.Lint == nil || *s.Lint
}

var getwd = os.Getwd

// Defaults returns the configuration used when no file and no env are set.
func Defaults() Config {
	return Config{Dir: "migrations", LockTimeout: 5 * time.Second}
}

// Load reads path, or the nearest godwit.yaml when path is empty, and applies GODWIT_* env overrides on top.
func Load(path string) (Config, error) {
	cfg := Defaults()
	if path == "" {
		wd, err := getwd()
		if err != nil {
			return Config{}, err
		}
		path = find(wd)
	}
	if path != "" {
		if err := cfg.readFile(path); err != nil {
			return Config{}, err
		}
	}
	if err := cfg.applyEnv(); err != nil {
		return Config{}, err
	}
	cfg.applySourceEnv()
	if err := cfg.validate(); err != nil {
		return Config{}, err
	}

	return cfg, nil
}

func find(dir string) string {
	for {
		candidate := filepath.Join(dir, FileName)
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			return ""
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

func (c *Config) readFile(path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(c); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	if !filepath.IsAbs(c.Dir) {
		c.Dir = filepath.Join(filepath.Dir(path), c.Dir)
	}
	if s := c.SchemaSource; s != nil && s.Path != "" && !filepath.IsAbs(s.Path) {
		s.Path = filepath.Join(filepath.Dir(path), s.Path)
	}

	return nil
}

func (c *Config) applyEnv() error {
	for _, e := range []struct {
		key string
		dst *string
	}{
		{"GODWIT_DIR", &c.Dir}, {"GODWIT_TARGET", &c.Target}, {"GODWIT_ROLLOUT", &c.Rollout}, {"GODWIT_SERVER", &c.Server},
	} {
		if v := os.Getenv(e.key); v != "" {
			*e.dst = v
		}
	}
	for _, e := range []struct {
		key string
		dst *time.Duration
	}{
		{"GODWIT_LOCK_TIMEOUT", &c.LockTimeout}, {"GODWIT_STATEMENT_TIMEOUT", &c.StatementTimeout},
	} {
		v := os.Getenv(e.key)
		if v == "" {
			continue
		}
		d, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("%s: %w", e.key, err)
		}
		*e.dst = d
	}
	if v := os.Getenv("GODWIT_ALLOW_OUT_OF_ORDER"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("GODWIT_ALLOW_OUT_OF_ORDER: %w", err)
		}
		c.AllowOutOfOrder = b
	}

	return nil
}

func (c *Config) applySourceEnv() {
	kind, path, bin := os.Getenv("GODWIT_SCHEMA_SOURCE_KIND"), os.Getenv("GODWIT_SCHEMA_SOURCE_PATH"), os.Getenv("GODWIT_SCHEMA_SOURCE_BIN")
	if kind == "" && path == "" && bin == "" {
		return
	}
	if c.SchemaSource == nil {
		c.SchemaSource = &SchemaSource{}
	}
	for _, e := range []struct {
		value string
		dst   *string
	}{
		{kind, &c.SchemaSource.Kind}, {path, &c.SchemaSource.Path}, {bin, &c.SchemaSource.Bin},
	} {
		if e.value != "" {
			*e.dst = e.value
		}
	}
}

func (c *Config) validate() error {
	if c.SchemaSource == nil {
		return nil
	}
	if !slices.Contains(sourceKinds, c.SchemaSource.Kind) {
		return fmt.Errorf("schema_source.kind %q is not one of %s", c.SchemaSource.Kind, strings.Join(sourceKinds, ", "))
	}

	return nil
}
