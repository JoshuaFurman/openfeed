# Development plan

The living record of what is done, what comes next, and what still needs a decision.
[`AGENTS.md`](AGENTS.md) owns settled design decisions and engineering conventions; this file owns
work status and sequencing. Proposals here are not settled decisions until recorded there.

## Session handoff

- **Completed:** configuration, standalone feed fetching, store lifecycle/migrations, atomic refresh
  persistence, and chronological-range timeline queries across selected active subscriptions.
  Settled decisions and the timeline contract are in `AGENTS.md`.
- **Current behavior:** `openfeed` still bootstraps/loads config and prints paths and subscriptions.
  Fetching, persistence, and timeline queries are tested libraries, not wired into the command.
  Schema version 3 adds the timeline index; seen state, reader orchestration, and TUI do not exist yet.
- **Review completed:** reviewed all timeline changes against the approved contract for correctness,
  API/design, reliability/tests, performance, and security; no blocking findings. Cursor scope remains
  a caller constraint, and tied/inactive index scans and live date-correction effects remain documented
  trade-offs. This review changed only the handoff; verification below is from implementation.
- **Next action:** commit the reviewed timeline increment, then design persisted seen-position
  representation and advancement, including ties and publication-date corrections. Do not implicitly
  persist the in-memory pagination cursor as the seen-state format; reader/command wiring is separate.
- **Implementation in progress:** none. Timeline queries and their design records are implemented,
  verified, and uncommitted; no known blockers. Atomic refresh persistence is committed in `f91a737`.
- **Last verification:** formatting, uncached full race and no-cgo tests, `go vet`, `staticcheck`,
  `go mod verify`, and `git diff --check` passed. Store coverage is 93.4% in the no-cgo run.
  Timeline insertion/correction, range refresh, cursor reopen/upgrade, cancellation, WAL, and concurrent
  query tests passed 25 race-detector runs. Store tests cross-compile without cgo for Linux/amd64 and
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
- [ ] **Persist reading position.** Save/load the agreed seen-position representation across runs.

**Timeline implementation:** [`internal/store/timeline.go`](internal/store/timeline.go) implements the
approved [timeline read contract](AGENTS.md#timeline-reads). Actual SQLite query plans confirm ordered
index scans for head reads and date seeks for bounded reads, without a global sort, in tie-heavy and
sparse-active fixtures. No reader reconciliation, viewport behavior, or refresh scheduling is wired yet.

**Next design comparison:** choose the saved chronological seen-position representation and when it
advances. Pagination has an opaque observed position, but its existence does not settle divider ties,
advancement, or the persisted seen-state format.

**Timeline acceptance coverage:**

- `internal/store/timeline_test.go`: active/empty/uncached visibility, literal and repeated URLs,
  independent cross-subscription identities, unchanged cache contents, deterministic multi-page ties,
  limits/exhaustion, inclusive/exclusive range edges, publication/fallback transitions, and UTC
  nanosecond/extreme-year results and boundaries.
- `internal/store/timeline_refresh_test.go`: above/below/tied insertions between pages, observed cursor
  independence after moved/deleted boundary rows and reopen, repeats/skips from date corrections,
  a 125-post head refresh with interior discoveries/corrections, resuming older history afterward,
  and exhaustion that does not hide a later discovery. Boundary deletion is test-only, not a new API.
- `internal/store/timeline_sql_test.go`: version-2 upgrade without data changes, cursor reuse across
  reopen, invalid queries, public errors without partial pages, cancellation during execution and
  connection waits, WAL reads during writes, concurrent page consistency, 40,000-URL selections,
  and query plans for each boundary combination in tied/sparse fixtures.

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
- Existing config comments and the source-of-truth decision overstate deletion as lossless; reading
  position is explicitly non-regenerable elsewhere. Reconcile that wording when settling seen state.

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

**Seen-state cases:** late-discovered dated posts sort by publication, not discovery; corrections
can move existing items across a divider. The divider must not claim to separate all newly discovered
posts. Cover timestamp ties when choosing its representation and advancement rules.

**Remaining storage verification:** saved reading position surviving reopen and its agreed boundary/
advancement behavior. Public errors and results must not expose SQL types. Apply the quality gates in
`AGENTS.md` to each increment.

**Still deferred:** launch refresh, scheduling, rendering, full-text extraction, search, web serving,
or AI. Do not introduce their packages or schema speculatively.

### Then: `internal/reader` and command wiring

- [ ] Construct and inject configuration, store, and HTTP dependencies in the command.
- [ ] Orchestrate subscription selection, fetching, and persistence behind a reader-shaped API.
- [ ] Handle one feed's failure independently while keeping cached items available.
- [ ] Implement the chosen refresh policy and expose results without HTTP/SQL details to the UI.
- [ ] Prove the full refresh → store → timeline path with offline integration tests.

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
| Seen-position representation and advancement | Seen-state schema and UI | Chronological semantics are settled. Choose the saved cursor/tie-breaker, when it advances, behavior when publication corrections move items across it, and truthful divider wording. No arrival watermark, per-item unread flags, or unread counts. |
| Retention and removed subscriptions | Explicit policy before v0.1 completion | Decide how to bound growth and whether to eventually prune inactive caches. Active-config visibility is settled; refreshes and queries do not delete cached posts. Subscriptions never become DB-owned. |
| Refresh policy | Reader orchestration | Launch, manual, and/or periodic refresh; bounded concurrency and polite behavior toward feed servers without blocking cached reading. |
| TOML round-tripping | Subscription-management commands, later | Preserve comments/formatting when editing subscriptions programmatically; no library choice settled. v0.1 only reads config. |

## Later, roughly in order

- [ ] Reader pane with on-demand full-text extraction and cached fallback to feed content.
- [ ] Feed management commands and OPML import/export, after solving TOML round-tripping.
- [ ] FTS5 search.
- [ ] Feed/tag filters over the same river.
- [ ] `openfeed serve`: localhost UI using the same core, embedded templates, and htmx.
- [ ] Optional AI features behind the provider seam; no real providers until reading is solid.
