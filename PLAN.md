# Development plan

The living record of what is done, what comes next, and what still needs a decision.
[`AGENTS.md`](AGENTS.md) owns settled design decisions and engineering conventions; this file owns
work status and sequencing. Proposals here are not settled decisions until recorded there.

## Session handoff

- **Completed:** configuration and the standalone feed-fetching module. The latest implementation
  commit is `6d0f6ac` (`feat(feed): fetch and normalize feeds with stable item keys`).
- **Current behavior:** `openfeed` bootstraps/loads config and prints paths and subscriptions.
  Fetching is tested but not wired into the command. There is no database or TUI yet.
- **Next action:** discuss the store's domain API and schema, then implement its first small increment.
  Start with the store design questions below; do not build the reader or TUI alongside it.
- **Implementation in progress:** none.
- **Last code verification:** the feed increment passed formatting, tests with the race detector,
  tests with cgo disabled, `go vet`, and `staticcheck`. Re-run relevant checks after code changes.

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

- [ ] **Design the boundary.** Define store-owned models, the consumer-facing operations, schema,
  and conversion responsibilities without importing HTTP/parser types into the persistence API.
- [ ] **Open and migrate.** Add `modernc.org/sqlite`, embedded hand-written migrations, database
  lifecycle handling, and reopen/migration tests using temporary databases.
- [ ] **Persist refresh outcomes.** Store items and validators atomically; enforce uniqueness on
  `(configured feed URL, item key)`; update existing entries and preserve first-fetch dates.
  A 304 updates metadata without replacing items; failed refreshes leave cached state untouched.
- [ ] **Query the river.** Return a deterministic newest-first timeline with bounded pagination
  across active subscriptions. Keep SQL, ordering, and deduplication inside the store.
- [ ] **Persist reading position.** Save/load the agreed seen-position representation across runs.

**First design comparison:** one domain operation that commits a feed's refresh outcome versus
fine-grained item/validator writes coordinated by the caller. Prefer the atomic domain operation:
callers should not have to know that committing validators before items can lose posts. The exact
API and models still need to be designed before implementation.

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
| Store boundary and models | Store API/schema | Store-owned persistence models and explicit adapters versus leaking fetched-item types across layers. Preserve the separate-model decision without creating a shared dumping-ground package. |
| Updates to an existing item | Upsert implementation | Which entry wins when a response repeats a key; whether/how corrected or newly supplied publication dates change ordering. First-fetch fallback must remain stable. |
| Timeline pagination | Timeline query | Offset versus cursor pagination, with a deterministic tie-breaker and behavior when refresh inserts items between pages. |
| Meaning of seen position | Finalizing time fields and seen-state schema | Publication order versus arrival/new-since semantics, including late/undated items and when the position advances. No per-item unread flags or counts. |
| Retention and removed subscriptions | Store lifecycle; explicit policy before v0.1 completion | How to bound growth, whether to retain removed feeds' cached posts, and how active config controls visibility. Subscriptions never become DB-owned. |
| SQLite concurrency policy | Store connection setup | Connection/transaction strategy, busy handling, and journal settings suitable for a future local server alongside the TUI. |
| Refresh policy | Reader orchestration | Launch, manual, and/or periodic refresh; bounded concurrency and polite behavior toward feed servers without blocking cached reading. |
| TOML round-tripping | Subscription-management commands, later | Preserve comments/formatting when editing subscriptions programmatically; no library choice settled. v0.1 only reads config. |

## Later, roughly in order

- [ ] Reader pane with on-demand full-text extraction and cached fallback to feed content.
- [ ] Feed management commands and OPML import/export, after solving TOML round-tripping.
- [ ] FTS5 search.
- [ ] Feed/tag filters over the same river.
- [ ] `openfeed serve`: localhost UI using the same core, embedded templates, and htmx.
- [ ] Optional AI features behind the provider seam; no real providers until reading is solid.
