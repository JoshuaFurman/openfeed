package config

import (
	"os"
	"path/filepath"
	"reflect"
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
			name:     "port without hostname",
			contents: "[[feed]]\nurl = \"https://:8080/feed.xml\"\n",
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
			if err := f.Normalize(); err != nil {
				t.Fatalf("Normalize: %v", err)
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

// loadAndRead loads the current test's config and returns its file's bytes.
func loadAndRead(t *testing.T) (*Config, string) {
	t.Helper()
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	data, err := os.ReadFile(cfg.FilePath())
	if err != nil {
		t.Fatal(err)
	}
	return cfg, string(data)
}

func TestAddAppendsWithoutRewriting(t *testing.T) {
	tests := []struct {
		name     string
		existing string
		feed     Feed
		want     Feed
		appended string
	}{
		{
			name:     "after comments, name left implicit",
			existing: "# my feeds\n[[feed]]\nurl  = \"https://a.example/feed\" # aligned\n",
			feed:     Feed{URL: "  https://WWW.B.example/rss  ", Name: "  "},
			want:     Feed{URL: "https://www.b.example/rss", Name: "b.example"},
			appended: "\n[[feed]]\nurl = \"https://www.b.example/rss\"\n",
		},
		{
			name:     "unterminated last line",
			existing: "[[feed]]\nurl = \"https://a.example/feed\"",
			feed:     Feed{URL: "https://b.example/rss", Name: " bee ", Tags: []string{"tech", "daily"}},
			want:     Feed{URL: "https://b.example/rss", Name: "bee", Tags: []string{"tech", "daily"}},
			appended: "\n\n[[feed]]\nurl = \"https://b.example/rss\"\nname = \"bee\"\ntags = [\"tech\", \"daily\"]\n",
		},
		{
			name:     "empty file",
			feed:     Feed{URL: "https://b.example/rss", Name: `quote " and \ back`},
			want:     Feed{URL: "https://b.example/rss", Name: `quote " and \ back`},
			appended: "[[feed]]\nurl = \"https://b.example/rss\"\nname = \"quote \\\" and \\\\ back\"\n",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			useTempXDG(t)
			writeConfig(t, tc.existing)
			cfg, _ := loadAndRead(t)
			before := len(cfg.Feeds)

			got, err := cfg.Add(tc.feed)
			if err != nil {
				t.Fatalf("Add: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("Add = %+v, want %+v", got, tc.want)
			}
			if len(cfg.Feeds) != before {
				t.Errorf("Add changed the loaded feeds to %v", cfg.Feeds)
			}
			reloaded, data := loadAndRead(t)
			if data != tc.existing+tc.appended {
				t.Errorf("file = %q, want the original followed by %q", data, tc.appended)
			}
			if last := reloaded.Feeds[len(reloaded.Feeds)-1]; !reflect.DeepEqual(last, tc.want) {
				t.Errorf("reloaded feed = %+v, want %+v", last, tc.want)
			}
		})
	}
}

func TestAddRejectsWithoutWriting(t *testing.T) {
	const existing = "[[feed]]\nurl = \"https://a.example/feed\"\n"
	tests := []struct {
		name     string
		existing string
		feed     Feed
		wantErr  string
	}{
		{"missing url", existing, Feed{Name: "nameless"}, "missing url"},
		{"unsupported scheme", existing, Feed{URL: "ftp://b.example/rss"}, "scheme must be http or https"},
		{"duplicate differing in host case", existing, Feed{URL: "https://A.example/feed"}, "feed 2 duplicates feed 1"},
		{"file broken since launch", existing + "[[feed]\n", Feed{URL: "https://b.example/rss"}, "parse"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			useTempXDG(t)
			writeConfig(t, existing)
			cfg, _ := loadAndRead(t)
			writeConfig(t, tc.existing)

			if _, err := cfg.Add(tc.feed); err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("Add error = %v, want it to contain %q", err, tc.wantErr)
			}
			if data, err := os.ReadFile(cfg.FilePath()); err != nil || string(data) != tc.existing {
				t.Errorf("file = %q, %v; want it unchanged", data, err)
			}
		})
	}
}

// A hand-added duplicate is caught even though it postdates Load.
func TestAddRereadsTheFile(t *testing.T) {
	useTempXDG(t)
	cfg, _ := loadAndRead(t)
	writeConfig(t, "[[feed]]\nurl = \"https://b.example/rss\"\n")
	if _, err := cfg.Add(Feed{URL: "https://b.example/rss"}); err == nil || !strings.Contains(err.Error(), "duplicates") {
		t.Errorf("Add error = %v, want a duplicate", err)
	}
}

func TestAddRecreatesAMissingFile(t *testing.T) {
	useTempXDG(t)
	cfg, _ := loadAndRead(t)
	if err := os.Remove(cfg.FilePath()); err != nil {
		t.Fatal(err)
	}
	if _, err := cfg.Add(Feed{URL: "https://b.example/rss"}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	_, data := loadAndRead(t)
	if want := starterConfig + "\n[[feed]]\nurl = \"https://b.example/rss\"\n"; data != want {
		t.Errorf("file = %q, want the starter config and the feed", data)
	}
	if info, err := os.Stat(cfg.FilePath()); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("recreated file mode = %v, %v; want 0600", info.Mode(), err)
	}
}

// Dotfile managers often link config.toml into a repository; adding must write through the link.
func TestAddKeepsASymlink(t *testing.T) {
	useTempXDG(t)
	cfg, _ := loadAndRead(t)
	target := filepath.Join(t.TempDir(), "dotfiles-config.toml")
	if err := os.WriteFile(target, []byte("# tracked\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(cfg.FilePath()); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, cfg.FilePath()); err != nil {
		t.Fatal(err)
	}

	if _, err := cfg.Add(Feed{URL: "https://b.example/rss"}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if info, err := os.Lstat(cfg.FilePath()); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("config.toml is no longer a symlink: %v, %v", info.Mode(), err)
	}
	data, err := os.ReadFile(target)
	if err != nil || string(data) != "# tracked\n\n[[feed]]\nurl = \"https://b.example/rss\"\n" {
		t.Errorf("target = %q, %v", data, err)
	}
	if info, err := os.Stat(target); err != nil || info.Mode().Perm() != 0o640 {
		t.Errorf("target mode = %v, %v; want it unchanged", info.Mode(), err)
	}
}
