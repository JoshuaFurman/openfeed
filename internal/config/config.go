// Package config loads openfeed's settings and locates the files it reads and
// writes.
//
// config.toml is the source of truth for subscriptions. The database never owns
// one; deleting it discards cached posts and the saved reading position.
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

// Config is openfeed's resolved configuration. Construct it with Load; the
// zero value is not usable.
type Config struct {
	// Feeds are in config.toml order. The timeline is chronological and
	// ignores it.
	Feeds []Feed `toml:"feed"`

	paths paths
}

// starterConfig is written on first run: valid TOML subscribing to nothing, so
// a fresh install launches to an empty timeline instead of an error.
const starterConfig = `# openfeed configuration
#
# This file is the source of truth for what you read. Deleting openfeed's
# database never unsubscribes you, but it does discard cached posts and your
# saved reading position.
#
# Subscribe by adding a [[feed]] block pointing at any RSS or Atom URL.
# "name" and "tags" are optional.

# [[feed]]
# url  = "https://lobste.rs/rss"
# name = "lobsters"
# tags = ["tech"]
`

// Load reads and validates config.toml, writing a commented starter file if
// none exists.
//
// One malformed subscription fails the whole load: skipping it would silently
// unsubscribe the user from a feed they believe they are reading.
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

	if cfg.Feeds, err = parse(cfg.FilePath(), data); err != nil {
		return nil, err
	}
	return cfg, nil
}

// Add appends feed to config.toml as a new [[feed]] block, leaving the rest of the file as written,
// and returns it as Load would resolve it. Nothing is written unless the file still loads with it.
//
// Add rereads the file, so it catches a duplicate added by hand since Load; it does not change c.Feeds.
func (c *Config) Add(feed Feed) (Feed, error) {
	block, err := encodeFeed(feed)
	if err != nil {
		return Feed{}, err
	}
	data, err := os.ReadFile(c.FilePath())
	var addition string
	switch {
	case errors.Is(err, os.ErrNotExist):
		addition = starterConfig + "\n" + block
	case err != nil:
		return Feed{}, fmt.Errorf("read %s: %w", c.FilePath(), err)
	case len(data) == 0:
		addition = block
	case data[len(data)-1] == '\n':
		addition = "\n" + block
	default:
		addition = "\n\n" + block // The file's last line is unterminated.
	}
	feeds, err := parse(c.FilePath(), append(data, addition...))
	if err != nil {
		return Feed{}, err
	}

	// Appending rather than replacing keeps symlinks, permissions, and concurrent appends intact.
	file, err := os.OpenFile(c.FilePath(), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return Feed{}, fmt.Errorf("open %s: %w", c.FilePath(), err)
	}
	_, err = file.WriteString(addition)
	if err == nil {
		err = file.Sync()
	}
	if err = errors.Join(err, file.Close()); err != nil {
		return Feed{}, fmt.Errorf("write %s: %w", c.FilePath(), err)
	}
	return feeds[len(feeds)-1], nil
}

// encodeFeed renders feed as a TOML block, omitting a blank name so the host fallback stays implicit.
func encodeFeed(feed Feed) (string, error) {
	type block struct {
		URL  string   `toml:"url"`
		Name string   `toml:"name,omitempty"`
		Tags []string `toml:"tags,omitempty"`
	}
	explicitName := strings.TrimSpace(feed.Name)
	if err := feed.Normalize(); err != nil {
		return "", err
	}
	var out strings.Builder
	enc := toml.NewEncoder(&out)
	enc.Indent = ""
	err := enc.Encode(struct {
		Feed []block `toml:"feed"`
	}{[]block{{URL: feed.URL, Name: explicitName, Tags: feed.Tags}}})
	if err != nil {
		return "", fmt.Errorf("encode feed: %w", err)
	}
	return out.String(), nil
}

// parse decodes and validates config.toml's contents; path only labels errors.
func parse(path string, data []byte) ([]Feed, error) {
	var cfg Config
	md, err := toml.Decode(string(data), &cfg)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	// Unknown keys are almost always typos in a hand-edited file.
	if unknown := md.Undecoded(); len(unknown) > 0 {
		return nil, fmt.Errorf("%s: unknown setting %q", path, unknown[0].String())
	}
	if err := cfg.normalize(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return cfg.Feeds, nil
}

// FilePath reports where config.toml lives, whether or not it exists yet.
func (c *Config) FilePath() string { return filepath.Join(c.paths.config, "config.toml") }

// DatabasePath reports where cached posts and the saved reading position are
// stored. Deleting that file loses both, but no subscriptions.
func (c *Config) DatabasePath() string { return filepath.Join(c.paths.data, "openfeed.db") }

// LogPath reports where openfeed writes its log.
func (c *Config) LogPath() string { return filepath.Join(c.paths.state, "openfeed.log") }

// normalize validates every subscription and rejects duplicates. Errors name a
// feed by 1-based position, the only identifier a [[feed]] block has.
func (c *Config) normalize() error {
	seenAt := make(map[string]int, len(c.Feeds))
	for i := range c.Feeds {
		if err := c.Feeds[i].Normalize(); err != nil {
			return fmt.Errorf("feed %d: %w", i+1, err)
		}
		if first, dup := seenAt[c.Feeds[i].URL]; dup {
			return fmt.Errorf("feed %d duplicates feed %d (%s)", i+1, first, c.Feeds[i].URL)
		}
		seenAt[c.Feeds[i].URL] = i + 1
	}
	return nil
}

// Normalize validates one subscription as Load does and fills in derived defaults.
// It never touches the network; openfeed has to start offline.
func (f *Feed) Normalize() error {
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
	if u.Hostname() == "" {
		return fmt.Errorf("url %q: missing host", f.URL)
	}
	// url.Parse preserves host case; DNS ignores it. Lowering makes duplicate
	// detection reliable. Paths are left alone — they are case-sensitive.
	u.Host = strings.ToLower(u.Host)
	f.URL = u.String()

	f.Name = strings.TrimSpace(f.Name)
	if f.Name == "" {
		f.Name = strings.TrimPrefix(u.Host, "www.")
	}
	return nil
}
