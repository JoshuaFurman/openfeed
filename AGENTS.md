# openfeed

A TUI-first RSS reader in Go. The reading *feel* of a social timeline; the sources are yours.

---

## 1. Vision

Most RSS readers are filing cabinets — folders, unread counts, an inbox to clear. openfeed is a **river**: one merged chronological stream across every feed you subscribe to, scrolled in the terminal, with no backlog guilt. You control the sources completely. Nothing is algorithmic unless you ask for it.

Three faces, one core:

- **TUI** (primary) — Bubble Tea. Launch `openfeed`, scroll, read, move on.
- **Web** (later) — `openfeed serve` on localhost, for when a browser is genuinely easier to read in.
- **AI** (later) — daily digest, surfacing personally interesting posts, article summary/explanation. Always optional; openfeed is fully usable with it turned off.

Open source. Contributors should be able to read the code and understand it.

---

## 2. Decisions made

These are settled. Revisit only with a reason, and update this file when you do.

| Area | Decision | Why |
|---|---|---|
| **Navigation** | Unified river is *the* view. Feeds and tags are filters over it, not destinations. | Matches the timeline metaphor. AI ranking later becomes an alternate sort of the same river, not a new screen. |
| **Read state** | Seen-position in publication/fallback chronology, not unread counts or an arrival watermark. A divider marks that position; no bold/unbold, no badges. | No inbox-zero pressure. Late discoveries can fall below the divider; any discovery count must be separate from it. |
| **Storage** | SQLite via `modernc.org/sqlite` (pure Go, **no cgo**). `database/sql` directly, no ORM. Hand-written migrations as embedded `.sql`. | Buys FTS5 search, `ORDER BY score` for AI ranking, and safe concurrent access when `serve` runs alongside the TUI. Pure-Go driver keeps cross-compiled release binaries trivial. |
| **Store boundary** | One atomic operation will persist each successful refresh's items and validators, using store-owned inputs. The reader adapts fetched results; config retains subscription intent and labels. | Separate writes let callers advance validators without saving posts. No HTTP/parser types or caller-managed transactions belong in the store API. |
| **SQLite connections** | WAL, one connection per store, foreign keys enabled, and FULL synchronous durability. Retry busy lock acquisition for at most five seconds, respecting caller cancellation; never replay a partially executed transaction. | Small local operations do not need an in-process pool. WAL permits readers in another process during writes; Go-managed waits avoid a long, non-cancellable SQLite busy timeout. |
| **Migrations** | Numbered embedded SQL files and a `schema_migrations` ledger; all pending migrations and their version records commit in one exclusive transaction. Check/migrate before enabling WAL; reject newer schemas without changing them. | Exclusive locking prevents commit contention during concurrent first opens, before WAL exists; under WAL it is equivalent to immediate locking and permits readers. The first migration owns only bookkeeping; domain tables arrive with their reviewed behavior. |
| **Source of truth** | **Config is intent; the DB is cache.** Feeds are declared in hand-editable `config.toml`. The DB holds only derived state. | `rm openfeed.db` must lose nothing you care about. Subscriptions stay greppable and version-controllable. |
| **Item models** | `feed.Item` represents publisher data; persisted/timeline models will be separate and introduced with their owning packages. | Avoids leaking HTTP/parser concerns into presentation or database IDs into fetching. |
| **Feed labels** | Resolved config names are authoritative, including the hostname fallback. Fetched titles never rename subscriptions. | No naming-provenance flag or config changes needed for fetching. |
| **Item identity** | Within the configured feed URL: publisher ID → usable article URL → SHA-256 of title, feed content, and publication date when present. | Prefer stable identifiers; hashing is a best-effort fallback whose key can change after edits. Different subscriptions retain separate entries. |
| **Duplicate keys** | Within one response, the first normalized occurrence of a key wins as a whole item. Storage owns this rule; the reader preserves source order. | First and last position are both imperfect freshness signals. Respect source order rather than infer freshness from dates or merge contradictory entries. |
| **Publisher updates** | For an existing identity, a successfully persisted refresh replaces title, URL, content, and publication date with the winning entry, including empty/missing values. First-fetch time remains immutable. | Keeping selected historical fields invents a composite item and can conceal publisher corrections or removals. The cache reflects the accepted response, not an editorial history. |
| **Empty feed entries** | Skip entries with no title, feed content/description, or usable article URL after normalization. | An identifier or timestamp alone gives the reader nothing to display or open; partially populated entries remain usable. |
| **Undated items** | Fetching preserves missing/invalid publication dates; storage keeps them absent and uses immutable first-fetch time as the ordering fallback. Atom update dates are not publication dates. | Never hash fetch time, fabricate a publication date, or move a persistently undated post forward on each refresh. |
| **Timeline dates** | Order by the current publication date when present, otherwise by first-fetch time. Newly supplied, corrected, or removed publication dates can change an existing item's position. | Freezing the initial position would ignore corrections. Publication chronology and discovery are different facts; late discoveries belong at their publication date. |
| **Feed fetching** | A stateless client accepts/returns ETag and Last-Modified validators, with a 30-second deadline and 10 MiB body limit. | Storage owns persistence; validators must be committed with successfully fetched items, never before them. Limits bound stalled or oversized responses. |
| **Article text** | Feed-provided content in the river. Full text extracted **on demand** when an item is opened, then cached. | Eager extraction means hundreds of requests to other people's servers per refresh. On-demand is polite and keeps refresh fast. |
| **Web UI** | `html/template` + `//go:embed` + htmx. Stdlib `net/http` routing. | Single self-contained binary, no Node, no build step. Go 1.22+ pattern routing (`"GET /items/{id}"`) makes `gorilla/mux` unnecessary — **dropped from the original plan**. |
| **AI** | Define the `Provider` seam now with a no-op implementation. Build zero real providers until reading is solid. | Core never branches on "is AI on?" — it just holds a Provider. No speculative generality. |
| **Paths** | XDG conventions **on all platforms, including macOS.** Hand-rolled in `internal/config`, ~40 lines, no dependency. | Primary dev machine is darwin, where `adrg/xdg` would resolve to `~/Library/Application Support`. We want `~/.config/openfeed` everywhere. |

### Refresh item semantics

For one stored `(configured feed URL, item key)`, let `F₀` be the `FetchedAt` value from the
refresh whose insertion first commits successfully. Preserve it across updates and reopen;
it is not the minimum timestamp across subsequent refreshes. A rolled-back insertion establishes
no first-fetch state. `feed.Result.FetchedAt` is a UTC request-start timestamp, not a commit time
or a reliable arrival cursor.

Keep publisher publication time nullable and separate from `F₀`. The effective timeline date is
derived from those facts, not an independently frozen date:

| Publication-date transition | Effective timeline date |
|---|---|
| Missing → missing | `F₀`, never the latest fetch time |
| Missing → dated | Supplied publication date |
| Dated → corrected | Corrected publication date, earlier or later |
| Dated → missing/invalid | `F₀` |
| Missing after removal → dated again | Newly supplied publication date |

Clearing a date can move an older post forward once to its original first-fetch time; repeatedly
fetching it undated does not move it again. Do not retain a previous date when the winning entry
has none, or borrow a date/content field from a losing duplicate. An unchanged response (304)
does not mutate items or their dates; a failed refresh leaves all cached state untouched.

These are same-identity update rules, not similarity matching. If an edit changes a hash-derived
key, it produces a distinct identity with its own first-fetch time; different subscriptions also
remain independent.

A post published a week ago and first fetched today belongs a week back in the river, even below
a saved seen divider. The divider marks chronological position, not every arrival since a wall-clock
time. Any count attached to it must describe the chronological segment, not all newly discovered
items; exact cursor representation and advancement are separate decisions.

### Paths

```
$XDG_CONFIG_HOME/openfeed/config.toml    # intent — yours, hand-edited, git-able
$XDG_DATA_HOME/openfeed/openfeed.db      # derived — items, extracted text, seen position, summaries
$XDG_STATE_HOME/openfeed/openfeed.log    # logs
```

Defaults when unset: `~/.config`, `~/.local/share`, `~/.local/state`.

The DB lives in `DATA`, not `CACHE`, because seen-position is not regenerable and system cleaners purge `CACHE`.

### Config shape

```toml
[[feed]]
url  = "https://lobste.rs/rss"
name = "lobsters"
tags = ["tech"]
```

### Invariants established by `config.Load`

Downstream packages may rely on these and should not re-check them:

- Every `Feed.URL` is absolute, `http`/`https`, and has a host.
- Hosts are lowercased. `url.Parse` preserves case but DNS does not, so normalizing here is what makes duplicate detection and any later host comparison correct. Paths are left alone — they *are* case-sensitive.
- No two feeds share a URL.
- `Feed.Name` is never empty; it falls back to the host with any `www.` prefix stripped. No renderer needs a blank-label branch.
- A malformed subscription fails the whole load. Silently skipping one would unsubscribe the user from a feed they believe they are reading.

---

## 3. Design principles

From *A Philosophy of Software Design*, 2nd ed. (Ousterhout). Applied to this project specifically — not recited generically.

### Deep modules

**A module's interface should be much simpler than its implementation.** Depth is the whole game.

The most consequential interface in openfeed is the seam between *what the reader knows* and *how it's displayed*. Get it right and the web server is a weekend's work; get it wrong and you write the app twice. It should look roughly like:

```go
Timeline(ctx, Query) ([]Item, error)
Refresh(ctx) (RefreshResult, error)
Article(ctx, ItemID) (Article, error)
MarkSeen(ctx, time.Time) error
```

Four methods hiding conditional GET, ETag handling, GUID dedup, chronological merge, pagination, readability extraction, and caching. Both the TUI and the web server consume exactly this and nothing more.

Likewise `internal/store`: its interface speaks in **reader terms**, never SQL terms. If a `*sql.Rows`, a `WHERE` clause, or a driver error ever escapes that package, the abstraction has leaked and storage becomes unswappable.

### Pull complexity downward

Dedup, ordering, and pagination are genuinely hard. Push them into SQLite so the TUI never learns they were hard. It is better for the module implementer to suffer than the module's users.

### Different layer, different abstraction

Adjacent layers must not have similar interfaces. The TUI does not speak SQL. The store does not import Bubble Tea. If a method's body is one call to a lower layer with the same signature, it is a **pass-through method** and should not exist.

> ⚠️ Known risk: a `reader` layer sitting over `store` could easily degrade into pass-throughs. It earns its existence only by *orchestrating* — refresh = fetch + parse + upsert + dedup; article = cache lookup *or* extract-then-cache. If it stops orchestrating, collapse it into `store`.

### Define errors out of existence

The best exception is one that cannot occur. Concretely, here:

- Missing config file → create it with sensible defaults, don't error.
- Missing XDG dirs → create them.
- A feed is unreachable → the river still renders from cached items. One dead feed never blocks a launch.
- An item has no publish date → fall back to fetch time, don't drop it.
- Extraction fails → show the feed-provided blurb and a note. Never a modal.

### Design it twice

For anything non-trivial, sketch two genuinely different approaches before writing code. The second one is usually better, and comparing them teaches you what the real constraints are. This is explicitly worth the time.

### Strategic, not tactical

We are not shipping to a deadline. Investing 10–20% extra time in design is the default, not a luxury. Working code is not the finish line; *good design that also works* is.

### Comments

Comments exist to record what the code **cannot** say. If a comment repeats the code, delete it.

**Terse and functional.** Prose in comments does not age well and it is not where design rationale belongs. Hard budgets:

- Doc comment: **≤ 2 sentences.** A third only to state an invariant or a constraint a caller can violate.
- Implementation comment: **1 line.** If it needs a paragraph, the code needs a better name or a smaller function.
- Package comment: what the package is for, plus the one thing a reader must know before using it. Not an essay.

A comment that explains *why we rejected the alternative* over several lines belongs in this file, under §2, with at most a pointer from the source. The source says what is true; `AGENTS.md` says why we chose it.

- Write the **interface comment first**, before the implementation. If it's hard to write, the design is wrong — fix the design, not the comment.
- Interface comments describe *what and why*. Implementation comments describe *how*. Never mix them; implementation detail in an interface comment is information leakage.
- Comment the non-obvious: units, invariants, why an alternative was rejected, why an ugly workaround exists.
- Do not restate the signature. `// Load reads and validates config.toml` earns its place; `// Load loads the config` does not.

### Red flags

Treat any of these as a signal to stop and redesign:

- **Shallow module** — interface complexity ≈ implementation complexity.
- **Information leakage** — a design decision known to more than one module.
- **Temporal decomposition** — structure mirrors execution order rather than knowledge. *(Watch for this: "fetch → parse → store → render" is a tempting but wrong package layout.)*
- **Overexposure** — callers must learn a rare feature to use a common one.
- **Pass-through method / variable** — does nothing but forward.
- **Repetition** — the same snippet over and over.
- **Special-general mixture** — a general mechanism containing a special case.
- **Conjoined methods** — you can't understand one without reading the other.
- **Comment repeats code**.
- **Vague name** / **hard to pick a name** / **hard to describe** — all mean the abstraction is muddled.

### Somewhat general-purpose

Prefer interfaces that are somewhat more general than today's need — usually *simpler*, not more configurable. Ask: "what is the simplest interface that covers all my current needs?" Not "what might someone want someday?"

---

## 4. Go conventions

- **Go 1.26.6**, pinned via `mise.toml`. Module `github.com/JoshuaFurman/openfeed`.
- `context.Context` as the first parameter for anything doing I/O.
- Wrap errors with `%w` and context that helps the reader: `fmt.Errorf("fetch %s: %w", url, err)`. Sentinel errors (`errors.Is`) at package boundaries where callers must branch.
- Accept interfaces, return concrete types. Define interfaces at the **consumer**, not alongside the implementation.
- Table-driven tests. Golden files + recorded feed fixtures for parser tests — never hit the network in a test.
- No package-level mutable state. Dependencies are constructed in `main` and injected down.
- `gofmt` (non-negotiable), `go vet`, `staticcheck`. All green before any commit.
- Packages get created **when there is code for them**, not in advance. Empty directories are deleted.

### Intended layout

Subject to revision — this is a sketch, not a contract. Implementation status lives in [`PLAN.md`](PLAN.md).

```
cmd/openfeed/         thin main: construct deps, hand off to cli
internal/config/      XDG paths, config.toml load/validate
internal/feed/        fetch + parse + normalize (gofeed), conditional GET
internal/store/       SQLite; domain-shaped API; embedded migrations
internal/reader/      orchestration — the core seam (see §3)
internal/tui/         Bubble Tea
internal/article/     on-demand readability extraction        [later]
internal/server/      templates + htmx                        [later]
internal/ai/          Provider interface + no-op              [later]
internal/cli/         cobra commands
```

> `article` may well collapse into `feed` — resist the urge to keep five shallow packages when two deep ones would do.

### Dependencies

Added only when first genuinely needed. Each new dependency is a decision to justify, not a default.

| Package | Status | Why this one |
|---|---|---|
| `github.com/BurntSushi/toml` | in use | Its decoder reports `Undecoded()` keys, so a typo in a hand-edited config (`nmae =`) becomes a named startup error instead of a silently missing setting. `pelletier/go-toml/v2` is faster but that matters nowhere in this program. |
| `bubbletea` · `bubbles` · `lipgloss` | planned | TUI |
| `cobra` | planned | CLI |
| `mmcdole/gofeed` | in use | RSS/Atom/JSON Feed parsing behind our source-item model; avoids maintaining three format parsers. HTTP remains ours for conditional GET and response limits. |
| `modernc.org/sqlite` | in use | pure-Go, no cgo (see §2) |
| `go-shiori/go-readability` | later | article extraction |

### Tooling

`mise.toml` pins Go. `staticcheck` must be installed from source against the current toolchain — an older binary fails with `unsupported version: 2` on Go 1.26 export data:

```
go install honnef.co/go/tools/cmd/staticcheck@latest
```

---

## 5. Living plan

[`PLAN.md`](PLAN.md) is the authoritative roadmap, task checklist, and cross-session handoff.
It tracks the v0.1 acceptance criteria, the next reviewed increment, open questions, and later work.
This file remains the record of settled decisions and engineering conventions; do not duplicate task status here.

---

## 6. How we work

**Do not one-shot this project.** It is being built deliberately, in reviewed increments.

- Read `PLAN.md` at session start and update it after each reviewed increment and before handoff: completed work, unfinished work/blockers, verification, and the next concrete action.
- Small, coherent changes. One design decision at a time.
- Discuss design before implementing anything non-trivial. Propose two approaches (§ Design it twice) and recommend one.
- Explain meaningful trade-offs and implement the agreed behavior completely, including tests. Do not leave implementation exercises or decision placeholders for the user.
- Explain *why*, not just *what*. Insights about the codebase and the reasoning behind choices are part of the deliverable.

### Commits

Conventional Commits, and keep them short. `feat(config): ...`, `fix(store): ...`,
`chore:`, `docs:`. Subject line does the work; a body only when the *why* is
genuinely non-obvious, and then a couple of lines. Unrelated changes get
separate commits.

### Definition of done for an increment

1. `gofmt`, `go vet`, `staticcheck` clean.
2. Tests pass; new logic has tests.
3. Interface comments written for every exported symbol.
4. No new red flags from §3.
5. This file updated if a decision changed.
6. `PLAN.md` updated to reflect actual progress, remaining work, and the next action.

---

## 7. Open questions

Unresolved questions and when they must be decided live in [`PLAN.md`](PLAN.md#open-design-questions).
When a choice is settled, record the decision and rationale in §2 here and remove the open question from the plan.
