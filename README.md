# dk-nfl-live

A web page showing DraftKings Ontario's (`dkcaon`) current NFL main-line odds: spread, total and
moneyline for every game, no alternate lines. It updates each price the moment DraftKings (DK) pushes a
change. When DK is slow, down or sends something unexpected, the page keeps the last good odds on
screen and labels them stale instead of going blank.

One Go binary serves both the feed and the page. It needs no database, build step or config file.

## Run it locally

**Requirements:**
- **Go 1.27** or newer (`winget install GoLang.Go` on Windows). `go.mod` sets the version.
- **A network DK serves.** Everything so far was verified from a home connection in Toronto.
  DK's CDN (Akamai) rejects some clients and IP ranges. If the snapshot gets a 403, see
  "Troubleshooting" below.

```bash
go run .
```

Open <http://localhost:8080>. The board fills within about a second and shows **LIVE** about 8 s
later (see "How fresh are the odds?" for why).

| URL | What it returns |
|---|---|
| `/` | The page |
| `/events` | The Server-Sent Events stream the page reads |
| `/healthz` | JSON health: state, sync, socket, frame counters and DK lag. Returns 200 whenever the process is up |

`PORT` is the only setting. It defaults to `8080`:

```bash
PORT=9000 go run .
```

**Tests:**

```bash
go vet ./...
go test -race ./...
```

- `-race` needs cgo, so on Windows it needs gcc (for example WinLibs). Without gcc, run
  `go test ./...`.
- `TestAppJS` runs the page's script against a fake DOM when `node` is on `PATH`, and is
  skipped otherwise.
- Every test runs against real DK payloads captured into `testdata/`, and none contacts DK.

**Recording fresh DK traffic** (optional). This writes to `captures/`, which git ignores:

```bash
CAPTURE_LEAGUE=88808 CAPTURE_FOR=3h go test -tags capture -run TestCapture -timeout 0 -v
```

### Troubleshooting

`/healthz` shows what the server sees:

- **`snapshotErr` mentions 403.** Akamai rejected the snapshot request. It does that to clients and
  networks it doesn't trust (curl gets a 403 where Go's stdlib client gets a 200). Try another
  network.
- **State stays `polling`.** The socket or the snapshot is failing. `socketErr` and `snapshotErr`
  say which one. `misses` counts snapshots that disagreed with the socket.
- **State is `stale`.** Neither the socket nor the snapshot has delivered fresh data for 15 s. The
  page keeps showing the last good odds, dimmed.

## How it works

```
DK REST snapshot  ── bootstrap, 60 s resync, 2 s polling when needed ─┐
                                                                      ├─► Feed ─► Board ─► Hub ══ SSE ══► browsers
DK push WebSocket ── every price change, as it happens ───────────────┘   dk.go   board.go  sse.go         web/
```

**Feed (`dk.go`).** It opens one WebSocket to DK and subscribes to the NFL main markets. DK's
subscription is the same one DK's own site uses, taken from the snapshot's `subscriptionPartials`.
- **Snapshot.** The socket only sends changes, so the feed also fetches DK's full-league REST
  snapshot to get a starting board. It always subscribes before it fetches, so nothing published
  between the two steps is lost.
- **Scheduler.** Every snapshot request goes through one scheduler: one request in flight at a time,
  starts at least 2 s apart, with backoff after failures. The triggers are bootstrap, reconnect, the
  60 s resync, polling, and a bad frame.
- **Health checks.** The socket is pinged every 15 s. A dead socket reconnects with jittered backoff
  (0.5 s up to 15 s).

**Board (`board.go`).** It holds DK's events, markets and selections in memory and turns them
into one row per game.
- **Applying changes.** DK sends each change as an `add`, `change` or `remove` of one entity. A
  `change` merges only the fields DK sent. A line move replaces a selection with a new ID, and the
  new ID keeps the fields DK didn't resend.
- **All or nothing.** Every frame and every snapshot is validated, then committed whole or not at
  all. A bad frame, or even a panic while applying it, leaves the board untouched and triggers a
  resync. A snapshot that fails validation, or comes back empty while we hold games, is rejected.
- **Home and away** come from DK's `venueRole`, never from list position.
- **Odds.** DK writes negative odds with the Unicode minus `−`, which is normalised to `-`.
- **Suspended markets** show a lock instead of a price.

**Keeping the snapshot and the socket consistent.** DK's snapshot is cached and can trail the socket
by a few seconds, so a snapshot fetched after a frame can still hold the older price.
- **Evidence.** The feed keeps every value the current subscription delivered. When a snapshot
  arrives, those values are laid over it, so a lagging snapshot never undoes a newer price.
- **Synced.** The board only counts as synced when a snapshot cut at least 8 s after the subscribe
  ack (`snapshotLag`) agrees with everything the socket delivered.
- **Live.** The page shows **LIVE** only when the socket is healthy and the board is synced. The
  full rules are in PROJECT_PLAN.md under "Ordering and consistency".

**Hub (`sse.go`).** A single goroutine applies every frame and snapshot, then hands the changed
rows to the Hub.
- **Fan-out.** The Hub encodes each change once and queues it for every connected browser.
- **New clients.** A new browser gets the full board (~10 KB) first, then every patch after it.
  The board and the client list share one lock, so no patch is missed or repeated.
- **Slow clients.** A browser whose 32-message buffer fills is dropped. It reconnects and gets a
  fresh board, so one slow client never delays the others.
- **Heartbeat.** A `status` event goes out every 15 s.

**Page (`web/`).** Plain HTML and JavaScript embedded in the binary.
- **Updates.** A `patch` event updates only the rows it names. Moved prices flash with ▲ or ▼ as
  well as colour.
- **Status bar.** It shows LIVE, Polling DK, Reconnecting or "Stale since hh:mm:ss", plus the
  last update time and DK's lag.
- **Watchdog.** If the browser hears nothing for 35 s (the server is down or unreachable), it marks
  the odds stale and reopens the stream itself. It never needs a reload.
- **Security.** DK strings reach the page through `textContent` only, under a
  `default-src 'self'` Content-Security-Policy.

**How failures look to a viewer:**

| What happened | What the page shows | How it recovers |
|---|---|---|
| DK closes the socket (it does this every 30 min) | Polling DK for ~9 s. Prices keep moving. | It resubscribes, then waits for a snapshot cut after the ack |
| Socket down | Polling DK. Prices refresh from the snapshot every 2 s. | It reconnects with backoff |
| Socket and snapshot both failing | "Stale since …" with dimmed prices. All games stay on screen. | It goes back to LIVE by itself |
| DK sends a malformed frame | Nothing changes | The frame is rejected and the board resyncs |
| Our server restarts | Reconnecting…, then Stale | The page reopens the stream and gets the full board |

A local soak tested these cases for real. A 98 s network outage showed stale odds, never a blank
board, and the page was LIVE again 10 s after the network came back. After a process kill and a
manual restart, the open page recovered without a reload.

## Why this approach

- **DK's push socket, not polling.** DK's snapshot is cached (`max-age=1`), so polling it can
  never be faster than about 1 s. DK's origin also sometimes serves a snapshot 0.5–4.2 s behind the
  socket (1–2% of 1,900 polls). The socket delivers a change tens of milliseconds after DK
  publishes it, so the snapshot is used only for bootstrap, resync and fallback.
- **No batching anywhere.** Every DK frame goes straight through to the browsers. Nothing
  debounces, throttles or batches changes, and nothing on that path does disk or network I/O.
- **Go.**
  - One static binary with a small memory footprint (~25–60 MB during the soak).
  - One cheap goroutine per viewer.
  - A standard library covering HTTP, JSON, TLS, logging and file embedding.
  - Go's stock TLS client gets through Akamai's check on the snapshot. There's only one outside
    dependency, `coder/websocket`, for the DK socket.
- **Server-Sent Events (SSE), not WebSockets, to the browser.** Data flows one way.
  - SSE is plain HTTP, so it passes through proxies and CDNs.
  - The browser's `EventSource` reconnects on its own.
  - It needs no client library.
- **Memory only, no database.** The board is 32 games. A restart rebuilds it from DK in about a
  second, so there is nothing to persist. Odds history can be added later as a background writer
  that never blocks the live path.
- **Plain HTML and JavaScript.** It is one table. Patching single rows by ID is simpler and faster
  than re-rendering through a framework, and there is no build step.
- **Hosting in Toronto (Fly.io `yyz`).** The server should sit close to DK's Akamai edge (~7.5 ms
  round trip from Toronto) and to Ontario viewers. Render's Ohio region, running the same image, is
  the fallback if DK blocks Fly's IP addresses.
- **Stale instead of blank.** A board with a clear "stale since" label is still useful. An empty
  page is not.

## How fresh are the odds?

**In short:** when the page says **LIVE**, a price is typically on screen about **50–250 ms after
DK's system creates the change**. Almost all of that time is inside DK. This app adds about
**1 ms of processing**, plus the network hops to DK and to your browser.

### Timeline of one price change, with the page LIVE

Measured on 2026-09-23 on a home PC in Toronto, using DK's own timestamps in the captured
frames (`testdata/`) and the running server's `/healthz`.

| # | Stage | Time | Source |
|---|---|---|---|
| 0 | A trader or model decides to move the line → DK creates the message | Unknown | DK exposes no timestamp before `createdTime` |
| 1 | DK internal: `createdTime` → `receivedTime` | p50 7–8 ms · p95 9 ms | Frame metadata, all 3 leagues |
| 2 | DK internal: `receivedTime` → `publishedTime` | p50 3–164 ms · p95 7–828 ms | Frame metadata. In-play NPB baseball is fast (3 / 7 ms); pre-game NFL (10 / 659 ms) and MLB (164 / 828 ms) are slower |
| 3 | DK internal: `publishedTime` → WebSocket publish | p50 24–30 ms · p95 38–113 ms | Frame metadata vs `websocketPublishTimestamp` |
| | **Total inside DK (1–3)** | **p50 41–220 ms · p95 88–950 ms** | NPB in-play 41 / 88 · NFL pre-game 48 / 690 · MLB pre-game 220 / 950 |
| 4 | DK socket → our server (network) | ~5–20 ms (estimate) | Not measurable directly; see below |
| 5 | Decode frame, apply to board, rebuild and diff rows, encode the SSE patch | **~0.18 ms** | Go benchmark over the NFL frames with a 32-game board |
| 6 | Hub → browser connections | Microseconds | A non-blocking queue send, then an immediate flush. No buffering |
| 7 | Server → browser (network) | <1 ms locally; a few ms from Fly `yyz` to a nearby viewer | Local run measured. Hosted not yet measured |
| 8 | Browser: parse the patch, update one row, start the flash | <1 ms (estimate) | One small JSON object and a few `textContent` writes |

**Notes on the timeline:**
- **Stage 4 is hidden by clock skew.** `/healthz` reports `lagP50ms`: our receive time minus DK's
  WebSocket publish time, over the last 1,024 frames. Tonight it read **p50 −34 ms, p95 −22 ms**.
  It is negative because the home PC's clock is at least ~35 ms behind DK's, so skew hides the true
  one-way time. The spread from p50 to p95 (~12 ms) is real network jitter. The ~5–20 ms estimate
  comes from the measured round trips: ~7.5 ms to DK's Akamai edge and ~20 ms to AWS Ohio.
- **The NFL numbers come from few frames.** They are pre-game and only 14 frames. NFL in-play
  frames will be measured on the next game day.
- **The two sides of a market can arrive apart.** DK sends each side of a market (for example the
  over and the under) as its own message, up to ~100 ms apart. So the page can briefly show one
  side moved before the other.

### When the page is not LIVE

The status bar always says which mode it is in:

| Status | How old a price can be | Why |
|---|---|---|
| **LIVE** | As in the timeline above | The socket is healthy and the board is synced |
| **Polling DK**, after a reconnect (DK closes the socket every 30 min) | Still per frame. Changes stream as soon as the new subscription is acked | It shows Polling for ~8–9 s only until a snapshot cut after the ack confirms the board (`snapshotLag` = 8 s) |
| **Polling DK**, socket down | Usually ≤ ~2.3 s: a 2 s poll interval plus a 125–260 ms fetch. Up to ~6.5 s when DK's snapshot itself lags (0.5–4.2 s, in 1–2% of polls) | Prices come from the REST snapshot only |
| **Stale since hh:mm:ss** | Anything since that time; prices are dimmed | Neither source has been fresh for 15 s, or the browser heard nothing from our server for 35 s |

**Recovery times measured in the local soak:**
- **After a 98 s network outage:** LIVE 10 s after connectivity returned.
- **After a process kill and manual restart:** LIVE 14 s after the restart.
- **After each of DK's 30-min socket closes:** about 9 s of Polling, with prices still streaming.

### Reading the status bar

- **"updated hh:mm:ss"** is when your browser last received a price change.
- **"DK lag N ms"** is the server's median receive time minus DK's publish time, which is stage 4
  plus clock skew. A value near zero or negative just means the server's clock trails DK's. Watch it
  for changes, not for its absolute value.
