# openfeed

A TUI-first RSS reader in Go. The reading *feel* of a social timeline; the sources are yours.

[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)
[![Go](https://img.shields.io/badge/Go-1.26-00ADD8.svg)](go.mod)

> **Status: v0.1, a read-only river.** openfeed reads, refreshes, and remembers your place.
> It is early: expect rough edges and changes between versions. [`PLAN.md`](PLAN.md) tracks what comes next.

## Why

Most RSS readers are filing cabinets — folders, unread counts, an inbox to clear. openfeed is a
**river**: one merged chronological stream across every feed you subscribe to, scrolled in the
terminal, with no backlog guilt.

- **One view.** Feeds and tags are filters over the river, not separate destinations.
- **No unread counts.** A divider marks where you left off in the chronological river, not which
  posts arrived since your last visit. Nothing is bold, nothing is a badge, nothing needs clearing.
  Items age out of the top on their own.
- **Your sources, in a text file.** Subscriptions live in a hand-editable `config.toml` you can
  grep, diff, and commit; adding one in the app appends to it. Nothing is algorithmic unless you ask for it.
- **Subscriptions never live in the database.** `rm openfeed.db` loses cached posts and your
  reading position — never a subscription.

The same river has a mobile-first web face, `openfeed serve`, for reading on your phone. Optional
AI (daily digest, article summaries) comes later. openfeed is designed to be fully usable with AI turned off.

## Install

With Homebrew:

```sh
brew install --cask joshuafurman/tap/openfeed
```

Or download an archive for macOS, Linux, or Windows from the
[latest release](https://github.com/JoshuaFurman/openfeed/releases/latest). The binaries are not
signed, so on macOS a browser download needs `xattr -d com.apple.quarantine openfeed` before it runs.

Or with Go 1.26.6 or newer:

```sh
go install github.com/JoshuaFurman/openfeed/cmd/openfeed@latest
```

Or from a clone:

```sh
git clone https://github.com/JoshuaFurman/openfeed
cd openfeed
go build ./cmd/openfeed
```

There is no cgo anywhere in the dependency tree, so cross-compiling is just `GOOS`/`GOARCH`.
`openfeed --version` prints the version you have.

## Usage

```sh
openfeed
```

The first run writes a commented starter config and opens an empty river — openfeed is runnable
immediately after installation, with nothing to configure first.

Press `n` to subscribe. A popup asks for the feed's URL, an optional name, and optional
comma-separated tags. openfeed fetches the URL first and saves it only if it is a working RSS, Atom,
or JSON feed; otherwise the popup says why, so you can correct it. The new subscription's posts join
the river at once, and it is appended to `config.toml` as a `[[feed]]` block, leaving the rest of the
file, comments included, as you wrote it.

You can also subscribe by editing that file:

```toml
[[feed]]
url  = "https://lobste.rs/rss"
name = "lobsters"        # optional; defaults to the feed's host
tags = ["tech"]          # optional
```

A malformed subscription fails startup with the offending block named. That is deliberate:
silently skipping a bad entry would unsubscribe you from a feed you believe you are reading.

openfeed opens straight to the cached river where you left off: your last post sits just below a
"── You left off here ──" line, with newer posts above it. Read upward with `k` toward the newest post,
or press `g` to skip straight there. Your place is saved as you move (after a two-second pause, after
each refresh, and when you quit), so it survives restarts. The first time, the river opens at the
newest post, and everything already cached counts as history. Meanwhile, openfeed refreshes every
subscription in the background (at most two requests at a time per website). New posts appear above your cursor, which
stays on the post you were reading. To refresh again, double-tap `k` (or ↑) at the newest post; a
single press or a held key never refreshes. A feed fetched in
the last 30 seconds is not fetched again, but the pull still shows its spinner. A failing feed never
hides posts that are already cached; the status line says why it failed, and the log has the
details.

When any feed has tags, each post shows its feed's tags in their own column, each tag in a color of its
own, so you can pick out a topic at a glance. Set `NO_COLOR=1` to turn the colors off. Press `/` to
read only one tag's feeds. It lists your tags, with `Tech` and `tech` counting as one, and
the header shows the filter, as in `openfeed #tech`. The filter keeps your place in time, and Esc
returns you to where you were in the whole river. Reading in a filtered view never moves "You left off
here", because it passes over posts the filter hides.

| Key | Action |
|---|---|
| `j` / `k` or ↓ / ↑ | Move through the river; older posts load as you scroll |
| `k` `k` (or ↑ ↑) at the newest post | Refresh |
| `g` or Home | Jump to the newest post |
| `o` or Enter | Open the post's article in your browser |
| `n` | Add a feed: Enter subscribes, Tab or ↓ / ↑ change fields, Esc cancels |
| `/` | Filter by tag: `j` / `k` choose a tag or All posts, Enter applies, Esc cancels |
| Esc | Leave a tag filter for the whole river |
| `q` or Ctrl-C | Quit |
| Ctrl-Z | Suspend to the shell (`fg` returns) |

### On your phone

```sh
openfeed serve                  # every interface, port 80
openfeed serve --addr :8080     # another port
openfeed serve --addr 100.x.y.z:80   # only your Tailscale address
```

Open the printed address from your phone, over your home network or Tailscale. The page shows the
newest posts, loads older ones as you scroll, and taps open the article in a new tab. Each visit
refreshes in the background; when newer posts arrive, a "New posts ↑" button takes you to them, and
pulling down to reload does the same. A red dot on the bell means a feed is failing to refresh; tap it
to see which and why. The sun/moon button switches between light and dark, and each browser remembers
its choice; until you pick one, the page follows your device. The filter icon next to the bell shows
only one tag's feeds; the header then shows `#tag ×`, and tapping it returns to all posts. As in the
TUI, reading a filtered view never moves "You left off here".

There is **no login**: anyone who can reach the address can read your river and refresh it. Bind
`--addr` to your Tailscale address, or keep the port off untrusted networks. Some systems need
privileges for ports below 1024; use `--addr :8080` there.

The web page has no "You left off here" line. Opening it counts as reading up to the newest post it
showed, so the TUI picks up above what you read on the phone. Running `openfeed serve` and the TUI at
the same time is fine.

## Not yet

openfeed is deliberately small so far. It does not have:

- a reader pane: `o` opens the article in your browser;
- removing or renaming subscriptions in the app, or OPML import: edit `config.toml`;
- search, filtering by feed, or AI features;
- adding feeds on the web page: use the TUI;
- pruning: cached posts are never deleted, so the database grows with your subscriptions. Removing a
  feed from the config hides its posts without deleting them;
- log rotation: the log file grows too.

## Files

openfeed follows XDG conventions **on every platform, including macOS** — you get
`~/.config/openfeed`, not `~/Library/Application Support`.

| Path | Contents |
|---|---|
| `$XDG_CONFIG_HOME/openfeed/config.toml` | **Intent.** Your subscriptions. Hand-edited, git-able. |
| `$XDG_DATA_HOME/openfeed/openfeed.db` | **Local state.** Cached posts and your reading position. |
| `$XDG_STATE_HOME/openfeed/openfeed.log` | Logs. |

Defaults when those variables are unset: `~/.config`, `~/.local/share`, `~/.local/state`.

The database lives under `DATA` rather than `CACHE` because reading position is not regenerable
and system cleaners purge `CACHE`.

## Contributing

Contributions are welcome, but this is a small, opinionated codebase and design discussion comes
before code. Open an issue describing the problem before sending a PR for anything non-trivial.

[`AGENTS.md`](AGENTS.md) records settled design decisions, their reasoning, and engineering conventions.
[`PLAN.md`](PLAN.md) is the living roadmap: current status, next steps, open questions, and session handoff.
Read both before proposing anything structural.

Before submitting:

```sh
gofmt -l .        # must print nothing
go vet ./...
staticcheck ./...
go test ./...
```

`staticcheck` must be installed from source against the current toolchain — an older binary fails
with `unsupported version: 2` on Go 1.26 export data:

```sh
go install honnef.co/go/tools/cmd/staticcheck@latest
```

Commits follow [Conventional Commits](https://www.conventionalcommits.org/) and stay short:
`feat(store): ...`, `fix(feed): ...`, `docs: ...`. Unrelated changes get separate commits.
Tests never hit the network; parser tests use checked-in fixtures and golden results, and HTTP tests
use an in-memory transport.

## License

[MIT](LICENSE)
