---
name: extend-board
description: Use when adding a second league, a new sport, or a second sportsbook to this odds board (dk-nfl-live), or when changing which markets it shows. Maps what is DraftKings/NFL-specific, what is generic, and the steps and invariants for each kind of extension.
---

# Extending the board

`AGENTS.md` (loaded through `CLAUDE.md`) applies to every new feed, not just DK's. Where a recipe here conflicts with it, `AGENTS.md` wins. Change it first, and with the user's agreement.

## What is specific and what is generic

| Layer | File | Specific to |
|---|---|---|
| Snapshot + socket client, sync, freshness | `dk.go` | DK, one league (`leagueID`, `subcategoryID`, `nflSubscription`, `subscribeID`) |
| TLS fingerprint for Akamai | `tlshello.go` | DK's CDN |
| DK wire types → `Row` | `board.go` (`event`/`market`/`selection`, `rebuild`, `marketKinds`) | DK's schema. `marketKinds` is the sport-specific market-name map |
| `Row`, `Price`, `Sides`, `OverUnder`, `Status` | `board.go`, `dk.go` | Nothing. This is the contract between a feed and the rest of the app |
| Fan-out to browsers | `sse.go` (`Hub`) | Nothing, apart from one board and one status per Hub |
| Page | `web/` | Two-way markets (`MARKETS` in `app.js`), the "DraftKings NFL" title |

The seam is `Feed.OnPatch(rows []Row, removed []string)` + `Feed.OnStatus(Status)`, wired in `main.go`. Anything that calls those two with correct `Row`s and honest `Status` plugs into the `Hub` and page unchanged.

## Always first: capture real payloads

Tests run against real captures, never against guessed payloads. For DK:

```bash
CAPTURE_LEAGUE=<leagueId> CAPTURE_FOR=30m go test -tags capture -run TestCapture -timeout 0 -v
```

The output goes to `captures/` (gitignored). Copy one start snapshot, the frames and one end snapshot into
`testdata/<league>-start.json`, `<league>-frames.jsonl` and `<league>-end.json`, then add the
league's subcategory to `fixtureSubcategory` in `board_test.go`. Response bodies only, never
cookies or headers. Capture while the league has games on the board, and in-play if possible.

To find IDs, open the league on sportsbook.draftkings.com and watch
`/api/sportscontent/dkcaon/v1/leagues/<id>` in the network tab. The snapshot's
`subscriptionPartials["league-events-<id>"]` holds the exact socket query and the main-line
subcategory ID.

## Recipe A: another league of a sport already supported (e.g. CFL, NCAAF)

1. Capture fixtures (above). Check that the market names appear in `marketKinds` and the outcome types are `Away/Home/Over/Under`.
2. Turn the league constants into a value: a `league{Name, ID, Subcategory string}` passed to `newFeed`, used for `snapshotURL`, the subscription query, `NewBoard(subcategory)` and a per-league `subscribeID`. Build the query the same way `nflSubscription` does, or read it from the snapshot's `subscriptionPartials` as `capture_test.go` does.
3. One `Feed` + one `Hub` per league, e.g. `/events/nfl`, `/events/cfl`, and one entry per league in `/healthz`. A single Hub has one `Status`, and one league's outage must not mark another stale.
4. Page: pick the league from the path or a tab and point `EventSource` at that league's stream.
5. Keep one DK socket (`AGENTS.md`). Send one `subscribe` per league on it, each with its own `id`, and route frames to the league's Board by `id`. Verify with a capture that DK accepts several subscriptions on one socket. If it doesn't, stop and ask before running more sockets. Snapshot requests for every league share the one scheduler (single-flight, ≥ 2 s apart).

## Recipe B: a new sport

Everything in Recipe A, plus:

- Add the sport's market names to `marketKinds` (baseball's `"Run Line"` → `spread` is the model). Only map main lines.
- **Three-way markets** (soccer, hockey regulation): `Sides` has no draw. Add a `Draw *Price` (or a `ThreeWay` type) to `Row`, fill it from the `"Draw"` outcome type in `rebuild`, include it in `samePrices`, and add a column in `web/app.js` `MARKETS` and `index.html`. Never drop the draw and show a two-way price.
- **Events without Home/Away** (tennis, golf, MMA): `rebuild` skips any event without both `venueRole`s, and must never guess. Define what those rows are before you build them. Don't reuse Home/Away by position.
- Add a board test from the new fixtures: the snapshot builds the expected rows, frames apply, the end snapshot matches.

## Recipe C: a second sportsbook

- New files `<book>.go` (client + sync) and `<book>_board.go` (wire types → `Row`). Don't generalise `Board`: DK's `event/market/selection` merge model is DK's. Share only `Row`, `Status`, `Hub` and the page.
- Carry over the invariants, each with a test against that book's captured payloads:
  - Push source is the source of truth if the book has one. REST is for bootstrap, resync and fallback, subscribed before fetching.
  - Every request has a timeout. One connection. Snapshots go through one single-flight scheduler.
  - Atomic apply. A payload that fails validation, or an empty snapshot while we hold games, changes nothing.
  - Home/away from an explicit field only. Normalise odds text (e.g. U+2212 → `-`). Suspended stays suspended.
  - `Status` is `live` only with a healthy stream *and* a synced board. Otherwise `polling` or `stale`, never blank.
- Row IDs must not collide across books. Prefix them (`dk:123`), or give each book its own Hub/stream.
- For side-by-side books on one row you need a shared game key (teams + start time). That is a new matching layer. Build it only when that view is actually wanted, and never match on team-name guesses without a test.
- Probe the book from the Render host before deciding it is blocked. See the README's hosting notes on Fly vs Render and TLS.
- A new dependency needs a reason written down (`AGENTS.md` → Stack).

## Done means

```bash
go vet ./...
go test -race ./...
```

Both must pass, the new fixtures must be exercised, `README.md` must describe the new league/sport/book, and `go run .` must show it live at <http://localhost:8080>.
