# dk-nfl-live

A public web page showing DraftKings' current NFL main-line odds (moneyline, spread, total).
It updates itself as lines move and survives DK being down or returning anything unexpected.

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
accepted the snapshot in the tested working configuration: Node's TLS ClientHello plus `br` in
`Accept-Encoding`, which is why
the snapshot client uses uTLS (`tlshello.go`).

```bash
go vet ./...
go test -race ./...
```

`-race` needs cgo (gcc on Windows); without it, run `go test ./...`. Tests use captured DK payloads
in `testdata/` and never contact DK. The frontend tests need Node.js on `PATH`; without it, `go test`
skips them.

## Adding a league, sport, or sportsbook

The project ships an agent skill, [`extend-board`](.claude/skills/extend-board/SKILL.md).
`AGENTS.md` points coding agents (Codex, Cursor, Claude Code and others) to it, so asking for the change directly (e.g. "add the CFL") is enough. Claude Code
also loads it automatically and runs it as `/extend-board`. It covers:

- which code is DK- or NFL-specific and which is generic (`Row`/`Status` → `Hub` → page),
- capturing real DK payloads into `testdata/` before writing any code,
- step-by-step recipes for **another league**, **a new sport** (three-way markets, events without
  home/away) and **a second sportsbook**, each with the invariants from `AGENTS.md` it must keep,
- known gotchas per sportsbook, league and sport ([`gotchas.md`](.claude/skills/extend-board/gotchas.md)).

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
  socket delivered are laid over each snapshot so it doesn't roll a price back, unless a resync
  supersedes them (see `docs/sync.md`). Scores come only
  from socket frames (snapshots don't carry them) and are laid over snapshots the same way.
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
- **Plain HTML and JS.** One table patching rows by ID needs no framework or build step.
- **Hosting on Render.** Fly.io Toronto (`yyz`) was the first choice, closest to DK's edge
  and Ontario viewers. Testing it on 2026-09-24 showed DK returns 403 to Fly for both the snapshot
  and the socket. The same image runs on Render Ohio instead, at a cost of roughly 5 ms end to end.
  Render isn't blocked; its snapshot requests passed Akamai in the tested working configuration, a
  Node-like TLS handshake with `br` offered.

## How fresh are the odds?

When the page says **LIVE**, most of a price's delay is inside DK: a median **41–220 ms** from DK
creating a change to it leaving DK's socket. The network to us and on to you adds a few to ~20 ms,
and our server's processing about 0.2 ms per frame.

Measured 2026-09-23 from Toronto, using DK's timestamps in captured frames and `/healthz`:

| # | Stage | Time |
|---|---|---|
| 1 | DK: `createdTime` → `receivedTime` | p50 7–8 ms · p95 9 ms |
| 2 | DK: `receivedTime` → `publishedTime` | p50 3–164 ms · p95 7–828 ms (in-play fastest; NFL pre-game 10 / 659 ms) |
| 3 | DK: `publishedTime` → WebSocket publish | p50 24–30 ms · p95 38–113 ms |
| | **Inside DK (1–3)** | **p50 41–220 ms · p95 88–950 ms** (NFL pre-game 48 / 690 ms) |
| 4 | DK socket → our server | ~5–20 ms (estimated from round trips; clock skew hides one-way time) |
| 5 | Decode, apply, diff, encode the patch | ~0.19 ms per frame (`BenchmarkFrame`, NFL fixture, 32 games, desktop CPU) |
| 6 | Hub → browser connections | not measured (non-blocking queue, immediate flush) |
| 7 | Server → browser | a few ms to nearby viewers |
| 8 | Browser updates one row | not measured (patches one row, no re-render) |

- NFL figures come from only 14 pre-game frames.
- DK sends each side of a market separately, up to ~100 ms apart, so one side can move first.
- The status bar's "est. delay ~N ms" is the median of stages 1–3 and 5, plus half the round trips
  for stages 4 and 7. It leaves out stages 6 and 8 and uneven network paths, and never compares two
  clocks, so skew can't distort it.

When the page is not LIVE:

| Status | How old a price can be |
|---|---|
| **Polling DK** after DK's 30-min socket close | Still per frame; shows Polling ~8.5 s until a snapshot confirms the board |
| **Polling DK**, socket down | Usually ≤ ~2.3 s (2 s poll + fetch); up to ~6.5 s if DK's snapshot lags |
| **Stale since hh:mm:ss** | Anything since that time; prices dimmed |
