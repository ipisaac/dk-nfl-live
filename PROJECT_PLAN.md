# dk-nfl-live — project plan

## Goal
Show DraftKings' current NFL **main-line** odds (moneyline / spread / total, no alts) on a public URL.
The page updates itself as DK moves lines and survives DK being unreachable or returning junk.
**Priority: latency from a DK line move to our screen.**

## Decisions
| Area | Decision | Why |
|---|---|---|
| Stack | **Go**: stdlib `net/http`, `log/slog`, `embed`; `github.com/coder/websocket` | Single binary, cheap goroutine fan-out, fast JSON; chosen over TS/Node and Elixir after comparison |
| Feed | **Ontario** (`dkcaon`) | This is what the linked DK page shows from Toronto |
| Host | **Fly.io `yyz`** after a probe shows DK accepts Fly's IPs; otherwise **Render Starter (Ohio)** with the same image | Toronto vs Ohio differs by only ~5 ms end to end; the probe decides |
| Dev | Home PC on localhost; first public URL is Fly | Proven DK access from here. Cloudflare quick tunnels don't support SSE, and a named tunnel would need another account |

## DK facts (verified on 2026-09-22)

### Snapshot
`GET https://sportsbook-nash.draftkings.com/api/sportscontent/dkcaon/v1/leagues/88808`
- Returns 200 in ~260 ms: ~174 KB of JSON with `cache-control: public,max-age=1`, so polling can
  never beat ~1 s.
- There is no `Age`, `ETag` or `Last-Modified`, and no timestamp in the body. `Date` follows the edge
  clock, and Akamai's debug `Pragma` headers are ignored. The snapshot can't tell us when its data
  was cut.
- Holds 32 events and 96 markets ({Moneyline, Spread, Total} × 32), all `subcategoryId 4518` with
  `main:true`.
- A league with no events returns **404**, not an empty body. We treat it as a failed snapshot.
- Has 192 selections, each with `outcomeType` (Home/Away/Over/Under), `points`, `trueOdds` and
  `displayOdds.american` (uses U+2212 for minus). Main lines carry the tag `MainPointLine`.
- The snapshot also publishes its own socket subscription.
  `subscriptionPartials["league-events-88808"]` =
  `{entity:"events", query:"$filter=leagueId eq '88808' and clientMetadata/Subcategories/any(s: s/Id eq '4518')&$orderBy=startEventDate asc", includeMarkets:"$filter=tags/all(t: t ne 'SportcastBetBuilder') and clientMetadata/subCategoryId eq '4518'"}`

### Push socket
`wss://sportsbook-ws-ca-on.draftkings.com/websocket?format=json&locale=en`
- JSON-RPC 2.0 `subscribe`. Params:
  - `entity`
  - `queryParams{query, includeMarkets, initialData:false, projection:"sportsbook", locale:"en-US"}`
  - `forwardedHeaders:{}`
  - `clientMetadata{feature:"league","X-Client-Name":"web","X-Client-Version"}`
  - `jwt:""`
  - `siteName:"dkcaon"`
- The ack is `{"id","event":"subscribed","data":"","websocketPublishTimestamp"}`.
- `initialData:true` sends nothing more, so **the snapshot is required** for bootstrap and resync.

### Akamai and TLS
- Akamai checks the TLS client on the snapshot, not the socket:
  - Snapshot: Node `fetch` and Go stdlib `net/http` get 200; **curl gets 403** from the same IP with
    browser headers.
  - Socket handshake: curl gets 101.
  - The snapshot also needs `Accept-Language`: the same Go client gets 403 without it (2026-09-23).
  - Over HTTP/1.1 it also needs `Connection: keep-alive`, whatever the TLS client (2026-09-24).
- Both hosts are Akamai-fronted. The socket CNAMEs to `sportsbook-ws-us-star-stls…`, probably a
  St. Louis origin.
- RTT from Toronto: ~7.5 ms to the Akamai edge, ~20 ms to AWS Ohio.

### Update frames (observed 2026-09-23: MLB pre-game, NPB in-play, NFL pre-game)
`{id, event:"update", data:{data:{add|change|remove:{events,markets,selections}}, metadata:{createdTime,
receivedTime, publishedTime}}, context, websocketPublishTimestamp}`. Fixtures are in `testdata/`.
- **One entity per message.** The two sides of a market arrive as separate messages, up to ~100 ms
  apart.
- **`add`** carries a full object. **`remove`** carries bare ID strings.
- **`change`** is a top-level field merge with absolute values. It sends only some fields; a
  selection change omits `marketId`, `outcomeType` and `participants`. Nested objects
  (`displayOdds`, `participants`) come whole. Replaying all frames this way over a start snapshot
  reproduced a later snapshot's odds, points, suspensions and statuses exactly, in all three
  leagues. The only differences were fields we don't use: `tags`, `isTeamInPossession`, `eventScore`,
  and `startEventDate` formatting (`…Z` vs `….0000000Z`; parse it as a time).
- **Idempotent.** Re-applying a frame changes nothing. DK resends identical values, including a
  `replacedSelectionId` whose old ID is already gone.
- **Line moves.** A `change` or `add` with `replacedSelectionId` gives the old selection a new ID
  (e.g. `…O700_1` → `…O750_1`). The new ID takes the old one's place and inherits the fields the
  frame omits.
- **Suspension** is `markets[].isSuspended`. Frames send `true`/`false` explicitly. The snapshot omits
  the field when false.
  - Pre-game, DK suspends a market and keeps its selections.
  - In-play, DK also removes the selections, and removes and re-adds whole markets
    (`remove.markets`/`add.markets`), every few seconds to minutes.
  - A removal can arrive market-first, then its selections, a few ms apart.
- **Game end**: `remove.events`.
- **No per-entity versions or timestamps**, and the snapshot has none either. So the content check
  and `snapshotLag` stay.
- **Order.** `websocketPublishTimestamp`, `publishedTime` and `createdTime` never went backwards
  within a subscription (~340 updates over 3 subscriptions).
- **DK-internal delay** (`publishedTime − createdTime`), p50 / p95:
  - NPB in-play: 10 / 15 ms.
  - NFL pre-game: 18 / 668 ms.
  - MLB pre-game: 176 / 837 ms.
- **Clock skew.** Our receive time minus `websocketPublishTimestamp` was about −35 ms, so the home
  PC's clock runs ~35 ms behind DK's.
- **Snapshot freshness vs the socket.** We polled every 2 s (1,900+ polls), plus plain vs
  cache-busted vs `no-cache` side by side. 98–99% of polls matched the socket state.
  - Stale ones lagged 0.5–4.2 s.
  - A few were *ahead* of the socket by up to ~1 s.
  - Cache-busting made no difference, so the lag is at DK's origin, not Akamai.
- [colemcph/dk-scraper](https://github.com/colemcph/dk-scraper) predicted this shape. We used its
  facts only; no code was copied.

## Latency budget
| Leg | Estimate | How we control it |
|---|---|---|
| DK odds engine → socket publish | ~630 ms p50 | Can't |
| DK socket → our server | ~5–20 ms | Host choice |
| Parse + apply + encode | < 1 ms | Publish every frame |
| Server → browser (SSE) | ~5–30 ms | Flush per event, heartbeat, no buffering proxy |
| DOM update | < 1 ms | Patch one row |

## Architecture
```
DK snapshot (one scheduler: bootstrap, 60 s resync, 2 s poll)     ─┐
DK push socket (deltas; ping 15 s)                                ─┴─► Feed ─► Board ─► Hub ──SSE──► browsers
                                                                       dk.go   board.go  sse.go      web/
```
- One process. A restart re-bootstraps in under 1 s.

### Files
Flat layout, all `package main`.

| File | Responsibility |
|---|---|
| `main.go` | Constants, `PORT` (default 8080), routes `/`, `/events`, `/healthz`; graceful shutdown via `signal.NotifyContext` + `srv.Shutdown` |
| `dk.go` | `Feed`: the apply goroutine (evidence, overlay, sync state), snapshot scheduler, socket dial/subscribe/read/ping (read limit raised), reconnect backoff, status, lag stats |
| `board.go` | DK wire types + validation; atomic `ApplySnapshot` / `ApplyUpdate` → changed game IDs; `Games()` → UI rows |
| `sse.go` | `Hub`: atomic board-then-patches handoff on connect; publish fans out pre-encoded frames; `status` heartbeat; slow-client drop |
| `web/index.html`, `web/app.js` | The page |
| `*_test.go`, `testdata/` | Real captured payloads + fault injection. `testdata/<league>-{start,end}.json` are snapshots, and `<league>-frames.jsonl` holds the raw update messages between them |
| `capture_test.go` | Build tag `capture`: records a league's frames and distinct snapshots into `captures/` (gitignored) |
| `Dockerfile`, `fly.toml` | `golang` build → `distroless/static`; Fly config |

### Constants
- `site="dkcaon"`, `leagueID="88808"`, `subcategoryID="4518"`, plus the snapshot and socket URLs.
- Switching region means changing the site and socket constants.

## Behaviour

### Ordering and consistency
- **One writer.** A single apply goroutine owns the Board and calls `Hub.Publish`. Socket frames and
  snapshot results reach it through one channel, in arrival order, so the board and the stream of
  patches can't diverge.
- **Subscribe before snapshot.** On start and on every reconnect: dial, subscribe, wait for the ack,
  and only then request the snapshot. This alone doesn't close the gap: a cached or lagging snapshot
  can be cut before the ack, so an update published in between is in neither. The sync rule below
  covers that.
- **Evidence.** For each entity a frame of the current subscription touched, the apply goroutine keeps
  the values frames set, each field stamped with the receive time of the frame that last set it.
  - A `change` merges its fields into the entry, an `add` replaces the entry with the whole object, and
    a `remove` replaces it with a tombstone. `replacedSelectionId` moves the old ID's fields to the new
    ID and leaves a tombstone on the old one; an `add` that moves a held selection then merges only the
    fields it sent, as the board does, so inherited fields keep their times.
  - Times are per field because `change` is partial: an unrelated later update must neither erase an
    earlier price nor make it look newer than a rejected-frame cutoff.

  A reconnect starts empty:
  a dead subscription's evidence is never used, because a newer update may have been missed after it.
  - Entries don't expire with time; they leave only by the rules below or with their subscription.
    `ponytail:` memory is about one entry per line move per game per subscription; prune entries
    whose event left the board if a subscription ever lives long enough for that to matter.
- **Sync state.** The board is either synced (it passed the checks below, which assume `snapshotLag`
  holds; there is no proof) or needs a resync from a moment `resyncFrom`. These set it:
  - socket loss: needs resync until the next ack;
  - each subscribe ack: `resyncFrom` = the ack;
  - a rejected frame, or a frame part skipped for an unknown ID: `resyncFrom` = its receive time.

  `snapshotLag` is the longest delay between a frame reaching our socket and a snapshot request
  showing it. Step 2 measured a max of 4.2 s, so it is **8 s** (max + 2 s sampling + margin);
  re-check on NFL Sunday. A snapshot's **cut** is `requestStart −
  snapshotLag`: if `snapshotLag` holds, the snapshot reflects every frame received before it.
- **Snapshot commit.** Build a new board from the snapshot → validate → drop superseded evidence →
  content check → overlay evidence → swap. Nothing is dropped unless the board validates and swaps.
  - **Drop superseded evidence.** If there is a `resyncFrom` and the cut is at or after it, drop fields and
    tombstones received before `resyncFrom` (an entry left empty goes). A rejected frame or skipped
    part may have set later values for any of them, which this snapshot holds and no entry does. A
    snapshot cut before `resyncFrom` keeps them. It may already hold the rejected frame's values, but
    nothing shows it does, so keeping the evidence is the conservative choice; the board stays
    unsynced until a snapshot cut at or after `resyncFrom` drops them.
  - **Content check**: every remaining field and tombstone received before the cut matches the snapshot.
    Values are compared as the board reads them, since DK spells some differently in frames and
    snapshots: a missing `main` is true, a missing `isSuspended` is false, `tags` count only for
    `MainPointLine`, and times compare in UTC. Replaying every captured frame and then committing
    the later capture gives zero misses in all three leagues. A miss has
    two possible causes we can't tell apart: the snapshot is older than `snapshotLag` allows, or it
    holds a newer value whose frame hasn't reached us yet. Either way: count it, stay unsynced, and
    let the next poll retry.
  - **Lost frame.** A field still missing in a snapshot requested ≥ 10 s after its first miss means
    the stream lost a frame (a late frame doesn't stay late that long): force a reconnect. The new
    subscription starts with no evidence, and its post-ack snapshot sets the board.
  - **Overlay**: apply every remaining field and tombstone to the new board. A snapshot never undoes a value the
    current subscription delivered and nothing has superseded. Overlaying a value the snapshot
    already holds is a no-op. An entity the snapshot lacks is rebuilt from its remaining fields plus
    the ones that never change under its ID (a selection's market and outcome, a market's event and
    type, an event's participants). A field whose evidence was dropped stays empty: no price, and a
    market with no suspension evidence shows as suspended.
  - Why overlaying is safe: entries are the latest values of one subscription's ordered stream. The
    only possible regression is an update the snapshot already has and the socket hasn't delivered
    yet; it arrives on the same subscription and overwrites that field. This relies on DK delivering a
    subscription's frames in order and without loss (step 2 checks the order; loss is caught by the
    lost-frame rule).
  - Overlaying requires frames that set absolute values. Step 2 confirmed it: `change` is an
    absolute-value field merge, which is why evidence is kept per field.
  - DK has no per-entity versions or timestamps (step 2), so there is nothing better to compare than
    values.

  The snapshot clears the sync state only if the cut is at or after `resyncFrom` and the content
  check has no miss. An unsynced snapshot still commits if it validates, since after the overlay it
  is the freshest data we have; only the status stays below `live`.
  - DK gives no better evidence. The snapshot has no `Age`, `ETag`, `Last-Modified` or body
    timestamps, and `Date` follows the edge clock (probed 2026-09-22), so its cut can't be read from
    HTTP caching headers.
  - `ponytail:` dropping superseded evidence, and syncing with no evidence to check, rest on the
    measured `snapshotLag` alone. Ceiling: a cache slower than measured, just after a reconnect or
    rejected frame, in a quiet market. Upgrade: per-entity versions, if DK has them.
- **Atomic commit.** A snapshot builds a whole new board and swaps it in. A frame builds new rows for
  the games it touches from copies, validates them, then commits all of them or none. A panic
  (caught by `recover`) or a validation failure rejects the frame: the board is untouched, the
  rejection is logged and counted, and the board needs a resync.

### Board (`board.go`)
- **Market filter**: keep a market only if `subcategoryId` is the board's (NFL 4518), `main!=false` and
  `marketType.name` is one of {Moneyline, Spread, Total}. Baseball's "Run Line" counts as the spread,
  so the MLB/NPB fixtures load. `subcategoryId` is a number in snapshots and a string in frames.
- **Selection choice**: per (market, outcomeType), take the only selection present, else the only one
  tagged `MainPointLine`, else none.
- **Odds**: use `displayOdds.american` with `−` replaced by `-`, and decimal from `trueOdds`. If
  American is missing or not `±digits`, derive it from `trueOdds`. A price with neither is not
  rendered; it never rejects a frame or snapshot, since DK would keep sending it.
- **Storage**: flat maps of events, markets and selections by ID, joined when building rows. A
  selection whose market is gone (a market removed a few ms before its selections) is simply not
  rendered.
- **Rows**: every apply rebuilds all rows and diffs them against the last, so it returns exactly the
  changed or removed game IDs. `ponytail:` ~100 markets; index by event past a few thousand.
- **Validation**: a frame or snapshot is rejected if it doesn't decode or an entity lacks an `id`. A
  frame also needs `data.add`, `data.change` and `data.remove` objects (all 845 captured updates had
  them); unused parts such as `add.leagues` are ignored. A snapshot with no games is rejected too (an
  empty NFL league is a 404 anyway).
- **Applying frames** (see Update frames): `add` sets the whole object; `change` merges the top-level
  keys the frame sent, so an explicit `null` clears a field; `remove` deletes by ID.
  `replacedSelectionId` (on `change` or `add`) moves the old entity to the new ID first, and the frame
  merges over it, so omitted fields are inherited. It is a no-op if the old ID is already gone. A
  market's `isSuspended` defaults to false.
- **Unknown IDs**: a `change` for an ID we don't hold (and not replacing one we hold) can't be merged.
  That part is skipped, the rest applies, and `ApplyUpdate` returns the skipped IDs; the Feed counts
  them and marks the board as needing a resync. Step 2 never saw one. `remove` of an unknown ID is a
  no-op.
- **Panics / invalid frames**: rejected whole; see Atomic commit above.
- **Offseason** (`ponytail:`): the board stays stale, which is fine during the NFL season.
- **UI row**: `{id, away, home, start, live, spread{away,home}, total{over,under},
  moneyline{away,home}, suspended flags, updatedAt}`. Each price is `{line?, american, decimal}`.

### Feed (`dk.go`)

#### Snapshot scheduler
Every snapshot request goes through one scheduler goroutine, whatever the trigger: bootstrap,
reconnect, the 60 s resync, polling, an unknown ID, a rejected frame.
- `Request(reason)` never blocks and coalesces into a single pending flag.
- At most one request is in flight.
- Request starts are at least 2 s apart. After a failure the spacing grows with full-jitter backoff
  (2 s → 30 s cap) and resets on the next success, or on a subscribe ack: the network is back, and
  a long backoff would hold the board below `live` after an outage.
- Polling is the scheduler re-requesting after each result while status isn't `live`: the socket is
  down, or the board needs a resync.
- Except right after DK's 30-min close: when a `live` board's socket closes after 60 s healthy, it
  redials at once, and requests wait up to 1 s for the ack (a failed redial ends the wait). A snapshot
  sent before the ack can't hold what the redial misses, and would push the post-ack one back 2 s.
  Requests already queued before the close aren't held, and a snapshot started less than 2 s before
  the ack still delays the post-ack snapshot until 2 s after that start, as before.

#### Freshness
Four separate facts, never mixed:
- **Socket healthy**: subscribed, and a pong or frame within the last 25 s. Transport only.
- **Synced**: see Sync state above. Only a qualifying snapshot sets it; a healthy socket never does.
- **Last sync**: when a snapshot last committed.
- **Last change**: when a price last moved, per game. Display only; it never affects status.

Status is derived from the first three:
- `connecting`: no board yet.
- `live`: socket healthy and synced. A quiet market stays `live` indefinitely.
- `polling`: not `live`, and last sync within 15 s or the scheduler's redial wait running.
- `stale`: neither. `staleSince` is the later of the last `live` moment and the last sync.

#### Failures
| Failure | Detection | Response | Viewer sees |
|---|---|---|---|
| Socket can't connect / closes | Dial or read error | Reconnect at once after 60 s healthy, else with full-jitter backoff (0.5 s → 15 s cap); request a resync after every subscribe ack | `Reconnecting…` |
| Socket half-open | `conn.Ping` every 15 s (5 s timeout) fails, two consecutive resyncs find drift (the snapshot changed a price no entry explains), or the lost-frame rule fires | Force reconnect | Brief blip |
| Socket down, or board needs a resync | Freshness rules | Scheduler polls every 2 s until `live` | `Polling`, then `Stale` if snapshots fail too |
| Snapshot 403 / 5xx / timeout / junk | Status code, `http.Client{Timeout: 5s}`, validation | Keep the last good board; scheduler backs off | Nothing, until stale |
| Neither socket nor snapshot fresh | Freshness rules above | Status `stale` | Red "stale since hh:mm:ss" banner; odds dimmed |
| Process dies | Platform | Restart; `EventSource` reconnects and gets the full board | Brief `Reconnecting…` |

The `/healthz` body carries:
- last snapshot status and time
- socket state and last error
- frames applied and entities skipped
- synced, `resyncFrom`, and content-check misses
- **lag p50/p95**, where lag = receive time − `websocketPublishTimestamp` (includes clock skew; diagnostic only)
- **delay p50/p95**, where delay = `websocketPublishTimestamp` − `createdTime` + socket ping RTT/2 +
  our processing. No cross-clock comparison, so skew can't make it negative. Skipped until the first
  ping, and for frames whose `createdTime` is missing, malformed or after the publish time.

### Fan-out (`sse.go`)
- **Events**: `board` (full, ~10 KB) on connect, then `patch` (changed/removed games) and `status`.
- **Heartbeat**: `status` is re-sent every 15 s. It is a named event with `data`, so it reaches the
  page's listeners; an SSE comment (`: hb`) would not reset the browser watchdog.
- **Handoff**: the Hub holds the current rows and the subscriber set under one mutex.
  - `Publish` updates the rows and enqueues the patch to every subscriber under that mutex.
  - `Subscribe`, under the same mutex, encodes the board from the rows, puts it first in the new
    client's queue, and registers the client.
  - So every client gets its baseline first and every patch after it, with none missed. Sends are
    non-blocking, so the lock never waits on a client.
- **Flushing**: via `http.ResponseController`, with a write deadline.
- **Slow clients**: each client has a 32-message buffer. When it fills, that client is dropped; it
  reconnects and gets a fresh board.
- **Headers**: `text/event-stream`, `Cache-Control: no-cache, no-transform`, `X-Accel-Buffering: no`.

### Page (`web/`)
- **Table**: kickoff date and time (viewer's local zone; blank if a rebuilt event lost it), away @ home,
  spread, total, moneyline. Rows are keyed by game id.
- **Moves**: moved cells flash with ▲/▼ plus colour, not colour alone (▲ = decimal odds up, or the line up
  if only it moved). Suspended cells are greyed and show 🔒 instead of a price.
- **Reconnect**: a `board` event patches the rows already shown, so moves across the gap still flash.
- **Status bar** (`role="status"`): LIVE / Polling / Stale since … / Reconnecting, the last update
  time, and, while LIVE, "est. typical delay ~N ms": delay p50 plus half the browser's round trip
  to us (a `HEAD /healthz` on each `board` and `status` event, 5 s timeout, one at a time), shown
  only once both are measured. A tooltip lists what it includes and excludes.
- **CSP**: CSS inline in `index.html`, JS in `app.js`.
- **Watchdog**: every event, including the 15 s `status`, resets a 35 s timer on the browser's own
  clock.
  - On `EventSource` `error`: show `Reconnecting…`.
  - When the timer fires: show "stale since <last event time>" over the dimmed odds, and reopen the
    `EventSource`. This covers a server the browser can't reach, which can't report its own staleness.
  - A server-reported `stale` renders the same way.
- Works at phone width.

## Build order
- [x] 1. **Install and probe.** Install Go ≥ 1.25 (`winget install GoLang.Go`; 1.25 is needed for
  `testing/synctest`) and cloudflared
  (`winget install Cloudflare.cloudflared`), then `go mod init`. Write the snapshot fetch and the socket
  subscribe. Run the TLS ladder on the snapshot and stop at the first rung that returns 200:
  1. stdlib `crypto/tls`
  2. `refraction-networking/utls` Chrome ClientHello, ALPN `http/1.1`, snapshot client only, keep-alive
  3. `bogdanfinn/tls-client`

  The socket stays on stdlib TLS whichever rung wins.
  - **Result (2026-09-23, home PC, Go 1.27.0, cloudflared 2026.9.1):** rung 1 wins. The stdlib
    snapshot got 200 four times out of four (~180 KB, 125–190 ms). The socket acked in 160–480 ms, and
    `websocketPublishTimestamp` is an ISO-8601 string with 100 ns precision. No uTLS dependency.
    `go run .` reruns the probe until step 5 replaces `main.go`; step 7 re-checks from Fly.
- [ ] 2. **Capture fixtures** into `testdata/`: the snapshot, plus real frames covering add, change,
  remove, a line move and a suspension.
  - Take the frame shape from an in-play MLB game, via that league snapshot's `subscriptionPartials`.
  - Confirm on NFL Sunday.
  - **Decide full-object vs partial-merge for `change`**, confirm frames are idempotent, and look for
    per-entity versions or timestamps.
  - Check that `websocketPublishTimestamp` never goes backwards within a subscription.
  - Measure `snapshotLag`: poll the snapshot with starts 2 s apart (the scheduler's spacing) while
    logging frames. For each frame, record the largest `requestStart − receipt` among snapshots that
    still lacked it and held no later value. Each sample can miss the true lag by up to the 2 s
    spacing; frames land at random phases, so the max over many frames closes most of that gap. Set
    the constant to that max + 2 s + margin.
  - **Result (2026-09-23, home PC):** see "Update frames" for the protocol and numbers.
    - Captured: 30 min of NPB in-play (suspensions, market add/remove, game end), ~25 min of MLB
      pre-game (line moves via `replacedSelectionId`) and NFL pre-game.
    - Decided: `change` is an absolute-value partial merge; frames are idempotent; there are no
      per-entity versions; timestamps are monotonic.
    - `snapshotLag` = 8 s.
    - Fixtures: `testdata/{nfl,mlb,npb}-{start.json,frames.jsonl,end.json}`.
    - **Still open:** NFL in-play frames and suspensions (TNF 2026-09-24 or Sunday 2026-09-27), and
      more `snapshotLag` samples there. Rerun `capture_test.go` then, and check this box. From the
      same capture, measure how often a snapshot already holds a change under 0.5 s old: after DK's
      30-min close, the prices the redial missed come from the post-ack snapshot.
- [x] 3. **Board.** Write `board.go` + `board_test.go`.
  - The subcategory is a Board field (NFL 4518, baseball 4519), so the baseball fixtures load too.
  - Replay: for each fixture league, start + every frame gives the same rows as end.
  - Line move via `replacedSelectionId`, including a repeat after the old ID is gone.
  - In-play suspension from the NPB frames: suspended with no prices, then prices again after `add`.
  - Rejected frame (junk, and a forced panic): the board is deep-equal to before.
  - A frame applied twice gives the same board as once.
  - **Result (2026-09-23):** all pass, plus venueRole (home/away, and a skipped event), odds
    normalisation and unknown-ID tests.
    - The MLB fixture's line moves arrive as `remove` + `add` (replay covers them). Its `change` frames
      with `replacedSelectionId` are all resends whose old ID is already gone, so the move test
      rewrites one into a move off a held selection, sent both as a `change` and as an `add`.
    - Review fixes: a replacement `add` inherits omitted fields; merge follows the keys sent, so an
      explicit `null` clears a field; a frame without the full envelope is rejected. Each fix's
      test fails when the fix is reverted.
    - `-race` needs cgo; WinLibs gcc is installed (2026-09-23), and `go test -race ./...` passes.
- [x] 4. **Feed.** Write the rest of `dk.go` + `dk_test.go`.
  - **Two kinds of test.** Timing tests run under `testing/synctest` with in-memory connections: the
    snapshot and socket clients get a `Transport.DialContext` that returns one end of a `net.Pipe`,
    served in-process by the fake handlers. Real sockets would stop fake time from advancing. A few
    separate integration tests use `httptest` over real sockets, for the handshake and TLS only, with
    no synctest and no timing assertions.
  - Fake snapshot: 403, garbage, or hanging past the 5 s timeout.
  - Fake socket: `websocket.Accept`, sending frames and then dropping.
  - Assert the board is kept and the status goes live→polling→stale→live.
  - Reconnect: the snapshot request is recorded after the subscribe ack, never before.
  - Redial after DK's 30-min close, with the fake acking in 300 ms: the missed price arrives with the
    post-ack snapshot at +0.3 s, `live` at +8.3 s. A hung redial releases snapshots at 1 s, a refused
    one at once. A stale board gets no wait. A resync falling due neither flashes `stale` nor jumps
    ahead of the ack. A lagging post-ack snapshot doesn't undo the new socket's frame.
  - Overlay: a snapshot cut before a frame → after commit, the frame's value wins.
  - Old frame, stale snapshot: F sets 110 at t=0, a snapshot requested at t=`snapshotLag`+1 returns
    100 → the board keeps 110, a counted miss, not `live`.
  - Persistent stale snapshot: the same, with every snapshot returning 100 for 60 s → the board keeps
    110 and is never `live` on that subscription; the lost-frame rule reconnects ~10 s after the
    first miss; the post-ack snapshot sets 100 and goes `live`.
  - Rejected later frame: F sets 110 at t=0; G sets 120 at t=1 and is rejected. A snapshot requested
    at t=1.5 holding 120 → the board shows 110, not `live`. A snapshot requested at `1 + snapshotLag`
    holding 120 → the board shows 120, `live`, and F's field is gone.
  - Superseded evidence survives a failed commit: the same, but the second snapshot fails validation
    → F's field is kept.
  - Partial merge: F sets a selection's price at t=0, G changes only another field of it at t=2, a
    frame rejected at t=1 → the price keeps F's time, and a snapshot cut after t=1 drops the price but
    keeps G's field. The same with G an `add` moving the selection to a new ID. And with the
    selection missing from the snapshot → it is rebuilt with G's field and no price.
  - Lost update: frame F on the old subscription, a newer G missed during the disconnect, the recovery
    snapshot holds G → the board shows G.
  - Pre-ack snapshot: the fake serves data cut before the ack → it commits, status stays `polling`;
    the next post-ack snapshot → `live`.
  - Content check: a snapshot missing an older entry's value → a counted miss, not `live`.
  - Unsynced on a healthy socket: reconnect with pongs flowing and every snapshot failing → never
    `live`, `stale` after 15 s. The same after a rejected frame.
  - Scheduler: a burst from every trigger at once gives one request in flight, starts ≥ 2 s apart,
    and backoff after failures.
  - Freshness: a synced, healthy socket with no frames for 5 min stays `live`.
  - **Result (2026-09-23):** `dk.go` + `dk_test.go`; `go test -race ./...` passes, five times over.
    - The sync rules are handler methods (`onAck`, `onFrame`, `onSnapshot`, …) that the apply
      goroutine calls, so the ordering tests drive them directly with explicit times. The goroutine
      tests run under synctest over `net.Pipe` (HTTP and the websocket both), and
      `TestRealSockets` covers TLS and the handshake over `httptest`.
    - Covered: every scenario above; plus a half-open socket (pings unanswered → reconnect at
      15 + 5 s), a removed market a stale snapshot brings back, drift, and replaying the real
      captures with evidence (zero misses).
    - Each rule's test fails when the rule is removed (overlay, dropping superseded evidence,
      evidence reset on ack, lost-frame, drift, the cut check, normalisation, polling while not
      `live`, resync on rejection, pongs counting as heard, the ack ending backoff).
    - Smoke run against DK from the home PC: `live` 8 s after the ack (the `snapshotLag` wait),
      32 games, zero misses.
    - Follow-up (2026-09-24): the redial after DK's 30-min close (`TestRedial`). The old code fails 5
      of its 7 cases, and removing any part of the change fails at least one.
- [x] 5. **Server and page.** Write `sse.go`, `main.go` and `web/`.
  - Handoff: publish concurrently with many subscribes under `-race`. Every client's first message is
    `board`, and applying its patches in order gives the final board.
  - Heartbeat (synctest): an idle stream emits `event: status` with `data` every 15 s.
  - **Result (2026-09-23):** `sse.go`, `main.go` (routes, CSP, `BaseContext` so shutdown ends SSE
    streams), `web/index.html` + `web/app.js`; `go test -race ./...` passes, three times over.
    - Tests: handoff (fails if the board and registration are split across two locks), slow-client
      drop, heartbeat timing under synctest, routes + CSP, and no HTML sinks in `app.js`.
    - Empty board encodes `rows: []`, not `null`, so a browser connecting before the first snapshot
      doesn't throw.
    - `web_test.js` (run by `TestAppJS` when `node` is on PATH) drives `app.js` with a fake DOM,
      EventSource and clock. It checks three things: an empty bootstrap board is accepted; retries
      that keep failing still go stale 35 s after the last event, since only events reset the
      watchdog; and a silent open stream is reopened every 35 s.
    - Home run against DK: `live` 8 s after the ack, 32 games, `/healthz` synced with zero misses.
      At 375 px the table fits with no page scroll. Forced checks in the browser: watchdog stale banner
      with dimmed odds, ▲/▼ flashes, suspended lock.
- [x] 6. **Local soak.** `go run .` on the home PC with a browser on `localhost:8080`, for 1 h.
  - Memory: process RSS (`Get-Process`) at the start, every 15 min and at the end stays flat.
  - Reconnects: `/healthz` `subscriptions` doesn't climb on its own, and the browser's `/events`
    requests stay at one.
  - Heartbeats: the idle stream shows a `status` event every 15 s, and the page never goes stale
    while the server is up.
  - Outage recovery: Verification 4 during the soak. Network off 60 s, then back on. For the process
    kill, `go run .` has no supervisor: kill it (`Stop-Process -Name dk-nfl-live`), restart it by hand,
    and check the already-open page returns to live without a reload. Automatic restart is a hosted
    check (step 7).
  - A quiet pre-game market doesn't count as NFL in-play validation; step 2 stays open for that.
  - **Results (2026-09-23, 13:05–14:05, follow-up to 14:48, network-off check at 18:23).**
    - Memory is flat within each run: 59–61 MB before the kill, 23–25 MB after the restart.
    - `rejected` 0, `misses` 0 and `skipped` 0 throughout. The page never went stale while the
      server was up. `status` heartbeats arrive every 15 s.
    - Process kill: pass. Killed at 13:35:34 and restarted by hand at 13:36:20. The open page kept
      all 32 rows, went stale, reopened its stream through the watchdog, and was LIVE at 13:36:34
      with no reload.
    - DK closes the socket (EOF) after exactly 30 min: 13:34:47, 14:06:26 and 14:36:26. Each time
      the server polled for about 9 s, then went live on a new subscription. That is the only reason
      `subscriptions` climbs; it isn't a reconnect loop. The page showed Polling and kept its stream.
    - Network off: pass. DK was unreachable 18:23:50–18:25:27 (about 98 s, logged every second). The
      page went stale at 18:24:05 ("Stale since …", prices dimmed) and kept all 32 games. DK was
      reachable again at 18:25:28; the server resubscribed once (`subscriptions` 10 → 11), polled,
      and the open page was LIVE at 18:25:37.8, about 10 s later, with no reload.
- [ ] 7. **Deploy to Fly and go public.**
  - `fly.toml`: `primary_region="yyz"`, `internal_port=8080`, `force_https`,
    `auto_stop_machines="off"`, `min_machines_running=1`, a `/healthz` check, `shared-cpu-1x` 256 MB,
    one machine.
  - The user runs `fly auth login` and billing; then `fly deploy`.
  - Probe DK from Fly: `/healthz` on the first deploy shows the snapshot status and socket state from
    `yyz`. If the snapshot 403s there, fall back to Render Starter (Ohio, Docker, health path
    `/healthz`) with the same image; Render sets `PORT`.
  - **Result (2026-09-24, Fly `yyz`, image 3.9 MB):** DK refuses Fly. The snapshot returns 403 and
    the socket handshake returns 403 too, while both work from the home PC. The socket has no TLS
    check, so this is IP or ASN blocking. Another TLS rung won't fix it. The machine is stopped;
    next is Render Ohio.
  - **Result (2026-09-24, Render Ohio Free):** half works. The socket subscribes and frames
    arrive (lag p50 ~20 ms, RTT ~37 ms), but every snapshot returns 403, so the board never
    bootstraps and stays `connecting`. The socket being accepted means Render's IPs aren't hard
    blocked like Fly's; the snapshot's Akamai check rejects this client from this network (it passes
    from home). Next is TLS ladder rung 2 (uTLS Chrome ClientHello, snapshot client only).
  - **Rung 2 tried and dropped (2026-09-24, from home):** every uTLS ClientHello (Chrome, Firefox,
    Safari, even Node's own, replayed) got 403 over HTTP/1.1, while plain Go got 200 over HTTP/2.
    It was HTTP/1.1, not TLS: Akamai 403s an HTTP/1.1 request without `Connection: keep-alive`,
    which Node sends and Go doesn't. Stdlib TLS with Node's exact request got 200; removing that one
    header made it 403. Fix, no new dependency: `dkClient` is HTTP/1.1 only (a fresh `Transport`;
    a `DefaultTransport` clone still offers h2 in ALPN) and the snapshot sends
    `Connection: keep-alive`, matching Node `fetch`, which reportedly passes from Render Ohio.
    Live from home with it. Next: push and check Render's `/healthz`.
  - From a phone on cellular: the board streams and prices move; then airplane mode for 60 s →
    stale banner, and back → live.
  - Automatic restart: kill the process on the machine; the platform restarts it on its own and an
    open page returns to live without a reload.
  - Repeat the step-6 soak through the public URL, to cover Fly's proxy (buffering, idle timeouts).

## Verification
1. `go vet ./...` and `go test -race ./...` pass.
2. The local page matches DK's NFL page side by side. `/healthz` shows `live`, snapshot 200 and
   socket subscribed.
3. A line move flashes about as fast as DK's own page. Lag p50 is under 50 ms (PC clock skew vs DK
   was ~40 ms).
4. **Chaos**:
   - Network off for 60 s: the stale banner shows over the last-good odds, never a blank board.
   - Network back on: live again within 25 s (socket backoff ≤ 15 s, then a snapshot cut after
     the ack). Prices move as soon as the socket is back.
   - Process killed: once it's back (restarted by hand locally, by the platform when hosted), the
     open page returns to live without a reload.
5. The Fly URL works on a phone on cellular, including recovery after an outage.
6. 1 h soak, locally and again through Fly: no reconnect loop, memory flat, heartbeats every 15 s.
7. The same checks pass on Fly (or on Render as the fallback).

## Deliberately skipped (add when needed)
- **Odds history / ClickHouse.** Add it as a background batch writer hooked where `Feed` publishes.
  - It runs on a separate host, with batched HTTP inserts.
  - It drops or retries its own batches and never blocks the feed.
  - The page keeps reading from memory, so it adds 0 ms to the live path.
- **SSE replay / `Last-Event-ID`.** The board is ~10 KB; add replay if it grows.
- **Frontend framework.** Add one if the UI outgrows a single table.
- **Config system.** Add it when a second league or region is real.
- **`render.yaml`.** Only needed if the Fly probe fails.
- **Second Fly machine.** Add it for zero-downtime deploys.
