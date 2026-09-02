// Package config loads openfeed's user-owned settings and locates the files
// openfeed reads and writes.
//
// The configuration file is the source of truth for intent: which feeds the
// user subscribes to. Everything openfeed stores in its database is derived
// from that intent and may be deleted at any time without loss.
package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

// Feed is one subscription, as declared by a [[feed]] block in config.toml.
type Feed struct {
	URL  string   `toml:"url"`
	Name string   `toml:"name"`
	Tags []string `toml:"tags"`
}

// Config is openfeed's resolved configuration.
//
// Construct it with Load; the zero value is not usable.
type Config struct {
	// Feeds are in the order they appear in config.toml. Reading order in the
	// timeline is chronological and unrelated to this ordering.
	Feeds []Feed `toml:"feed"`

	paths paths
}

// starterConfig is written on first run. It is valid TOML that subscribes to
// nothing, so a first launch succeeds and shows an empty timeline rather than
// failing.
const starterConfig = `# openfeed configuration
#
# This file is the source of truth for what you read. openfeed's database is
# derived from it and can be deleted at any time without losing anything.
#
# Subscribe by adding a [[feed]] block pointing at any RSS or Atom URL.
# "name" and "tags" are optional.

# [[feed]]
# url  = "https://lobste.rs/rss"
# name = "lobsters"
# tags = ["tech"]
`

// Load reads and validates config.toml.
//
// A missing configuration file is not an error. On first run Load writes a
// commented starter config and returns it, so that openfeed is runnable
// immediately after installation.
//
// Load rejects the whole file if any subscription is malformed. Skipping a bad
// entry would mean silently unsubscribing the user from a feed they believe
// they are reading; a startup error naming the offending block is easier to act
// on than posts that never arrive.
func Load() (*Config, error) {
	p, err := resolvePaths()
	if err != nil {
		return nil, err
	}
	cfg := &Config{paths: p}

	data, err := os.ReadFile(cfg.FilePath())
	switch {
	case errors.Is(err, os.ErrNotExist):
		if err := os.WriteFile(cfg.FilePath(), []byte(starterConfig), 0o600); err != nil {
			return nil, fmt.Errorf("write starter config: %w", err)
		}
		data = []byte(starterConfig)
	case err != nil:
		return nil, fmt.Errorf("read %s: %w", cfg.FilePath(), err)
	}

	md, err := toml.Decode(string(data), cfg)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", cfg.FilePath(), err)
	}
	// Unknown keys are usually typos in a hand-edited file. Reporting them
	// beats letting a misspelled setting be silently ignored.
	if unknown := md.Undecoded(); len(unknown) > 0 {
		return nil, fmt.Errorf("%s: unknown setting %q", cfg.FilePath(), unknown[0].String())
	}
	if err := cfg.normalize(); err != nil {
		return nil, fmt.Errorf("%s: %w", cfg.FilePath(), err)
	}
	return cfg, nil
}

// FilePath reports where config.toml lives. It is valid even when the file
// does not exist yet, so callers can name it in messages.
func (c *Config) FilePath() string { return filepath.Join(c.paths.config, "config.toml") }

// DatabasePath reports where openfeed's derived state is stored. Deleting this
// file discards cached posts and reading position, but no subscriptions.
func (c *Config) DatabasePath() string { return filepath.Join(c.paths.data, "openfeed.db") }

// LogPath reports where openfeed writes its log.
func (c *Config) LogPath() string { return filepath.Join(c.paths.state, "openfeed.log") }

// normalize validates every subscription and rejects duplicates.
//
// Feeds are reported by their 1-based position because a [[feed]] block has no
// stable identifier until it has been normalized.
func (c *Config) normalize() error {
	seenAt := make(map[string]int, len(c.Feeds))
	for i := range c.Feeds {
		if err := c.Feeds[i].normalize(); err != nil {
			return fmt.Errorf("feed %d: %w", i+1, err)
		}
		if first, dup := seenAt[c.Feeds[i].URL]; dup {
			return fmt.Errorf("feed %d duplicates feed %d (%s)", i+1, first, c.Feeds[i].URL)
		}
		seenAt[c.Feeds[i].URL] = i + 1
	}
	return nil
}

// normalize validates one subscription and fills in any derived defaults.
//
// It runs at load time and must not perform network access: openfeed has to
// start correctly while offline.
func (f *Feed) normalize() error {
	f.URL = strings.TrimSpace(f.URL)
	if f.URL == "" {
		return errors.New("missing url")
	}
	u, err := url.Parse(f.URL)
	if err != nil {
		return fmt.Errorf("invalid url %q: %w", f.URL, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("url %q: scheme must be http or https", f.URL)
	}
	if u.Host == "" {
		return fmt.Errorf("url %q: missing host", f.URL)
	}
	// url.Parse preserves the host's case, but DNS does not distinguish it.
	// Lowering it here makes duplicate detection reliable and lets any later
	// derivation match on a predictable host. The path is left untouched
	// because it is genuinely case-sensitive.
	u.Host = strings.ToLower(u.Host)
	f.URL = u.String()

	f.Name = strings.TrimSpace(f.Name)
	if f.Name == "" {
		f.Name = strings.TrimPrefix(u.Host, "www.")
	}
	return nil
}
