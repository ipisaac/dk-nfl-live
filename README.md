# dk-nfl-live

A live board of DraftKings Ontario's NFL mainline odds (spread, total, moneyline). Prices update
as DraftKings pushes a change. When DK is slow, down or sends something unexpected, the page
keeps the last good odds on screen and labels them stale.

## How to run locally

Requires Go 1.27+.

```bash
go run .
```

Open <http://localhost:8080>. `PORT` is the only setting (default `8080`).

| URL | Returns |
|---|---|
| `/` | The page |
| `/events` | The SSE stream the page reads |
| `/healthz` | JSON health (state, socket, snapshot errors, delay). 200 when the process is up |

If the board doesn't load, check `/healthz`. A `snapshotErr` with 403 means the snapshot request
was refused; it does not identify whether the cause is the network, TLS or HTTP request. A healthy
socket cannot bootstrap the board without a successful snapshot. From Render, DK's CDN (Akamai)
accepts the snapshot only with Node's TLS ClientHello plus `br` in `Accept-Encoding`, which is why
the snapshot client uses uTLS (`tlshello.go`).

```bash
go vet ./...
go test -race ./...
```

`-race` needs cgo (gcc on Windows); without it, run `go test ./...`. Tests use captured DK payloads
in `testdata/` and never contact DK.

## Adding a league, sport or sportsbook

The project ships a Claude Code skill, [`extend-board`](.claude/skills/extend-board/SKILL.md). In
Claude Code, run `/extend-board` or ask for the change directly (e.g. "add the CFL"). The skill
loads automatically for these requests. It covers:

- which code is DK- or NFL-specific and which is generic (`Row`/`Status` → `Hub` → page),
- capturing real DK payloads into `testdata/` before writing any code,
- step-by-step recipes for **another league**, **a new sport** (three-way markets, events without
  home/away) and **a second sportsbook**, each with the invariants from `AGENTS.md` it must keep.

Without Claude Code, read the same file as a checklist.

## How it works

```
DK REST snapshot  ── bootstrap, 60 s resync, 2 s polling when needed ─┐
                                                                      ├─► Feed ─► Board ─► Hub ══ SSE ══► browsers
DK push WebSocket ── every price change, as it happens ───────────────┘   dk.go   board.go  sse.go         web/
```

- **Feed (`dk.go`)** holds one WebSocket to DK, subscribed to the NFL main markets. The socket only
  sends changes, so a REST snapshot provides the starting board. All snapshot requests go through one
  scheduler (one in flight, ≥ 2 s apart, backoff on failure).
- **Board (`board.go`)** keeps DK's events, markets and selections in memory and builds one row per
  game. Every frame and snapshot is validated as a whole or not at all. A bad one leaves
  the board untouched and triggers a resync. Since DK's snapshot can trail the socket, values the
  socket delivered are laid over each snapshot so it never rolls a price back.
- **Hub (`sse.go`)** encodes each change once and sends it to every browser. A new browser gets the
  full board, then patches. A browser that falls 32 messages behind is dropped and reconnects.
- **Page (`web/`)** patches only the rows that changed. The status bar shows LIVE, Polling DK,
  Reconnecting or "Stale since hh:mm:ss". If it hears nothing for 35 s it marks the odds stale and
  reconnects on its own.

## Why this approach

- **DK's WebSocket instead of polling.** DK's snapshot is cached (`max-age=1`) and sometimes lags the
  socket by 0.5–4.2 s. The socket delivers a change tens of ms after DK publishes it.
- **No batching.** Every DK frame goes straight to browsers, with no disk or network I/O on the way.
- **Go.** One small static binary, a goroutine per viewer, and a stdlib covering HTTP, JSON and TLS.
  Two dependencies: `coder/websocket`, and `refraction-networking/utls` so the snapshot's TLS
  handshake matches Node's, which Akamai requires from Render.
- **SSE to the browser.** Data flows one way. SSE is plain HTTP, and `EventSource` reconnects by
  itself with no client library.
- **Memory only.** 32 games rebuild from DK in about a second after a restart.
- **Plain HTML and JS.** It's one table; patching rows by ID needs no framework or build step.
- **Hosting on Render (Ohio).** Fly.io Toronto (`yyz`) was the first choice, closest to DK's edge
  and Ontario viewers. Testing it on 2026-09-24 showed DK returns 403 to Fly for both the snapshot
  and the socket (an IP block, since both work from a home connection). The same image runs on
  Render Ohio instead, at a cost of roughly 5 ms end to end. Render isn't blocked, but its
  snapshot requests pass Akamai only with a Node-like TLS handshake and `br` offered. It runs on
  Render's free instance, which sleeps after 15 minutes without incoming requests; an uptime
  monitor requests `/healthz` every 5 minutes to keep it awake.

## How fresh are the odds?

When the page says **LIVE**, a price typically reaches the screen **50–250 ms after DK creates the
change**. Almost all of that is inside DK; this app adds under 1 ms.

Measured 2026-09-23 from Toronto, using DK's timestamps in captured frames and `/healthz`:

| # | Stage | Time |
|---|---|---|
| 1 | DK: `createdTime` → `receivedTime` | p50 7–8 ms · p95 9 ms |
| 2 | DK: `receivedTime` → `publishedTime` | p50 3–164 ms · p95 7–828 ms (in-play fastest; NFL pre-game 10 / 659 ms) |
| 3 | DK: `publishedTime` → WebSocket publish | p50 24–30 ms · p95 38–113 ms |
| | **Inside DK (1–3)** | **p50 41–220 ms · p95 88–950 ms** (NFL pre-game 48 / 690 ms) |
| 4 | DK socket → our server | ~5–20 ms (estimated from round trips; clock skew hides one-way time) |
| 5 | Decode, apply, diff, encode the patch | ~0.18 ms (benchmark, 32-game board) |
| 6 | Hub → browser connections | µs (non-blocking queue, immediate flush) |
| 7 | Server → browser | a few ms to nearby viewers |
| 8 | Browser updates one row | < 1 ms |

- NFL figures come from only 14 pre-game frames; in-play NFL is still to be measured.
- DK sends each side of a market separately, up to ~100 ms apart, so one side can briefly move first.
- The status bar's "est. typical delay ~N ms" is the median of stages 1–3 plus half the round trips
  for stages 4 and 7, plus stage 5. It never compares two clocks, so skew can't distort it.

When the page is not LIVE:

| Status | How old a price can be |
|---|---|
| **Polling DK** after DK's 30-min socket close | Still per frame; shows Polling ~8.5 s until a snapshot confirms the board |
| **Polling DK**, socket down | Usually ≤ ~2.3 s (2 s poll + fetch); up to ~6.5 s if DK's snapshot lags |
| **Stale since hh:mm:ss** | Anything since that time; prices dimmed |
