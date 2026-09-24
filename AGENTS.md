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
| **River interaction** | A refreshable, newest-first chronological timeline with endless scrolling through available cached history. Chronologically newer posts appear at the top; pagination is not a user-facing interaction. | Target the reading feel of Twitter's chronological timeline, without algorithmic ranking or page navigation. |
| **River refresh experience** | At the head, show newer posts; below it, preserve the current reading position and offer a jump to newer posts. Reconcile refreshed data by stable identity without discarding loaded history or silently skipping the interval between a fresh head page and existing history. | Refresh should not interrupt reading. Prepending one page alone misses intervening posts, late discoveries, and corrections inside the loaded range. |
| **Read state** | Seen-position in publication/fallback chronology, not unread counts or an arrival watermark. A divider marks that position; no bold/unbold, no badges. | No inbox-zero pressure. Late discoveries can fall below the divider; any discovery count must be separate from it. |
| **Seen position** | One global point in timeline order: effective date plus an `ItemID` tie-breaker, stored as values without a foreign key. Absent until the user first reaches an item. | An item bookmark moves with date corrections, loses its anchor when its feed is removed or pruned, and regresses under last-writer-wins. A timestamp-only point hides same-instant posts from date-only feeds. |
| **Seen advancement** | Only user navigation in the unfiltered river advances it, by keeping the maximum; it never moves backward. Viewport and selection are never persisted. | Loading or rendering is not reading. A maximum is idempotent and order-independent, so concurrent sessions need no revision or conflict error, unlike refreshes. |
| **Storage** | SQLite via `modernc.org/sqlite` (pure Go, **no cgo**). `database/sql` directly, no ORM. Hand-written migrations as embedded `.sql`. | Buys FTS5 search, `ORDER BY score` for AI ranking, and safe concurrent access when `serve` runs alongside the TUI. Pure-Go driver keeps cross-compiled release binaries trivial. |
| **Store boundary** | `FeedState` reads a subscription's cached validators and an opaque revision; `SaveRefresh` atomically persists a successful outcome against that snapshot, using store-owned inputs. The reader adapts fetched results; config retains subscription intent and labels. | Separate writes let callers advance validators without saving posts. No HTTP/parser types or caller-managed transactions belong in the store API. |
| **Overlapping refreshes** | Only the first refresh committed against a given feed revision succeeds. Every accepted outcome advances the revision; a stale snapshot returns `ErrRefreshConflict` without changes. | Last-commit-wins lets a slow response overwrite newer content/validators. Revisions work across processes without holding a database lock during HTTP; fetch timestamps are not version tokens. |
| **Refresh outcomes** | An explicit `Unchanged` outcome requires existing cached feed state and no items; a full outcome may contain zero items. Persist the supplied validators and fetch timestamp exactly in either case; no failure outcome is persisted. | An empty feed is not an unchanged feed. The fetcher already resolves validator inheritance, so storage must not merge tokens again. |
| **Refresh schema** | `feeds` stores URL, validators, accepted fetch timestamp, and revision. `items` stores stable IDs, feed URL/key uniqueness, publisher fields, nullable publication time, and immutable first-fetch time; no config labels or tags. | Keep refresh persistence cohesive without pre-building timeline, seen-state, extraction, or subscription-management schema. |
| **Timestamp storage** | Fixed-width UTC text with nine fractional digits, covering years 0000–9999; absent publication time is NULL. Reject unsupported dates rather than rounding or overflowing. | Canonical text compares chronologically and preserves nanoseconds without Unix nanoseconds' roughly 1678–2262 range limit. |
| **Refresh retention** | Refreshes never delete omitted items or prune cached feeds. Config remains authoritative for which subscriptions are active. | Feed responses are often sliding windows, not deletion lists. A bounded retention/removal policy belongs to its own reviewed increment. |
| **SQLite connections** | WAL, one connection per store, foreign keys enabled, and FULL synchronous durability. Retry busy lock acquisition for at most five seconds, respecting caller cancellation; never replay a partially executed transaction. | Small local operations do not need an in-process pool. WAL permits readers in another process during writes; Go-managed waits avoid a long, non-cancellable SQLite busy timeout. |
| **Migrations** | Numbered embedded SQL files and a `schema_migrations` ledger; all pending migrations and their version records commit in one exclusive transaction. Check/migrate before enabling WAL; reject newer schemas without changing them. | Exclusive locking prevents commit contention during concurrent first opens, before WAL exists; under WAL it is equivalent to immediate locking and permits readers. The first migration owns only bookkeeping; domain tables arrive with their reviewed behavior. |
| **Source of truth** | **Config is intent; the DB is local state.** Feeds are declared in hand-editable `config.toml`. The DB holds feed caches, derived data, and the saved reading position; it never owns a subscription. | Deleting `openfeed.db` never loses a subscription. It does lose the reading position, which is not regenerable, and cached posts that have left their feeds' windows. Subscriptions stay greppable and version-controllable. |
| **Item models** | `feed.Item` represents publisher data; `store.RefreshItem` and `store.TimelineItem` are separate store-owned input and read models. | Avoids leaking HTTP/parser concerns into presentation or database IDs into fetching. |
| **Feed labels** | Resolved config names are authoritative, including the hostname fallback. Fetched titles never rename subscriptions. | No naming-provenance flag or config changes needed for fetching. |
| **Item identity** | Within the configured feed URL: publisher ID → usable article URL → SHA-256 of title, feed content, and publication date when present. | Prefer stable identifiers; hashing is a best-effort fallback whose key can change after edits. Different subscriptions retain separate entries. |
| **Duplicate keys** | Within one response, the first normalized occurrence of a key wins as a whole item. Storage owns this rule; the reader preserves source order. | First and last position are both imperfect freshness signals. Respect source order rather than infer freshness from dates or merge contradictory entries. |
| **Publisher updates** | For an existing identity, a successfully persisted refresh replaces title, URL, content, and publication date with the winning entry, including empty/missing values. First-fetch time remains immutable. | Keeping selected historical fields invents a composite item and can conceal publisher corrections or removals. The cache reflects the accepted response, not an editorial history. |
| **Empty feed entries** | Skip entries with no title, feed content/description, or usable article URL after normalization. | An identifier or timestamp alone gives the reader nothing to display or open; partially populated entries remain usable. |
| **Undated items** | Fetching preserves missing/invalid publication dates; storage keeps them absent and uses immutable first-fetch time as the ordering fallback. Atom update dates are not publication dates. | Never hash fetch time, fabricate a publication date, or move a persistently undated post forward on each refresh. |
| **Timeline dates** | Order by the earlier of the current publication date and first-fetch time; undated items use first-fetch time. Newly supplied, corrected, or removed publication dates can change an existing item's position. | Freezing the initial position would ignore corrections. Publication chronology and discovery are different facts; late discoveries belong at their publication date. A post cannot have been published after openfeed first saw it, so a future-dated or clock-skewed date sorts at discovery. Otherwise it would pin the river head and the seen divider until that date passes. |
| **Timeline pagination** | Live keyset pagination ordered by effective timeline date descending, then stable item ID descending. Cursors capture an observed date/ID pair, not an item's mutable current position; each page reads current committed data. | Inserts above a cursor do not shift the boundary as offsets do. Date corrections can still repeat or skip moved identities across raw page reads; no frozen multi-page snapshot or exactly-once traversal under mutation is promised. |
| **Timeline visibility** | Queries receive the selected active configured URL set. Empty means an empty river; uncached URLs contribute nothing, and inactive cached subscriptions remain hidden without deletion. A changed selection starts a fresh traversal. | Config remains authoritative without becoming a store dependency. URL order does not affect chronology, and identities from different subscriptions remain separate. |
| **Timeline query contract** | `Timeline` accepts an exclusive `Before` and inclusive oldest `Through` boundary, both optional, with 50 items by default and an explicit limit of 1–200. A page returns items and an optional older continuation within that interval. | One bounded range query supports head reads, endless older scrolling, and reloading an existing span for refresh reconciliation; latest/older-only queries leave range comparison and termination to the reader. |
| **Timeline results and cursors** | Store-owned items expose a stable database-local `ItemID`, source URL, publisher fields, publication/first-fetch times, and an opaque observed position. Cursors come from results or `SeenPosition` and are used only with the same database, including across handles/reopen. They compare in timeline order across URL sets; continuing a traversal requires the same URL set. | Keep publisher inputs and config labels separate from persisted identity. Cursor scope is a caller constraint, as with `FeedState`, not a new database identity system. The seen position persists its own columns, never a serialized cursor or page continuation. |
| **Timeline indexing** | One global expression index on effective date descending, ID descending, and source URL. Bind the selected URLs as a JSON array; apply scalar date bounds alongside full date/ID comparisons. | Avoid per-feed merging, SQL variable-count limits, and a generated date column. SQLite does not seek with the expression tuple alone; date seeks can still scan ties and inactive entries, so page size bounds results, not total database work. |
| **Feed fetching** | A stateless client accepts/returns ETag and Last-Modified validators, with a 30-second deadline and 10 MiB body limit. | Storage owns persistence; validators must be committed with successfully fetched items, never before them. Limits bound stalled or oversized responses. |
| **Article text** | Feed-provided content in the river. Full text extracted **on demand** when an item is opened, then cached. | Eager extraction means hundreds of requests to other people's servers per refresh. On-demand is polite and keeps refresh fast. |
| **Web UI** | `html/template` + `//go:embed` + htmx. Stdlib `net/http` routing. | Single self-contained binary, no Node, no build step. Go 1.22+ pattern routing (`"GET /items/{id}"`) makes `gorilla/mux` unnecessary — **dropped from the original plan**. |
| **AI** | Define the `Provider` seam now with a no-op implementation. Build zero real providers until reading is solid. | Core never branches on "is AI on?" — it just holds a Provider. No speculative generality. |
| **Paths** | XDG conventions **on all platforms, including macOS.** Hand-rolled in `internal/config`, ~40 lines, no dependency. | Primary dev machine is darwin, where `adrg/xdg` would resolve to `~/Library/Application Support`. We want `~/.config/openfeed` everywhere. |

### Refresh item semantics

Obtain a `FeedState` snapshot before fetching and pass it back to `SaveRefresh` with the adapted
outcome, using the same database. Reading an uncached URL returns empty metadata without creating
a subscription or acquiring a network-duration lock. `SaveRefresh` returns only success or an error;
a conflict requires reading fresh state and fetching again, never replaying the stale response.
Revisions order accepted outcomes, not publisher freshness: the first committer wins a shared revision,
and later revisions may carry earlier request timestamps. Retain no database transaction while fetching.

For one stored `(configured feed URL, item key)`, let `F₀` be the `FetchedAt` value from the
refresh whose insertion first commits successfully. Preserve it across updates and reopen;
it is not the minimum timestamp across subsequent refreshes. A rolled-back insertion establishes
no first-fetch state. `feed.Result.FetchedAt` is a UTC request-start timestamp, not a commit time
or a reliable arrival cursor.

Keep publisher publication time nullable and separate from `F₀`. The effective timeline date is
derived from those facts, not an independently frozen date. A publication date later than `F₀` counts
as `F₀`, while the publisher's date stays available for display:

| Publication-date transition | Effective timeline date |
|---|---|
| Missing → missing | `F₀`, never the latest fetch time |
| Missing → dated | Supplied publication date |
| Dated → corrected | Corrected publication date, earlier or later |
| Dated → missing/invalid | `F₀` |
| Missing after removal → dated again | Newly supplied publication date |
| Any date later than `F₀` (future-dated or clock skew) | `F₀` |

Clearing a date can move an older post forward once to its original first-fetch time; repeatedly
fetching it undated does not move it again. Do not retain a previous date when the winning entry
has none, or borrow a date/content field from a losing duplicate. An unchanged response (304)
does not mutate items or their dates; a failed refresh leaves all cached state untouched.

These are same-identity update rules, not similarity matching. If an edit changes a hash-derived
key, it produces a distinct identity with its own first-fetch time; different subscriptions also
remain independent.

A post published a week ago and first fetched today belongs a week back in the river, even below
a saved seen divider. The divider marks chronological position, not every arrival since a wall-clock
time, and carries no count; see [Seen position](#seen-position).

### Timeline reads

`Before` excludes its observed date/ID position; `Through` includes its observed position and excludes
anything older. Nil bounds are unrestricted. Empty or reversed intervals return no items; supplied
zero-value cursors and limits outside 0–200 are errors. An empty URL set never means all cached feeds.
URL order and repeated selections do not affect the result. Queries never write or prune cached data.

A page's `Next` is the last returned position, not the lookahead row, and exists only when another row
matches the interval at query time. Continue with the same URL set and `Through` bound; nil `Next`
means current interval exhaustion, not permanent exhaustion of history. Results carry nullable
publication time as zero `time.Time`, immutable first-fetch time, and untrusted feed-provided content;
they do not embed refresh inputs or include config labels/tags. Failed reads return no partial page.

For a head refresh, start without `Before`, set `Through` to the old loaded span's oldest observed
position, and follow continuations through that span. This also finds late discoveries and corrections
inside the span, rather than just head additions. Clear `Through` to resume older scrolling below the
refreshed span. Each call sees committed data in one read, without holding a transaction between calls;
range reads do not turn a changing river into an exactly-once snapshot or perform UI reconciliation.

### Seen position

The saved seen position is one global point in timeline order: a canonical effective date and an
`ItemID` tie-breaker, stored as values in a singleton row. It is not a reference to the item it came
from, a page continuation, or the viewport. Items move across the fixed point; the point never follows
an item, and it survives that item's correction, hiding, or removal.

`AdvanceSeen` receives the observed position of an item the user reached and keeps the maximum.
Timeline dates are already capped at first fetch, so the point never runs ahead of real time. The
reached post sits exactly at the point, and posts tied with it and older sit directly below it in the
river. Older, equal, and lower-ID tied positions are no-ops. Among exact timestamp ties, later
discoveries receive larger IDs and sort above the point; retention must keep IDs increasing to preserve
this.

| After the point P | Placement |
|---|---|
| New post dated after P, or a later discovery at exactly P's date | Above |
| Late discovery dated before P, including a new subscription's back catalog | Below |
| Post corrected to a newer or older date | Moves across accordingly |
| The reached post's own date corrected or removed | Point stays; the post moves |

Only user navigation in the unfiltered river advances the position: selecting a post or explicitly
jumping to the newest. Launch placement, refresh, page loads, rendering, corrections, subscription
changes, and scrolling older never do; later filtered views display the point without advancing it.
A session reads the point once as its displayed divider and advances the saved point separately.
The reader saves only when its reached maximum grows, debounced and on refresh/exit; a failed save is
logged and never blocks reading.

There is no position or divider before the first advance, including on a fresh, upgraded, or deleted
database; none is fabricated at launch. Subscription changes never alter it. Sessions and processes
merge through the maximum in one write, so interleaving cannot regress it. The divider reads
"You left off here": no count, and never "new", "unread", "since your last visit", or "caught up".

### Paths

```
$XDG_CONFIG_HOME/openfeed/config.toml    # intent — yours, hand-edited, git-able
$XDG_DATA_HOME/openfeed/openfeed.db      # local state — cached items, extracted text, summaries, seen position
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
AdvanceSeen(ctx, Position) error
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
