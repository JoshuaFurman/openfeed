package config

import (
	"fmt"
	"os"
	"path/filepath"
)

// appName is the subdirectory created inside each XDG base directory.
const appName = "openfeed"

// paths holds openfeed's resolved XDG base directories.
//
// Unexported: callers ask Config where a file lives, never which base
// directory it belongs in. That mapping is this package's alone.
type paths struct {
	config string // $XDG_CONFIG_HOME/openfeed
	data   string // $XDG_DATA_HOME/openfeed
	state  string // $XDG_STATE_HOME/openfeed
}

// resolvePaths locates openfeed's directories, creating any that are missing.
//
// XDG applies on every platform, including macOS, so that dotfiles behave
// identically across a user's machines.
func resolvePaths() (paths, error) {
	// Deferred, not returned: a fully configured environment should not need a
	// discoverable home directory.
	home, homeErr := os.UserHomeDir()

	base := func(env string, fallback ...string) (string, error) {
		// A relative XDG value counts as unset. IsAbs also rejects "".
		if dir := os.Getenv(env); filepath.IsAbs(dir) {
			return filepath.Join(dir, appName), nil
		}
		if homeErr != nil {
			return "", fmt.Errorf("%s is unset and the home directory is unknown: %w", env, homeErr)
		}
		parts := append([]string{home}, fallback...)
		return filepath.Join(append(parts, appName)...), nil
	}

	var (
		p   paths
		err error
	)
	if p.config, err = base("XDG_CONFIG_HOME", ".config"); err != nil {
		return paths{}, err
	}
	if p.data, err = base("XDG_DATA_HOME", ".local", "share"); err != nil {
		return paths{}, err
	}
	if p.state, err = base("XDG_STATE_HOME", ".local", "state"); err != nil {
		return paths{}, err
	}

	// 0700 per XDG: these may hold reading history and, later, credentials.
	for _, dir := range []string{p.config, p.data, p.state} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return paths{}, fmt.Errorf("create %s: %w", dir, err)
		}
	}
	return p, nil
}
