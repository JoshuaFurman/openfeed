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
  grep, diff, and commit. Nothing is algorithmic unless you ask for it.
- **Subscriptions never live in the database.** `rm openfeed.db` loses cached posts and your
  reading position — never a subscription.

Later, the same core gets a web face (`openfeed serve`, localhost, htmx) and optional AI
(daily digest, article summaries). openfeed is designed to be fully usable with AI turned off.

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

The first run writes a commented starter config and exits cleanly — openfeed is runnable
immediately after installation, with nothing to configure first.

Subscribe by editing that file:

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

| Key | Action |
|---|---|
| `j` / `k` or ↓ / ↑ | Move through the river; older posts load as you scroll |
| `k` `k` (or ↑ ↑) at the newest post | Refresh |
| `g` or Home | Jump to the newest post |
| `o` or Enter | Open the post's article in your browser |
| `q` or Ctrl-C | Quit |
| Ctrl-Z | Suspend to the shell (`fg` returns) |

## Not yet

v0.1 is deliberately small. It does not have:

- a reader pane: `o` opens the article in your browser;
- subscription commands or OPML import: edit `config.toml`;
- search, feed or tag filters, the web UI, or AI features;
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
