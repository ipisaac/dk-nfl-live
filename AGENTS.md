# AGENTS.md

## Project Overview
A public web page showing DraftKings' current NFL main-line odds (moneyline, spread, total).
It updates itself as lines move and survives DK being down or returning anything unexpected.

## Priorities
- Minimize line-to-screen latency.
- Preserve a usable board through DK failures.

## Stack
- **Go**, a single binary, stdlib first. Only add a dependency with its reason in README's "Why this approach".
- **Frontend**: plain HTML/JS via `//go:embed`.
- **State**: in memory only.
- **Hosting**: Render, Docker.
- **Config**: `PORT` is the app's only env var.

## Rules

### Latency
- DK's push socket is the source of truth. REST snapshot is for bootstrap, resync and fallback.
- Process every frame immediately; publish changed rows with no batching, debounce or throttle anywhere between DK and the browser.
- Feed → Board → Hub path never does disk or network I/O, and never holds a lock across I/O.
- SSE frames are flushed immediately. The browser patches single rows, never re-renders the whole table.

### Correctness
- Home/away comes from `participants[].venueRole` only. Never from position, never guess.
- American odds from DK use U+2212 `−`. Normalise it to `-`.
- A suspended market renders as suspended, never as a live price.
- A snapshot never undoes a value the current subscription delivered, unless a rejected frame, or a frame part
  skipped for an unknown ID, may have superseded it. Subscribe before fetching; overlay the current subscription's evidence on every
  snapshot, never an older subscription's.
- Before changing feed/sync logic, read `docs/sync.md`. `live` requires both a healthy socket and a synced board.

### Robustness
- Every network call has a timeout.
- Never replace a good board with a bad one. A snapshot that fails validation, or is empty while
  we hold games, is rejected. Snapshots and frames commit atomically; a rejected one leaves the
  board untouched.
- Log failures, never silently turned into empty results.
- Label old data as stale (banner, dimmed), never blank.
- `/healthz` returns 200 whenever the process is up. A DK outage must not trigger a platform restart.
- Don't hammer DK: one socket, and every snapshot request goes through one scheduler (single-flight, starts ≥ 2 s apart).

### Security
- DK strings reach the DOM through `textContent` only.
- Keep the CSP `default-src 'self'` (plus inline styles).
- Never commit cookies or request headers from captures. `testdata/` holds response bodies only.

### Working
- Test against real captured DK payloads in `testdata/`.
- Time-dependent tests use `testing/synctest` over in-memory connections (`net.Pipe`), never real
  sockets or wall-clock sleeps. Real-socket tests are separate integration tests with no timing assertions.
- Probe from the affected host before claiming DK is unreachable.
- To add a league, sport or sportsbook, follow `.claude/skills/extend-board/SKILL.md`.
- Only the user performs account logins, billing, account creation, and pushing to git hosts.
- Keep comments minimal and purposeful. Do not add comments that restate what the code does.
- Prefer self-explanatory code, clear variable names, and small functions over comments.

## Commands
```bash
go vet ./...
go test -race ./...
go run .
CAPTURE_LEAGUE=88808 CAPTURE_FOR=3h go test -tags capture -run TestCapture -timeout 0 -v
```

Live capture tool: Connects to DK and writes to `testdata/`. Plain `go test` never contacts DK.
```powershell
$env:CAPTURE_LEAGUE='88808'; $env:CAPTURE_FOR='3h'; go test -tags capture -run TestCapture -timeout 0 -v
```