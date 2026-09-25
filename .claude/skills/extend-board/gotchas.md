# Known gotchas

These come from an earlier multi-book scraper, written in Python and polling REST. Its DraftKings client used a different API from `dk.go`, so check each item against captures before relying on it.

## By sportsbook

### DraftKings
- A wrong league or subcategory ID returns an empty payload or a 404, not an error. Check a new league's IDs against a capture that has games in it.
- On a new host or region, check the event catalogue, not the status code. A Canada-but-not-Ontario exit returns 200 with a different catalogue.
- Baseball: map `"Run Line"`, never `"Spread"`. DK also lists an in-play market called `Spread` at about 4.5 runs, and the shared `"Spread"` entry in `marketKinds` would pick it up. Make `marketKinds` per sport before adding a baseball league.
- Scores: read `eventScore.mainScore.homeScore`/`awayScore`, which only socket frames carry. The
  snapshot's `eventScorecard` labels teams first/second, and its order contradicted `eventScore` in an
  NFL capture (2026-09-24).
- Pregame MLB has one rung per market, and alternates only appear in-play. Capture in-play to test main-line filtering.

### FanDuel
- Read home/away from the fixture name, per event. North American events are `AWAY @ HOME` and the rest `HOME v AWAY`, even within one sport. Reading it backwards mirrors every row.
- The line is in a different place per sport: on the runner's `handicap` for basketball, in the market type for soccer (`OVER_UNDER_25`), and only in the runner label for tennis.
- A non-`OPEN` market or a non-`ACTIVE` runner is suspended.
- The anti-bot token is minted in a real browser and is bound to that browser's IP and user agent. Work out how to mint it from the Render host before building the client.
- On a 403, re-mint the token. Never retry.
- Filter out the simulated leagues ("eBasketball/eFootball H2H GG League"). They sit under the real sport IDs.
- Strip MLB's "(Pitcher)" suffix from team names.

### Pinnacle
- A flat 401 means the public guest key has rotated. Refresh it.
- The main line is the one rung without `isAlternate`.
- At kickoff the game is re-listed under new IDs. Carry the row across, or the live board loses it.
- Single legs of a market get closed. Reading only open markets makes the remaining leg look like a complete market. Negative vig gives it away.
- A 404 from `/related` on a started game means the game is gone, not an error.
- Filter out the "Team (Corners)" mirror fixtures. They arrive in the base league's payload.

### Kambi (Proline+)
- Use the exact offering key the brand's own site uses. A sibling tenant's key returns 200 with shifted prices.
- Lines are in milli-units, so divide by 1000. `oddsAmerican` has no `+`. `SUSPENDED` outcomes have null odds.
- The main rung carries the `MAIN_LINE` tag.
- MLB: map only the plain `Moneyline`. The "(X must start)" variants key identically.
- Match criterion text case-insensitively.

### Hard Rock Bet (Amelco)
- Send the Ontario segment/channel parameters. Without them the site returns 200 with no sports.
- Prices come from a separate ladder, joined on `rootIdx`. Without the ladder, the feed parses as complete but has no prices.
- Its Cloudflare rule 403s every Chrome TLS fingerprint. Build a Firefox hello the way `tlshello.go` builds DK's.
- IDs are around 10^18. Send them to the browser as strings, because `JSON.parse` rounds them.
- Take team sides from the `:A:`/`:B:` infix, not the market name.
- `SPRD` and `AHCP` are the same rungs at different vig. Read only one.
- A league can span several competition IDs (MLB has two).
- Ignore the "NFL Preseason" node in week 1. It repeats the regular-season fixtures.
- Read the period from the `subtype` prefix. The `period` field is sometimes empty.

### Betway
- Every call is a POST with a JSON body. Find the queries by replaying the bodies from the site's JavaScript bundle.
- North American sports put alternates in an `Alternate <X>` market. Soccer puts every rung under one title with no main flag.
- Key markets on `Title`, because the numeric market ID changes per fixture. League IDs aren't numeric.

### Altenar (ToonieBet, Swiper, DAZN Bet)
- Home/away comes from the moneyline odd's `typeId` (1 = home, 3 = away). `competitorIds` follows the name order, which is `AWAY @ HOME`.
- The events list only carries moneyline and total. Spreads need one details call per event.
### ComeOn
- Prices only come over RSocket over WebSocket, with permessage-deflate. Send the franchise/locale query on the upgrade, and join fragmented frames.
- Key markets on `marketType.id`. Names are localised per fixture.
- A league returns at most 30 fixtures, with no pagination.

### PowerPlay (Sportnco)
- `actors[].type` has home and away reversed for North American leagues. Use `competition.display_mode` instead; it varies by league and has changed overnight.
- Its WAF escalates on the total number of requests from an IP. Stop at the first block.

### Data-Streams (Betnova, Maverick)
- A DPI filter rejects some TLS hello shapes. When a connection fails, suspect the client hello before the host.
- Answer Socket.IO pings, or the league arrives truncated. Use the locale `en_EN`.
- The European handicap (id 99) is signed from the away side. Negate it.
- Filter on `line_entity_id`. Goals and corners share market IDs.
- The odds token is bound to the IP and expires after 300 s.

### NeoBet (Agido)
- Basketball: map `FT` (includes overtime), not `RT` (regulation).
- Dedupe markets on `bettingType`.
- An unknown sport string returns 200 with an empty group list.

### BetMGM (Entain)
- Request the full offer mapping. The main-markets mode returns only one market per fixture.
- Fixtures come in two formats (V2 for NFL and V1 for CFL on the same day). Handle both.
- `isMain` doesn't mark the main rung.
- Drop markets that carry `RangeValue`. They collide with the real markets.

### Circa
- The board's odds are all the `999999` sentinel, so get prices from the per-game call. When `999999` appears beside real points, it means the standard −110.

## By league or sport
- NFL: the moneyline is 2-way and includes overtime.
- NFL/MLB: show dates in Eastern time. A 20:15 ET kickoff falls on the next day in UTC.
- MLB: the same two teams can play twice on one date (doubleheaders). Never key a game on teams + date.
- Basketball: use full-time markets (including overtime), not regulation.
- Soccer: expect quarter lines (±0.25, ±0.75).
- Tennis: estimated start times differ by up to an hour between books.
- Neutral sites: strip a trailing "(N)" from team names.

## Cross-cutting
- When a book has no main-line flag, the main line is the rung priced closest to even money.
- Key main-line rows by market and side, never by points, because points move.
- An empty 200 usually means a wrong ID or a missing parameter, not an empty slate.
- Negative vig means a leg is missing.
- Base `live`/`stale` on the last successful fetch or frame, not the last price change. A quiet market isn't stale.
- Keep the user agent, client hints and TLS fingerprint consistent with each other.
- Cross-book matching: match start times within a window (books differ by about 2 minutes), compare prices in decimal, and alias whole team names ("NY Giants"), never city codes (NY, LA and CHI share them).
