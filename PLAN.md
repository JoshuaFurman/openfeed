# Development plan

The living record of what is done, what comes next, and what still needs a decision.
[`AGENTS.md`](AGENTS.md) owns settled design decisions and engineering conventions; this file owns
work status and sequencing. Proposals here are not settled decisions until recorded there.

## Session handoff

- **Completed:** v0.1's read-only river (`v0.1.0`); adding feeds, tag filters, and the tag column in the
  TUI; the first web river (`31af99a`).
- **Current behavior:** `openfeed serve` serves a mobile-first web river: rounded cards with tag chips
  and the article's site, endless scroll, a "New posts ↑" pill, a header bell whose popover lists
  failing feeds with their errors, and a light/dark toggle remembered in a cookie (uncommitted).
- **Implementation in progress: web polish** (see [below](#v03--web-river)). Implemented and verified,
  not yet reviewed or committed.
- **Last verification (web polish):** formatting, `go vet`, `staticcheck`, uncached full race and
  no-cgo tests. Headless Chrome at 390×844 with local feeds, one failing: screenshots inspected in
  light and dark; the bell showed its dot, opened a popover naming the feed and its 404, and closed on
  an outside tap. The toggle restyled in place, survived a reload, overrode a dark device in light mode,
  and with no cookie the page followed the device. No console errors and no horizontal overflow.
- **Next action:** the user checks it on the phone (needs iOS 17+ for the popover) and reviews.
  Suggested commit: `feat(web): card redesign, theme toggle, and a failing-feeds bell`. Then tag
  filters on the web.

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

**Remaining storage work:** none for v0.1. Retention is settled: nothing deletes until a pruning
increment (see Later). Apply the quality gates in `AGENTS.md` to each increment.

**Still deferred:** periodic refresh scheduling, full-text extraction, search, web serving, or AI. Do not introduce their packages or schema speculatively.

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
- [x] Automatically load older history while scrolling and reconcile refreshed results without
  silent head/history gaps or duplicate visible identities; implement the agreed position-preserving
  refresh behavior and settle anchor-movement edge cases before implementation.
- [x] Add `j`/`k` navigation and `o` to open a usable article URL in the browser.
- [x] Show the seen divider and persist the agreed position as the user reads/exits.
- [x] Handle empty config, empty feeds, refresh failures, resize, and clean shutdown.
- [x] Verify first-run and cached/offline reading end to end; update user-facing instructions.

TUI requirements already settled:
- refresh at launch while the cached river shows;
- pull to refresh with a double-tap of `k`/↑ at the newest post (user decision, 2026-09-25: a single
  press refreshed too easily), with a spinner and a minimum visible duration;
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

**TUI acceptance coverage (increment 4):** a `counting` river records `Reached` calls. Tests cover:
- resuming with the divider between post 99 and the resumed post, in the lower part of the screen;
- resume edges: no saved position, saved at the newest post, and nothing at or below;
- reading upward saving the furthest post, with only the latest idle timer saving and reading older
  never moving it back;
- the first launch saving the head on the first move;
- `g` and `o` reaching their post, and refresh saving without reaching;
- the exit save after a signal;
- the divider drawn only where it falls, never beside unloaded posts, and giving way at short heights;
- a launch refresh that finishes before the first window still resuming;
- after review: jumps surviving a supersede and a retry, placement waiting for the terminal size, no
  line reserved for an unloaded divider, failed opens not reaching, and one save timer per move.

**TUI acceptance coverage (increment 3):** a fake fetcher drives real refreshes through the reader and
store. Tests cover:
- the launch refresh landing new posts above the cursor;
- pull-to-refresh, with a second pull ignored and no pull or reload below the head;
- the double-tap gesture: single taps, taps too far apart, an intervening key, flagged and unflagged
  autorepeat, a tap after holding, arriving at the top, and the "Press again" prompt;
- span reconciliation with corrections and a late discovery, keeping each post once;
- the cursor moving to the nearest older post when its post leaves;
- rows loaded during a reload being kept;
- failure status clearing on recovery, and cancellation stopping the spinner;
- an empty river filling after a refresh;
- superseded head reads being dropped;
- the spinner running only while refreshing, for at least its minimum time;
- after review: failed reloads retried on the next move, one reload at a time plus one more, rows loaded
  mid-reload reread, pending head reads reissued, the empty state during a refresh, and quitting waiting
  for work.

**TUI design (approved: B, resume where you left off; recorded in
[`AGENTS.md`](AGENTS.md#tui-river)):**

- Launch placement: resume at the saved position, with newer posts above, reading upward. Pressing up
  past the newest post refreshes (later changed to a double-tap), and `g` jumps to the newest. This needs a store/reader "newer than"
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
     (`c0b67a3`).
  3. Launch and pull refresh with reconciliation (`ee8f22f`).
  4. Seen: divider, resume placement, and saves (`11c99ad`).

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

## v0.2 — adding feeds in the TUI

`n` opens a popup for a feed's URL, optional name, and optional comma-separated tags. Approved design
(user decisions, 2026-10-13): fetch before saving, all three fields, open the TUI with no
subscriptions, and append to `config.toml`. Recorded in [`AGENTS.md`](AGENTS.md#2-decisions-made)
(Adding subscriptions, Config writes) and [Adding a feed](AGENTS.md#adding-a-feed).

- [x] `config.Config.Add`: rereads the file, validates it plus the block with `Load`'s parser, and
  appends with one `O_APPEND` write. `Feed.Normalize` is exported for the reader's offline checks.
- [x] `reader.Reader.Subscribe`: offline validation and duplicate check, fetch with cached validators,
  config append, save, and a copy-on-write feed set that reads and rounds snapshot.
- [x] TUI popup over the river, empty-config launch, name column resize, and reload after subscribing.
- [x] README, `AGENTS.md`, and real-terminal verification.

**Acceptance coverage:**

- `internal/config/config_test.go`: appends exactly after comments, an unterminated last line, and an
  empty file; omits a blank name; escapes quotes and backslashes; leaves the loaded feeds alone.
  Rejections (missing URL, bad scheme, a host-case duplicate, a file broken since launch) write
  nothing. A duplicate added by hand since `Load` is caught; a deleted file is recreated as the
  starter plus the block, mode 0600; a symlinked config stays a symlink with its target's mode.
- `internal/reader/subscribe_test.go`: the new feed's posts show without a refresh, labeled, and the
  next round finds it not due; rejections (invalid, already subscribed, fetch failure, config
  failure) change nothing and fetch only when they should; a hidden cache revalidates with its
  validators (304) under the new name; a failed save keeps the subscription and logs; two concurrent
  subscribes to one URL add it once while reads run alongside (race detector).
- `internal/tui/add_test.go`: typing, Tab/Shift-Tab/arrows, and Enter subscribe with trimmed URL,
  name, and tags; posts land in place with the cursor kept and the name column widened; river keys
  (`q`, `j`, `o`, …) type into the popup; Ctrl-C quits; Esc closes and a reopened popup starts empty.
  Errors (empty URL, bad scheme, fetch failure with an escape sequence and a bidi override) stay in
  the popup, sanitized, keeping the URL; typing clears them. An abandoned check's error is dropped,
  and one that subscribes anyway still lands. Pasted text is sanitized. The cursor sits after the
  typed URL, and lines fit 80, 40, and 20 columns. A first subscription fills an empty river.
- `internal/tui/tui_test.go`: the no-subscriptions screen.
- `cmd/openfeed/main_test.go`: a first launch presents the empty river, and subscribing through it
  appends to the real starter config and shows the post.

## v0.2 — filtering by tag

`/` opens a picker of the configured tags and "All posts". Approved design (user decisions,
2026-10-14): a list picker rather than typing or cycling, one tag at a time, and Esc or "All posts"
to clear. Applying a tag keeps the place in time; clearing returns to where the whole river was left.
Recorded in [`AGENTS.md`](AGENTS.md#2-decisions-made) (Tag filters) and
[Filtering by tag](AGENTS.md#filtering-by-tag). No store or schema change: the reader narrows the URL
set the store already selects by.

- [x] `reader.Query.Tag` and `Reader.Tags`, matching tags ignoring case and surrounding space.
- [x] TUI: window reads as values carrying their tag, the picker, Esc, the `#tag` header, the empty
  filtered view, and no reaching while filtered.
- [x] Tag column (user decisions, 2026-10-14: its own column after the name, all of a feed's tags,
  and a color per tag). `reader.Item.FeedTags` and `Reader.Feeds` carry tags as `Tags` spells them.
- [x] README, `AGENTS.md`, and real-terminal verification.

**Acceptance coverage:**

- `internal/reader/reader_test.go`: item and `Feeds` tags once each, as `Tags` spells them, and
  copied, so modifying a result or New's input changes nothing; `Tags` once each, in config order, as first spelled, skipping
  blanks, and copied; filtered pages older and newer for several spellings, an unknown tag, and a
  blank one; the whole river unchanged; a subscription's tags joining both.
- `internal/tui/filter_test.go`: the picker lists and sanitizes tags, starts on the first or the
  active tag, stops at both ends, and keeps keys from the river. Esc and `q` close it, Ctrl-C quits,
  and with no tags it shows a notice instead. It fits 80, 40, 20, and 12 columns, scrolling to keep
  the choice in sight. Filtering pages through every tagged post once; Esc returns where the river was
  left, and a second Esc does nothing. A filter opens below the cursor's post with the divider in
  place. Moves, `g`, and `o` while filtered reach nothing; switching tags keeps the place, and Esc then
  returns to the whole river's place, where moving reaches again. A filter applied during the launch
  read still resumes, and Esc then opens at the divider. Refreshes and a new subscription land in the
  filtered river. An empty filtered view says so, and a failed filter read is retried by the next move.
- `internal/tui/filter_test.go` (tag column): exact rows for one, two, and no tags; each tag in its
  palette color, uncolored in the selected row, and in the picker and the `#tag` header; the column
  kept in a filtered view. It is capped with "…", sanitized, shown at 57 columns and dropped at 56
  and 20, with every line fitting. It is absent without tags and appears when a tagged feed is added.
- `internal/tui/tui_test.go`: existing tests now reach the resume through `windowRead`, and an
  untagged river renders exactly as before.

## v0.3 — web river

`openfeed serve` serves the river as a mobile-first web page. Approved design (user decisions,
2026-10-18): Go templates + vendored htmx, no Node or hand-written JS; read on a phone over LAN or
Tailscale; listen on `:80`, every interface, without authentication; no divider, but a page's launch
refresh reaches its shown head, as `g`; refresh on load and pull only; excerpts; articles in a new tab;
stdlib `flag` until the CLI grows. Recorded in [`AGENTS.md`](AGENTS.md#2-decisions-made) (Web UI, Web
exposure, Web seen position, Web refresh, Excerpts) and [Web river](AGENTS.md#web-river).

- [x] `store.TimelineCursor` text tokens, strict on decode.
- [x] `internal/server`: the river page, older-page fragments, the refresh notice and pill, excerpts,
  CSRF protection, CSP and referrer headers, immutable embedded assets, graceful shutdown with a
  final seen save.
- [x] `openfeed serve [--addr]`, logging to the same file as the TUI.
- [x] README, `AGENTS.md`, and a real-browser check at phone size.
- [x] Card redesign (user choice B, 2026-10-19), a light/dark toggle kept in a cookie, and failing
  feeds in a header bell's popover instead of a sticky notice that covered the first card.
- [ ] Tag filters on the web: tag chips or a picker, `?tag=` on every route, filtered views never reach.
- [ ] Adding feeds on the web: a form over `Reader.Subscribe`.
- [ ] Excerpts in the TUI.

**Acceptance coverage:**

- `internal/store/timeline_test.go`: tokens round-trip and page from the decoded position; the zero
  position does not encode; malformed, non-canonical, zero, negative, and overflowing tokens fail.
- `internal/server/excerpt_test.go`: markup, entities, inline joins and block breaks, hidden and
  unclosed script/style, attributes, comments, control characters, broken markup, and truncation.
- `internal/server/server_test.go`: rows with names, colored tags, excerpts, links, and dates; a post
  without a URL has no link; an excerpt repeating the title is dropped; feed text and config labels
  are escaped. Older pages reach every post once, as fragments. Bad tokens are 400s. The pill appears
  only when a newer post exists; the bell waits, then shows all refreshed or a dot and each failing
  feed with its reason, out of band. The theme cookie renders either theme, follows the device without
  one, ignores unknown values, rejects bad and cross-site posts, and redirects plain form posts; hosts
  drop `www.`. Rendering never reaches; the refresh reaches
  the shown head, and a forged head is capped at the real one. Empty states, cross-origin POSTs
  rejected without fetching, security headers, immutable assets, templates not served, and `Serve`
  stopping on cancellation and saving the position.
- `cmd/openfeed/main_test.go`: `serve` and `--addr` parsing and mistakes; a real listener serving
  the refreshed river and stopping on cancellation; a listen failure announced nothing.

## Open design questions

Resolve a question before the work it affects, not every future question up front.

| Question | Decide before | What needs comparing |
|---|---|---|
| TOML round-tripping | Removing or renaming subscriptions in the app | Preserve comments/formatting when changing existing blocks; no library choice settled. Adding is settled: append a block ([`AGENTS.md`](AGENTS.md#2-decisions-made), Config writes). |

## Later, roughly in order

- [ ] Reader pane with on-demand full-text extraction and cached fallback to feed content.
- [ ] Bound the log file, by rotation or size; each run currently appends without limit.
- [ ] Prune cached posts, including removed subscriptions' caches. First rebuild `items` with
  `AUTOINCREMENT`, so a deleted highest ID is never reused.
- [ ] Sign and notarize the macOS binaries, replacing the cask's quarantine hook.
- [ ] Update `ci.yml` to `actions/checkout@v7` and `actions/setup-go@v7`, matching the release workflow.
- [ ] Removing and renaming subscriptions in the app, and OPML import/export, after solving TOML
  round-tripping. Adding is done (`n`).
- [ ] FTS5 search.
- [ ] Feed filters over the same river. Tag filters are done (`/`); a feed filter could join the same
  picker and `Query`.
- [ ] Optional AI features behind the provider seam; no real providers until reading is solid.
