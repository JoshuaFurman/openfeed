# Development plan

The living record of what is done, what comes next, and what still needs a decision.
[`AGENTS.md`](AGENTS.md) owns settled design decisions and engineering conventions; this file owns
work status and sequencing. Proposals here are not settled decisions until recorded there.

## Session handoff

- **Completed:** configuration, standalone feed fetching, store lifecycle/migrations, atomic refresh
  persistence, chronological-range timeline queries across selected active subscriptions, the
  saved seen position (`b5384a3`), and timeline dates capped at first fetch (`0c1d8a3`). Settled
  decisions and contracts are in `AGENTS.md`.
- **Current behavior:** `openfeed` still bootstraps/loads config and prints paths and subscriptions.
  Fetching, persistence, timeline queries, the seen position, and reader orchestration are tested
  libraries, not wired into the command. Schema version 5 orders the timeline by the earlier of
  publication and first-fetch time. The TUI does not exist yet.
- **Last review (reader increment, before commit):** 10 findings, all verified against the code.
  - **Fixed, each with a test that fails without it:**
    - a joining call whose starter was canceled now runs a fresh round;
    - state is reread after waiting for a host slot;
    - a feed found not due forgets its earlier failures;
    - each joining call gets its own copy of the report;
    - `SaveSeen` no longer logs the caller's cancellation.
  - **Doc fixes:** the `Refresh` comment budget, `AGENTS.md` wording that put the seen-save triggers in
    the reader rather than the interface, and this handoff.
  - **Rejected, by approved design:** a global cap across hosts (the user chose "all at once") and
    `SeenPosition` forwarding (recorded as deliberate in `AGENTS.md`).

  Earlier: `b5384a3` was reviewed after commit; `0c1d8a3` was never separately reviewed. Item-ID reuse
  after deleting the highest-ID row remains a hard retention requirement.
- **Next action:** the user commits the reviewed reader increment. Then do step 2, command wiring:
  - `main` builds config, a `slog` log file at `LogPath`, the store, the HTTP client, and the reader;
  - Ctrl-C cancels cleanly;
  - until the TUI exists, `openfeed` runs one refresh and prints the newest ~20 posts plus a failure
    summary.
- **Implementation in progress:** the reader increment (`internal/reader`) is implemented, reviewed,
  fixed, and verified, but not yet committed. The design was approved this session and recorded in
  `AGENTS.md` (reader API, refresh policy, refresh rounds).

- **Last verification:** formatting, uncached full race and no-cgo tests, `go vet`, `staticcheck`,
  `go mod verify`, and `git diff --check` passed after the review fixes. Reader coverage is 100% and
  store coverage 93.7% in the no-cgo run. Reader tests passed 25 race-detector runs; concurrency tests
  use `testing/synctest` with a real SQLite store, so they need no sleeps.

  Deliberately breaking each reader rule made a test fail: removing the per-host limit; overlapping
  calls starting their own rounds; cancellation starting backoff; backoff never doubling; conflicts
  counted as failures; a backwards clock holding a feed; keeping the latest rather than the furthest
  reached position; and each review fix undone in turn. Reader and store tests cross-compile without cgo for Linux/amd64 and
  Windows/amd64 (compile checks only; runtime verification was on macOS/arm64).

### Keeping this useful

1. At session start, read `AGENTS.md`, this file, and the relevant code; check the working tree.
2. Before non-trivial implementation, compare two designs and record the agreed choice in
   `AGENTS.md`. Keep unresolved choices in the table below rather than in code placeholders.
3. After each reviewed increment, update its checklist and this handoff: completed work, unfinished
   work/blockers, verification, and the next concrete action. Do not mark planned or scaffolded work done.
4. Keep history in Git, not an ever-growing session diary. Link decisions rather than copying them.

## v0.1 — read-only river

The goal is one end-to-end path: configured subscriptions → fetch → persist → chronological river.
Config remains intent; stored posts and feed metadata are derived state, alongside the persisted
reading position. A failed refresh must not prevent reading cached posts.

### Completed

- [x] `internal/config`: XDG paths, first-run bootstrap, validation, authoritative feed labels.
- [x] `internal/feed`: RSS/Atom/JSON Feed normalization, conditional GET, bounded/cancellable HTTP,
  per-subscription identity keys, empty-entry filtering, offline fixtures and HTTP tests.

### Next: `internal/store`

Complete these as reviewed increments, not one large change:

- [x] **Choose the boundary.** Atomic refresh persistence with store-owned inputs; the future reader
  adapts fetched results, while config owns active subscriptions and labels.
- [x] **Open and migrate.** Added `modernc.org/sqlite`, embedded hand-written migrations, lifecycle
  handling, private-directory creation, literal-path handling, and temporary-database tests. Covers
  reopen, upgrades, whole-batch rollback, newer-schema rejection without modification, concurrent
  opens, WAL readers, connection replacement, and bounded/cancellable lock waits.
- [x] **Settle item update and time semantics.** Approved duplicate winners, whole-item publisher
  replacement, immutable first-fetch state, and publication/fallback chronology versus discovery.
  See [`AGENTS.md` refresh item semantics](AGENTS.md#refresh-item-semantics).
- [x] **Design refresh models and schema.** Approved `FeedState`/`SaveRefresh`, opaque revisions
  with conflict rejection, explicit unchanged outcomes, feed/item tables, and lossless canonical
  UTC timestamp storage. Store models remain independent of HTTP/parser types.
- [x] **Persist refresh outcomes.** Added migration `002_refresh.sql`, versioned feed snapshots,
  and atomic item/validator persistence. Enforces per-subscription identity, approved duplicate/date
  rules, unchanged-outcome handling, stale-snapshot rejection, and rollback without advancing state.
- [x] **Query the river.** Added store-owned timeline models, live date/ID cursors, exclusive/inclusive
  range bounds, 50/200 page limits, and migration `003_timeline.sql`. Queries select active URLs without
  changing cached data and support head-span reloads plus continued older-history loading.
- [x] **Persist reading position.** Added migration `004_seen.sql`, a singleton seen point saved by
  keeping the maximum, and `TimelineCursor.Compare` for placing the divider. The
  point survives reopen, sessions, corrections, and removal of its item. Committed in `b5384a3`
  and reviewed.
- [x] **Cap future dates at first fetch.** Migration `005_timeline_first_fetch_cap.sql` orders the
  timeline by the earlier of publication and first-fetch time, so future-dated posts no longer pin the
  head or the divider. Committed in `0c1d8a3`.

**Timeline implementation:** [`internal/store/timeline.go`](internal/store/timeline.go) implements the
approved [timeline read contract](AGENTS.md#timeline-reads). Actual SQLite query plans confirm ordered
index scans for head reads and date seeks for bounded reads, without a global sort, in tie-heavy and
sparse-active fixtures. No reader reconciliation, viewport behavior, or refresh scheduling is wired yet.

**Seen implementation:** [`internal/store/seen.go`](internal/store/seen.go) implements the approved
[seen position](AGENTS.md#seen-position) contract. It is store-only: nothing yet decides when to
advance, keeps a session's displayed divider, or renders "You left off here".

**Timeline acceptance coverage:**

- `internal/store/timeline_test.go`: active/empty/uncached visibility, literal and repeated URLs,
  independent cross-subscription identities, unchanged cache contents, deterministic multi-page ties,
  limits/exhaustion, inclusive/exclusive range edges, publication/fallback transitions with the capped
  effective date (including dates after first fetch), `Compare` order, and UTC
  nanosecond/extreme-year results and boundaries.
- `internal/store/timeline_refresh_test.go`: above/below/tied insertions between pages, observed cursor
  independence after moved/deleted boundary rows and reopen, repeats/skips from date corrections,
  a 125-post head refresh with interior discoveries/corrections, resuming older history afterward,
  and exhaustion that does not hide a later discovery. Boundary deletion is test-only, not a new API.
- `internal/store/timeline_sql_test.go`: version-2 upgrade without data changes, version-4 upgrade that
  rebuilds the index to match the query expression and reorders a future-dated post without touching
  data or the seen point, cursor reuse across
  reopen, invalid queries, public errors without partial pages, cancellation during execution and
  connection waits, WAL reads during writes, concurrent page consistency, 40,000-URL selections,
  and query plans for each boundary combination in tied/sparse fixtures.

**Seen acceptance coverage:**

- `internal/store/seen_test.go`:
  - Fresh and v3-upgraded databases start with no position, and reading creates no row; the upgrade
    leaves the cache unchanged. A saved position survives reopen and is visible from separate handles.
  - The divider split matches the `Before`/`Through` bounds.
  - Older, equal, and lower-ID tied positions do not change the saved point. In a date-only feed, a
    later post at the same instant lands above a divider placed inside a tie.
  - Late discoveries, a new subscription's back catalog, and date corrections land on the correct
    side, including corrections to the reached post itself.
  - A reached future-dated post sits at the divider, not above it, and a later real post lands above.
    The point outlives its hidden or removed item, and re-saving it is a no-op.
  - Invalid or uncached positions and public errors are rejected. Reads succeed during writes (WAL),
    and lock waits are bounded and cancellable.
  - Four handles advancing in shuffled order, interleaved with refreshes, end at the maximum.
- `internal/store/timeline_test.go`: `Compare` agrees with timeline order across ties and URL sets.

**Reader integration notes:**

- `feed.Result.Items` preserves source order and duplicate keys; the reader must preserve that
  ordering so storage can enforce the approved first-occurrence winner.
- The fetcher already merges validators on 304; a 200 without validators intentionally clears old
  tokens. Persist the resulting tokens exactly, atomically with the successful refresh outcome.
- Keep publisher publication time separate from immutable first-fetch time; derive timeline order
  using the approved transition table. First-fetch time is not a commit/arrival watermark.
- Config owns active URLs, names, and tags. Cached feed metadata must not become a subscription list;
  the future reader should adapt fetched data into store-owned inputs and attach config labels.
- Reload/reconcile loaded timeline spans by identity after refreshes; a fresh head page is not
  automatically adjacent to old history. Preserve a fetchable path across unfinished refresh spans,
  and handle selected items moving outside them. Nil `Next` exhausts the requested interval for now,
  not all history; exact staging and viewport mechanics belong to reader/TUI work.
- Seen state: follow [`AGENTS.md` seen position](AGENTS.md#seen-position) for what advances it,
  the session's displayed divider, and when to save. `AdvanceSeen` failures are logged and never
  block reading.

**Persistence acceptance coverage:**

- `internal/store/refresh_test.go`: schema upgrade and reopen, non-creating state reads, first-duplicate
  winners on insert/update, whole-field replacement, every approved publication transition, immutable
  first-fetch state despite earlier subsequent timestamps, separate subscriptions/hash identities,
  omitted-item retention, empty/unchanged outcomes, exact validators, and UTC nanosecond round-trips
  including dates outside Unix nanoseconds' range.
- `internal/store/refresh_atomic_test.go`: metadata/item/commit-failure rollback and recovery,
  first-fetch assignment after rollback, invalid-input rejection, stale full/unchanged outcomes,
  competing writers on one or separate handles, bounded/cancellable lock waits, connection-pool
  cancellation, and public errors without SQLite error types.
- Lifecycle tests now follow the embedded migration count; migration-engine tests start from an
  isolated bookkeeping-only schema so future domain migrations do not change their fixtures.

**Remaining storage work:** none planned for v0.1 beyond the retention open question below. Apply the
quality gates in `AGENTS.md` to each increment.

**Still deferred:** launch refresh, scheduling, rendering, full-text extraction, search, web serving,
or AI. Do not introduce their packages or schema speculatively.

### Then: `internal/reader` and command wiring

- [ ] Construct and inject configuration, store, and HTTP dependencies in the command (step 2).
- [x] Orchestrate subscription selection, fetching, and persistence behind a reader-shaped API.
- [x] Handle one feed's failure independently while keeping cached items available.
- [x] Implement the chosen refresh policy and expose results without HTTP/SQL details to the UI.
- [x] Prove the full refresh → store → timeline path with offline integration tests.

**Reader implementation:** [`internal/reader`](internal/reader) implements the approved
[reader API, refresh policy](AGENTS.md#2-decisions-made), and [refresh rounds](AGENTS.md#refresh-rounds).
Reviewed and fixed; commit pending.

**Reader acceptance coverage:**

- `internal/reader/reader_test.go`:
  - End to end: the real `feed.Client` over an in-memory transport and a temp store; a labeled
    cross-feed river from RSS and Atom; a second round sending saved validators and recording 304s.
  - Configured-feed selection and labels, hiding feeds removed from config, and empty config.
  - Seen bookkeeping: furthest reached position only, zero positions ignored, and saves retried after
    failure.
- `internal/reader/refresh_test.go`:
  - One feed's failure (fetch or save) never fails the round, cached posts stay readable, and failures
    are logged. A closed store fails every feed without panicking.
  - With `synctest`:
    - every host starts at once, with at most 2 requests per host;
    - overlapping calls share one round with separate copies of the report, and an impatient joiner
      leaves without cancelling it;
    - a joiner reruns the round after its starter is canceled;
    - a queued feed saved elsewhere is not fetched;
    - cancellation while fetching or waiting for a slot saves nothing and starts no backoff.
  - The 30s gap holds across readers sharing a database but not for a backwards clock. Backoff runs
    15s → 10 min and resets on success, including a success elsewhere. A save conflict counts as
    refreshed and is not refetched.
    Source order and 304 outcomes reach the store unchanged.

**Command wiring (step 2, approved):** build dependencies in `main` with a `slog` log file and signal
cancellation. Until the TUI exists, run one refresh and print the newest posts plus a failure summary.

This layer must earn its existence through orchestration, not merely forward store methods.
The article operation remains deferred until there is a reader pane.

### Then: `internal/tui`

- [ ] Render one chronological river from the reader API, with config-owned source labels.
- [ ] Automatically load older history while scrolling and reconcile refreshed results without
  silent head/history gaps or duplicate visible identities; implement the agreed position-preserving
  refresh behavior and settle anchor-movement edge cases before implementation.
- [ ] Add `j`/`k` navigation and `o` to open a usable article URL in the browser.
- [ ] Show the seen divider and persist the agreed position as the user reads/exits.
- [ ] Handle empty config, empty feeds, refresh failures, resize, and clean shutdown.
- [ ] Verify first-run and cached/offline reading end to end; update user-facing instructions.

### v0.1 acceptance

```text
$ vim ~/.config/openfeed/config.toml
$ openfeed
→ river renders; j/k scrolls
→ o opens the selected item's article URL in a browser
→ reading position survives restart
→ a dead feed does not block reading cached items
```

No subscription-management commands, reader pane, web UI, or real AI providers in v0.1.

## Open design questions

Resolve a question before the work it affects, not every future question up front.

| Question | Decide before | What needs comparing |
|---|---|---|
| Launch placement and newer-than reads | TUI | Selection-driven advancement is contiguous when the river opens at the seen point and reading moves upward; that needs an ascending "newer than" read or a head-to-divider span load. |
| Retention and removed subscriptions | Explicit policy before v0.1 completion | Decide how to bound growth and whether to eventually prune inactive caches. Active-config visibility is settled; refreshes and queries do not delete cached posts. Subscriptions never become DB-owned. `items.id` lacks `AUTOINCREMENT`, so deleting the highest-ID row lets SQLite reuse that ID (verified). Before any deletion, rebuild `items` with `AUTOINCREMENT` or never delete that row; the seen tie-breaker and first-fetch lookup rely on IDs never being reused. |
| TOML round-tripping | Subscription-management commands, later | Preserve comments/formatting when editing subscriptions programmatically; no library choice settled. v0.1 only reads config. |

## Later, roughly in order

- [ ] Reader pane with on-demand full-text extraction and cached fallback to feed content.
- [ ] Feed management commands and OPML import/export, after solving TOML round-tripping.
- [ ] FTS5 search.
- [ ] Feed/tag filters over the same river.
- [ ] `openfeed serve`: localhost UI using the same core, embedded templates, and htmx.
- [ ] Optional AI features behind the provider seam; no real providers until reading is solid.
