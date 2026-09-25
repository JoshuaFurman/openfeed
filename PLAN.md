# Development plan

The living record of what is done, what comes next, and what still needs a decision.
[`AGENTS.md`](AGENTS.md) owns settled design decisions and engineering conventions; this file owns
work status and sequencing. Proposals here are not settled decisions until recorded there.

## Session handoff

- **Completed:** configuration, standalone feed fetching, store lifecycle/migrations, atomic refresh
  persistence, chronological-range timeline queries across selected active subscriptions, the
  saved seen position (`b5384a3`), timeline dates capped at first fetch (`0c1d8a3`), reader
  orchestration (`bf4e163`), command wiring (`56309c5`), and newer-than timeline reads (`6990119`).
  Settled decisions and contracts are in `AGENTS.md`.
- **Current behavior:** `openfeed` loads config, refreshes every subscription (blocking, before the
  screen opens; this moves into the TUI in increment 3), and then opens the TUI river at the newest post.
  The river supports `j`/`k`, `g`, `o`, `q`, older paging, resize, empty states, and failing feeds in
  the status line. With no subscriptions it prints where to add one. Signals exit 130/143 quietly.
- **Last review (TUI increment 2, before commit):** 10 findings, all verified and fixed, each with a
  test that fails without its fix:
  - a failed launch read left a dead "No posts yet" screen; any move now retries it;
  - the name column shifted as pages loaded; it is now sized once from the configured names, via the
    new `reader.Feeds`;
  - failures hid the key hints, which moved to the header;
  - failure reasons were missing; one failure shows its reason, several point to the log;
  - Ctrl-Z did nothing; it now suspends;
  - rows retained feed content; rows are now slim and sanitized once;
  - ages went stale while idle; a minute tick redraws them;
  - terminals of 1–2 rows got a taller frame;
  - one test depended on the time zone (now pinned to UTC);
  - `x/ansi` was missing from the dependency table.

  Earlier: TUI increment 1 was reviewed before `6990119`, command wiring before `56309c5`, the reader
  before `bf4e163`, `b5384a3` after its commit, and `0c1d8a3` never separately. Item-ID reuse after
  deleting the highest-ID row remains a hard retention requirement.
- **Next action:** the user tries TUI increment 2 in a terminal and commits it. Then build increment 3: launch and pull refresh
  inside the TUI, with the spinner (adding `bubbles/v2`), reconciliation, and failing-feed status. That
  removes the blocking pre-TUI refresh.
- **Implementation in progress:** TUI increment 2 is implemented, reviewed, fixed, and verified, but
  not yet committed. It adds:
  - `internal/tui` (model, view, `Sanitize`, opener, `Run`);
  - Bubble Tea and Lip Gloss v2;
  - `reader.ItemID` and `reader.Feeds`;
  - `cmd/openfeed` handing off to `tui.Run`, which replaces the plain-text printout.

- **Last verification (increment 2):** formatting, uncached full race and no-cgo tests, `go vet`,
  `staticcheck` (including no literal format characters in sources), `go mod verify`, and
  `git diff --check` passed after the review fixes. TUI coverage is 92.0%; the rest is `openBrowser`
  (launches a real browser) and `Run`'s terminal wrapper. TUI tests passed 25 race-detector runs, and
  undoing each review fix made a test fail.

  Deliberately breaking each model rule made a test fail:
  - newer pages moving the cursor;
  - stale pages merging after `g`;
  - duplicate posts;
  - older pages loading only at the edge;
  - `g` without a head reload;
  - unsanitized titles.

  In a real pseudo-terminal, the built binary refreshed real feeds, entered and left the alternate
  screen, rendered the header hints, ages, names, and titles, showed "dead failed: <reason>", moved
  with `j`, and quit with `q` (exit 0), before and after the review fixes. The binary cross-builds for Linux/amd64 and Windows/amd64.

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
- `internal/store/timeline_after_test.go`: `After` reads.
  - Upward paging over ties, undated posts, and an inactive feed, for several starts and limits.
  - Edges: above the newest, exactly filled, nearest first, above a tie, and an empty selection.
  - Rejected combinations and zero cursors.
  - The resume sequence, with the left-off post in place, newest, moved newer, or removed, and with
    nothing at or below the saved position.
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

- [x] Construct and inject configuration, store, and HTTP dependencies in the command (`56309c5`).
- [x] Orchestrate subscription selection, fetching, and persistence behind a reader-shaped API.
- [x] Handle one feed's failure independently while keeping cached items available.
- [x] Implement the chosen refresh policy and expose results without HTTP/SQL details to the UI.
- [x] Prove the full refresh → store → timeline path with offline integration tests.

**Reader implementation:** [`internal/reader`](internal/reader) implements the approved
[reader API, refresh policy](AGENTS.md#2-decisions-made), and [refresh rounds](AGENTS.md#refresh-rounds).
Committed in `bf4e163`.

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

**Command wiring (step 2):** `cmd/openfeed` builds config, a private `slog` log file, the store, an HTTP
client, and the reader. Interrupts cancel through the context, and a second interrupt exits
immediately. Until the TUI exists, the command runs one refresh and prints a status summary, failing
feeds, and the 20 newest posts, replacing control characters in untrusted text.
`cmd/openfeed/main_test.go` covers:
- first launch with no subscriptions;
- a labeled, ordered, sanitized river alongside a failing feed that is logged, including escape
  sequences, bidi overrides, line separators, and a future-dated post shown at its river date;
- a rerun that finds feeds already fresh and still prints cached posts;
- cancellation mid-refresh that saves nothing and leaves the database reopenable;
- exit statuses: 130/143 for interrupt/terminate, 1 for cancellation without a signal, and joined
  failures still reported. Plus sanitizer cases.

This layer must earn its existence through orchestration, not merely forward store methods.
The article operation remains deferred until there is a reader pane.

### Then: `internal/tui`

- [x] Render one chronological river from the reader API, with config-owned source labels.
- [ ] Automatically load older history while scrolling and reconcile refreshed results without
  silent head/history gaps or duplicate visible identities; implement the agreed position-preserving
  refresh behavior and settle anchor-movement edge cases before implementation.
- [x] Add `j`/`k` navigation and `o` to open a usable article URL in the browser.
- [ ] Show the seen divider and persist the agreed position as the user reads/exits.
- [ ] Handle empty config, empty feeds, refresh failures, resize, and clean shutdown.
- [ ] Verify first-run and cached/offline reading end to end; update user-facing instructions.

TUI requirements already settled:
- refresh at launch while the cached river shows;
- pull to refresh by moving up past the newest post, with a spinner and a minimum visible duration;
- new posts appear above the cursor;
- the "You left off here" divider, with seen triggers per [`AGENTS.md`](AGENTS.md#seen-position);
- sanitized terminal text, with dates at `EffectiveDate`;
- columns sized by display width rather than rune count.

**TUI acceptance coverage (increment 2):** `internal/tui/tui_test.go` drives the model as the event loop
does, over a real reader and temporary store. It covers:
- exact rendering (ages, wide feed names, sanitized and untitled titles, height, alternate screen);
- lines fitting every width, and a name column sized from config that never shifts;
- older paging and bounds;
- resumed windows reading upward with the cursor kept on its post;
- `g` with stale-page dropping;
- overlapping pages never duplicating posts;
- opening, including missing links and opener errors;
- resize keeping the cursor visible, down to one row;
- loading, empty, failing-feed, read-error, and launch-read-retry states;
- quit, suspend, the minute tick, context cancellation through `run`, and `age`.

`text_test.go` covers `Sanitize`.

**TUI design (approved: B, resume where you left off; recorded in
[`AGENTS.md`](AGENTS.md#tui-river)):**

- Launch placement: resume at the saved position, with newer posts above, reading upward. Pressing up
  past the newest post refreshes, and `g` jumps to the newest. This needs a store/reader "newer than"
  read (increment 1). Opening at the newest (Twitter-style) was rejected, because its divider marks
  skipped posts as passed.
- Shared: a custom windowed list rendered with Lip Gloss, not `bubbles/list`, which is page-oriented
  and adds a filter UI. The window loads 50-post pages as the cursor nears either edge, and the cursor
  follows its post by `ItemID` across reloads. The divider is fixed at the session's launch baseline.
- Refresh: at launch, and on pull (k at the newest post), with a spinner shown for at least 600ms.
  When the window includes the head, it is reloaded from the head through its oldest post, and new
  posts land above the cursor. Otherwise new posts wait above the loaded window, and changes inside
  it are reconciled at the next head reload. The status line names failing feeds, with no post counts.
- Seen: `Reached` on every navigation (the post left and the post landed on), on `g`, and on `o`.
  `SaveSeen` runs after a 2s idle debounce, after each refresh, and at exit.
- `o` opens the article URL with the platform opener, without a shell. `q` or Ctrl-C quits.
- Dependencies: `charm.land/bubbletea/v2` v2.0.10, `charm.land/bubbles/v2` v2.2.1 (spinner), and
  `charm.land/lipgloss/v2` v2.0.6 (layout and display width).
- Increments:
  1. The newer-than read (`6990119`).
  2. River rendering, navigation, paging, `o`, resize, empty states, and quit, replacing the printout
     (implemented and reviewed; commit pending).
  3. Launch and pull refresh with reconciliation.
  4. Seen: divider, resume placement, and saves.

**Increment 1 (approved; contract in [`AGENTS.md`](AGENTS.md#timeline-reads)):** add `After` to
`TimelineQuery` and `reader.Query`. It selects the posts nearest above it (strictly newer, nearest
first), returned newest-first, with a `Newer` continuation. `After` cannot combine with `Before` or `Through`. Launch
reads `After: saved` first, then `Before:` that page's oldest post, or a head page when nothing is newer.
This order never needs a "from the bottom" bound. Rejected: a separate `Newer` method with duplicate
query/page types.

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
| Retention and removed subscriptions | Explicit policy before v0.1 completion | Decide how to bound growth and whether to eventually prune inactive caches. Active-config visibility is settled; refreshes and queries do not delete cached posts. Subscriptions never become DB-owned. `items.id` lacks `AUTOINCREMENT`, so deleting the highest-ID row lets SQLite reuse that ID (verified). Before any deletion, rebuild `items` with `AUTOINCREMENT` or never delete that row; the seen tie-breaker and first-fetch lookup rely on IDs never being reused. |
| TOML round-tripping | Subscription-management commands, later | Preserve comments/formatting when editing subscriptions programmatically; no library choice settled. v0.1 only reads config. |

## Later, roughly in order

- [ ] Reader pane with on-demand full-text extraction and cached fallback to feed content.
- [ ] Bound the log file, by rotation or size; each run currently appends without limit.
- [ ] Feed management commands and OPML import/export, after solving TOML round-tripping.
- [ ] FTS5 search.
- [ ] Feed/tag filters over the same river.
- [ ] `openfeed serve`: localhost UI using the same core, embedded templates, and htmx.
- [ ] Optional AI features behind the provider seam; no real providers until reading is solid.
