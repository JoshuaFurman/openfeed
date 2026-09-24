# openfeed

A TUI-first RSS reader in Go. The reading *feel* of a social timeline; the sources are yours.

[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)
[![Go](https://img.shields.io/badge/Go-1.26-00ADD8.svg)](go.mod)

> **Status: early. Not yet usable as a reader.**
> Configuration loading, feed fetching, and database lifecycle/migrations work; post persistence and the timeline UI do not exist yet.
> The CLI still only prints resolved configuration. [`PLAN.md`](PLAN.md) tracks what is left for v0.1.

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
- **The database is a cache.** `rm openfeed.db` loses cached posts and your reading position —
  never a subscription.

Later, the same core gets a web face (`openfeed serve`, localhost, htmx) and optional AI
(daily digest, article summaries). openfeed is designed to be fully usable with AI turned off.

## Install

Requires Go 1.26 or newer.

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

## Files

openfeed follows XDG conventions **on every platform, including macOS** — you get
`~/.config/openfeed`, not `~/Library/Application Support`.

| Path | Contents |
|---|---|
| `$XDG_CONFIG_HOME/openfeed/config.toml` | **Intent.** Your subscriptions. Hand-edited, git-able. |
| `$XDG_DATA_HOME/openfeed/openfeed.db` | **Derived.** Cached posts, extracted text, reading position. |
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
