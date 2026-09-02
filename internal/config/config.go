// Package config loads the godwit.yaml project file with env overrides.
package config

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"
)

// FileName is the project file looked up from the working directory upwards.
const FileName = "godwit.yaml"

// Config holds the per-project defaults shared by every CLI command.
type Config struct {
	Dir              string        `yaml:"dir"`
	Target           string        `yaml:"target"`
	Rollout          string        `yaml:"rollout"`
	Server           string        `yaml:"server"`
	LockTimeout      time.Duration `yaml:"lock_timeout"`
	StatementTimeout time.Duration `yaml:"statement_timeout"`
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

	return nil
}
