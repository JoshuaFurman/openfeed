package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// useTempXDG isolates a test from the developer's real configuration.
func useTempXDG(t *testing.T) {
	t.Helper()
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(root, "data"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
}

// writeConfig installs contents as the config file for the current test.
func writeConfig(t *testing.T, contents string) {
	t.Helper()
	p, err := resolvePaths()
	if err != nil {
		t.Fatalf("resolvePaths: %v", err)
	}
	if err := os.WriteFile(filepath.Join(p.config, "config.toml"), []byte(contents), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
}

func TestLoadFirstRunWritesStarterConfig(t *testing.T) {
	useTempXDG(t)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Feeds) != 0 {
		t.Errorf("starter config subscribes to %d feeds, want 0", len(cfg.Feeds))
	}
	if _, err := os.Stat(cfg.FilePath()); err != nil {
		t.Errorf("starter config not written: %v", err)
	}

	// A second Load must observe the file rather than overwrite it.
	writeConfig(t, "[[feed]]\nurl = \"https://example.com/feed.xml\"\n")
	again, err := Load()
	if err != nil {
		t.Fatalf("second Load: %v", err)
	}
	if len(again.Feeds) != 1 {
		t.Errorf("got %d feeds after edit, want 1", len(again.Feeds))
	}
}

func TestPathsFollowXDGVariables(t *testing.T) {
	useTempXDG(t)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for _, tc := range []struct {
		name string
		got  string
		env  string
	}{
		{"config", cfg.FilePath(), "XDG_CONFIG_HOME"},
		{"database", cfg.DatabasePath(), "XDG_DATA_HOME"},
		{"log", cfg.LogPath(), "XDG_STATE_HOME"},
	} {
		want := filepath.Join(os.Getenv(tc.env), appName)
		if filepath.Dir(tc.got) != want {
			t.Errorf("%s path %q, want it under %q", tc.name, tc.got, want)
		}
	}
}

// A relative XDG value must be ignored, per the specification.
func TestRelativeXDGValueFallsBackToHome(t *testing.T) {
	useTempXDG(t)
	t.Setenv("XDG_CONFIG_HOME", "relative/path")

	home := t.TempDir()
	// os.UserHomeDir uses HOME on Unix, USERPROFILE on Windows, and home on Plan 9.
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("home", home)

	p, err := resolvePaths()
	if err != nil {
		t.Fatalf("resolvePaths: %v", err)
	}
	if want := filepath.Join(home, ".config", appName); p.config != want {
		t.Errorf("config dir %q, want %q", p.config, want)
	}
}

func TestLoadRejectsBadConfigs(t *testing.T) {
	tests := []struct {
		name     string
		contents string
		wantErr  string
	}{
		{
			name:     "unknown key",
			contents: "[[feed]]\nurl = \"https://example.com/f.xml\"\nnmae = \"typo\"\n",
			wantErr:  "unknown setting",
		},
		{
			name:     "missing url",
			contents: "[[feed]]\nname = \"nameless\"\n",
			wantErr:  "missing url",
		},
		{
			name:     "unsupported scheme",
			contents: "[[feed]]\nurl = \"ftp://example.com/f.xml\"\n",
			wantErr:  "scheme must be http or https",
		},
		{
			name:     "missing host",
			contents: "[[feed]]\nurl = \"https:///feed.xml\"\n",
			wantErr:  "missing host",
		},
		{
			name: "duplicate feed",
			contents: "[[feed]]\nurl = \"https://example.com/f.xml\"\n" +
				"[[feed]]\nurl = \"https://example.com/f.xml\"\n",
			wantErr: "duplicates feed 1",
		},
		{
			name: "duplicate differing only in host case",
			contents: "[[feed]]\nurl = \"https://example.com/f.xml\"\n" +
				"[[feed]]\nurl = \"https://EXAMPLE.com/f.xml\"\n",
			wantErr: "duplicates feed 1",
		},
		{
			name:     "malformed toml",
			contents: "[[feed]\nurl = \"https://example.com/f.xml\"\n",
			wantErr:  "parse",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			useTempXDG(t)
			writeConfig(t, tc.contents)

			_, err := Load()
			if err == nil {
				t.Fatalf("Load succeeded, want error containing %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("Load error = %v, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

// An unnamed feed is labelled by its host, which appears beside every post.
func TestNormalizeDerivesNameFromHost(t *testing.T) {
	tests := []struct {
		name string
		url  string
		want string
	}{
		{"strips www", "https://www.theverge.com/rss/index.xml", "theverge.com"},
		{"bare host", "https://lobste.rs/rss", "lobste.rs"},
		{"keeps meaningful subdomain", "https://feeds.arstechnica.com/ars/index", "feeds.arstechnica.com"},
		{"lowercases host", "https://WWW.Example.COM/feed.xml", "example.com"},
		{"keeps port", "http://localhost:8080/feed.xml", "localhost:8080"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := Feed{URL: tc.url}
			if err := f.normalize(); err != nil {
				t.Fatalf("normalize: %v", err)
			}
			if f.Name != tc.want {
				t.Errorf("Name = %q, want %q", f.Name, tc.want)
			}
		})
	}
}

func TestNormalizeKeepsExplicitName(t *testing.T) {
	useTempXDG(t)
	writeConfig(t, "[[feed]]\nurl = \"https://lobste.rs/rss\"\nname = \"lobsters\"\ntags = [\"tech\"]\n")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	got := cfg.Feeds[0]
	if got.Name != "lobsters" {
		t.Errorf("Name = %q, want %q", got.Name, "lobsters")
	}
	if len(got.Tags) != 1 || got.Tags[0] != "tech" {
		t.Errorf("Tags = %v, want [tech]", got.Tags)
	}
}
