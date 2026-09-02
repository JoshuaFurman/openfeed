package config

import (
	"fmt"
	"os"
	"path/filepath"
)

// appName is the per-application subdirectory created inside each XDG base
// directory.
const appName = "openfeed"

// paths holds openfeed's resolved XDG base directories.
//
// It is deliberately unexported. Callers ask Config where a particular file
// lives (DatabasePath, LogPath) rather than learning which XDG base directory
// that file belongs in — that mapping is a decision this package owns alone.
type paths struct {
	config string // $XDG_CONFIG_HOME/openfeed
	data   string // $XDG_DATA_HOME/openfeed
	state  string // $XDG_STATE_HOME/openfeed
}

// resolvePaths locates openfeed's directories, creating any that do not exist.
//
// The XDG Base Directory specification is applied on every platform, including
// macOS, where the OS convention would instead be ~/Library/Application
// Support. openfeed prefers ~/.config everywhere so that a user's dotfiles
// behave identically across their machines.
func resolvePaths() (paths, error) {
	// The home directory is only needed when an XDG variable is unset, so its
	// failure is deferred rather than returned: a fully configured environment
	// should not require a discoverable home directory.
	home, homeErr := os.UserHomeDir()

	base := func(env string, fallback ...string) (string, error) {
		// An XDG variable holding a relative path must be treated as unset.
		// filepath.IsAbs also rejects "", so the unset case needs no separate
		// test.
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

	// 0700 per the XDG specification: these directories may hold reading
	// history and, later, API credentials.
	for _, dir := range []string{p.config, p.data, p.state} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return paths{}, fmt.Errorf("create %s: %w", dir, err)
		}
	}
	return p, nil
}
