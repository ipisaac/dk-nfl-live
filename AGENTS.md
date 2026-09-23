# AGENTS.md

## Project Overview
A public web page showing DraftKings' (`dkcaon`) current NFL main-line odds (moneyline, spread, total; no alts).
It updates itself as lines move and survives DK being down or returning anything unexpected.

## Priorities
- Minimize line-to-screen latency.
- Preserve a usable board through DK failures.

## Stack
- **Go**, a single binary, stdlib first. Only add a dependency with reasons in PROJECT_PLAN.md.
- **Frontend**: plain HTML/JS via `//go:embed`. No framework, no build step.
- **State**: in memory only. There is no database.
- **Hosting**: Fly.io `yyz`, with Render (Ohio) as the fallback running the same image.
- **Config**: `PORT` is the only env var. Everything else is a constant.

## Rules

### Latency
- DK's push socket is the source of truth. REST snapshot is for bootstrap, resync and fallback.
- Publish on every frame. No batching, debounce or throttle anywhere between DK and the browser.
- Feed → Board → Hub path never does disk or network I/O, and never holds a lock across I/O.
- SSE frames are flushed immediately. The browser patches single rows, never re-renders the whole table.

### Correctness
- Home/away comes from `participants[].venueRole` only, never from position; never guess.
- American odds from DK use U+2212 `−`. Normalise it to `-`.
- A suspended market renders as suspended, never as a live price.
- A snapshot never undoes a value the current subscription delivered, unless a rejected frame may
  have superseded it. Subscribe before fetching; overlay the current subscription's evidence on every
  snapshot, never an older subscription's.
- Before changing feed/sync logic, read PROJECT_PLAN.md's “Ordering and consistency” and “Freshness” rules. `live` requires both a healthy socket and a synced board.

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
  sockets or sleeps. Real-socket tests are separate integration tests with no timing assertions.
- Probe from the affected host before claiming DK is unrecheable.
- Only the user performs account logins (`fly auth login`), billing, account creation, and pushing to git hosts.
- Keep PROJECT_PLAN.md's build-order checkboxes current.
- Keep comments minimal and purposeful. Do not add comments that restate what the code does.
- Prefer self-explanatory code, clear variable names, and small functions over comments.

## Commands
```bash
go vet ./...
go test -race ./...
go run .
CAPTURE_LEAGUE=88808 CAPTURE_FOR=3h go test -tags capture -run TestCapture -timeout 0 -v
cloudflared tunnel --url http://localhost:8080
fly deploy
```