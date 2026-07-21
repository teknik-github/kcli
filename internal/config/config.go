// Package config loads kcli's optional user config file. Everything is
// best-effort: a missing or malformed file yields defaults, never an error that
// blocks startup.
package config

import (
	"os"
	"path/filepath"
	"strings"
	"time"

	"sigs.k8s.io/yaml"
)

// DefaultRefresh is the fallback auto-refresh cadence when none is configured.
const DefaultRefresh = 3 * time.Second

// DefaultAccent is the fallback accent colour (tview colour name).
const DefaultAccent = "aqua"

// Config mirrors the on-disk YAML. Fields use JSON tags because sigs.k8s.io/yaml
// converts YAML to JSON before unmarshalling.
type Config struct {
	Namespace       string            `json:"namespace"`       // startup namespace; "" = all
	RefreshInterval string            `json:"refreshInterval"` // e.g. "5s"; parsed leniently
	Theme           string            `json:"theme"`           // accent colour name (e.g. "green")
	Aliases         map[string]string `json:"aliases"`         // custom :jump aliases -> resource name
}

// Path returns the config file path: $KCLI_CONFIG, else
// $XDG_CONFIG_HOME/kcli/config.yaml, else ~/.config/kcli/config.yaml.
func Path() string {
	if p := os.Getenv("KCLI_CONFIG"); p != "" {
		return p
	}
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "kcli", "config.yaml")
}

// Load reads the config file, returning defaults when it is absent or invalid.
// The bool reports whether a file was actually loaded (for a startup notice).
func Load() (*Config, bool) {
	c := &Config{}
	path := Path()
	if path == "" {
		return c, false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return c, false // no file (or unreadable): defaults
	}
	if err := yaml.Unmarshal(data, c); err != nil {
		return &Config{}, false // malformed: fall back to defaults, don't block startup
	}
	return c, true
}

// Refresh returns the configured cadence, clamped to a sane floor, or the
// default when unset/unparseable.
func (c *Config) Refresh() time.Duration {
	if c.RefreshInterval == "" {
		return DefaultRefresh
	}
	d, err := time.ParseDuration(c.RefreshInterval)
	if err != nil || d < time.Second {
		return DefaultRefresh
	}
	return d
}

// Accent returns the configured accent colour name, or the default.
func (c *Config) Accent() string {
	if s := strings.TrimSpace(c.Theme); s != "" {
		return s
	}
	return DefaultAccent
}

// NormalizedAliases returns the custom aliases lower-cased on both sides, ready
// for a direct lookup. Nil-safe.
func (c *Config) NormalizedAliases() map[string]string {
	if len(c.Aliases) == 0 {
		return nil
	}
	out := make(map[string]string, len(c.Aliases))
	for k, v := range c.Aliases {
		out[strings.ToLower(strings.TrimSpace(k))] = strings.ToLower(strings.TrimSpace(v))
	}
	return out
}
