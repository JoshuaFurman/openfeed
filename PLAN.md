# Development plan

The living record of what is done, what comes next, and what still needs a decision.
[`AGENTS.md`](AGENTS.md) owns settled design decisions and engineering conventions; this file owns
work status and sequencing. Proposals here are not settled decisions until recorded there.

## Session handoff

- **Completed:** configuration, standalone feed fetching, and the first `internal/store` increment:
  database lifecycle, transactional embedded migrations, and contention/cancellation handling.
  The duplicate-key, publisher-update, and publication/first-fetch semantics are now approved
  and recorded in `AGENTS.md`; persistence itself is not implemented.
- **Current behavior:** `openfeed` still bootstraps/loads config and prints paths and subscriptions.
  Fetching and storage are tested libraries, not wired into the command. The database schema contains
  migration bookkeeping only; there are no persisted posts, reading position, or TUI yet.
- **Review completed:** all existing code, tests, fixtures, project guidance, dependencies, and CI
  reviewed before storage. The atomic refresh boundary and lifecycle-only scope were approved;
  settled storage decisions are recorded in `AGENTS.md`.
- **Next action:** review the exact store-owned refresh inputs/results and feed/item schema,
  including changed/unchanged outcomes, timestamp representation, and overlapping same-feed
  refreshes. Then implement atomic persistence against the approved semantics and acceptance cases
  below; keep reader/command wiring, querying, and seen-state implementation in later increments.
- **Implementation in progress:** none. The lifecycle increment remains implemented and uncommitted;
  this increment changes design documentation only. Remaining work is API/schema design, not a
  blocker in the lifecycle implementation.
- **This increment's verification:** reviewed the existing fetcher, normalization tests, and store
  lifecycle against the decisions; aligned the README's divider description and passed
  `git diff --check`. No code/schema changes or new executable tests; code quality gates were not
  re-run for this documentation-only increment.
- **Last code verification:** formatting, uncached race and no-cgo tests, `go vet`, `staticcheck`,
  and `go mod verify` passed. Concurrent-open/WAL/lock-release tests passed 100 race-detector runs;
  store coverage is 87.2%. Store tests cross-compile without cgo for Linux/amd64 and Windows/amd64
  (compile checks only; runtime verification was on macOS/arm64). CI now also tests with cgo disabled.

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
- [ ] **Design refresh models and schema.** Define exact inputs/results, feed/item tables, and time
  representation without importing HTTP/parser types into the persistence API.
- [ ] **Persist refresh outcomes.** Store items and validators atomically; enforce uniqueness on
  `(configured feed URL, item key)`; update existing entries and preserve first-fetch dates.
  A 304 updates metadata without replacing items; failed refreshes leave cached state untouched.
- [ ] **Query the river.** Return a deterministic newest-first timeline with bounded pagination
  across active subscriptions. Keep SQL, ordering, and deduplication inside the store.
- [ ] **Persist reading position.** Save/load the agreed seen-position representation across runs.

**Next design review:** compare concrete store-owned contracts for changed versus unchanged
refreshes, then define the feed/item schema and timestamp representation. Include how overlapping
same-feed refreshes interact; request-start timestamps are not assumed to be monotonic version
numbers. The atomic boundary and item/time behavior are settled in `AGENTS.md`; no domain API or
schema has been implemented.

**Pre-implementation review notes:**

- `feed.Result.Items` preserves source order and duplicate keys; the reader must preserve that
  ordering so storage can enforce the approved first-occurrence winner.
- The fetcher already merges validators on 304; a 200 without validators intentionally clears old
  tokens. Persist the resulting tokens exactly, atomically with the successful refresh outcome.
- Keep publisher publication time separate from immutable first-fetch time; derive timeline order
  using the approved transition table. First-fetch time is not a commit/arrival watermark.
- Config owns active URLs, names, and tags. Cached feed metadata must not become a subscription list;
  the future reader should adapt fetched data into store-owned inputs and attach config labels.
- Existing config comments and the source-of-truth decision overstate deletion as lossless; reading
  position is explicitly non-regenerable elsewhere. Reconcile that wording when settling seen state.

**Acceptance cases for the next persistence increment (not yet implemented):**

- Repeated keys in one response retain exactly the first normalized entry, even when a later one
  has more content or a newer/present publication date. Reversing source order reverses the winner;
  no fields are merged. A duplicate identity is inserted only once.
- A later successful refresh replaces all publisher fields for an existing key, including blank
  title/URL/content and missing/invalid publication time, while retaining its first-fetch time.
  Exercise duplicates on both insertion and update.
- Cover every publication-date transition in `AGENTS.md`, corrections in both directions, and
  repeated undated refreshes after date removal. Preserve absence as absence, not as fabricated
  publication time.
- First-fetch time survives repeat persistence and close/reopen, even if a later submitted refresh
  carries an earlier fetch timestamp. A rolled-back insertion establishes no first-fetch time;
  a subsequent successful insertion uses its own refresh timestamp.
- The same key in two configured subscriptions remains independent. A changed hash-derived key
  is a new identity, not an update of a similar-looking item.
- Unchanged responses preserve all item fields and dates. Failures/rollback preserve both items
  and validators; successful full responses persist the supplied validators, including clearing them.

**Later query/seen-state cases:** late-discovered dated posts sort by publication, not discovery;
publication corrections can move existing items. A chronological divider must not claim to separate
all newly discovered posts. Cover timestamp ties when choosing pagination and seen cursors.

**Tests that make storage complete:** fresh database and repeated open; data surviving close/reopen;
repeat-refresh deduplication; same key in different subscriptions retained separately; undated items
not jumping forward on refresh; atomic rollback; 304 preserving items; deterministic pagination
with tied timestamps; saved reading position surviving reopen. Public errors and results must not
expose SQL types. Apply the quality gates in `AGENTS.md` to each code increment.

**Not part of this increment:** launch refresh, scheduling, rendering, full-text extraction, search,
web serving, or AI. Do not introduce their packages or schema speculatively.

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
| Refresh models and schema | Item persistence | Exact store-owned inputs/results, feed/item tables, timestamp representation, and unchanged-response representation within the agreed atomic operation. Define handling of overlapping same-feed refreshes. The reader owns conversion; no shared dumping-ground package. |
| Timeline pagination | Timeline query | Offset versus cursor pagination, with a deterministic tie-breaker and behavior when refresh inserts items between pages. |
| Seen-position representation and advancement | Seen-state schema and UI | Chronological semantics are settled. Choose the saved cursor/tie-breaker, when it advances, behavior when publication corrections move items across it, and truthful divider wording. No arrival watermark, per-item unread flags, or unread counts. |
| Retention and removed subscriptions | Item persistence/querying; explicit policy before v0.1 completion | How to bound growth, whether to retain removed feeds' cached posts, and how active config controls visibility. Subscriptions never become DB-owned. |
| Refresh policy | Reader orchestration | Launch, manual, and/or periodic refresh; bounded concurrency and polite behavior toward feed servers without blocking cached reading. |
| TOML round-tripping | Subscription-management commands, later | Preserve comments/formatting when editing subscriptions programmatically; no library choice settled. v0.1 only reads config. |

## Later, roughly in order

- [ ] Reader pane with on-demand full-text extraction and cached fallback to feed content.
- [ ] Feed management commands and OPML import/export, after solving TOML round-tripping.
- [ ] FTS5 search.
- [ ] Feed/tag filters over the same river.
- [ ] `openfeed serve`: localhost UI using the same core, embedded templates, and htmx.
- [ ] Optional AI features behind the provider seam; no real providers until reading is solid.
