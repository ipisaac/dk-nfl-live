package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

var fixtureSubcategory = map[string]string{"nfl": "4518", "mlb": "4519", "npb": "4519"}

var at = time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)

func loadBoard(t testing.TB, league, which string) *Board {
	t.Helper()
	body, err := os.ReadFile("testdata/" + league + "-" + which + ".json")
	if err != nil {
		t.Fatal(err)
	}
	b := NewBoard(fixtureSubcategory[league])
	if _, err := b.ApplySnapshot(body, at); err != nil {
		t.Fatal(err)
	}
	return b
}

// loadFrames returns each captured message's `data`, the input to ApplyUpdate.
func loadFrames(t testing.TB, league string) [][]byte {
	t.Helper()
	raw, err := os.ReadFile("testdata/" + league + "-frames.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	var out [][]byte
	for line := range bytes.Lines(raw) {
		var m socketMessage
		if err := json.Unmarshal(line, &m); err != nil {
			t.Fatal(err)
		}
		out = append(out, m.Data)
	}
	return out
}

func mustApply(t *testing.T, b *Board, f []byte) []string {
	t.Helper()
	changed, unknown, err := b.ApplyUpdate(f, at)
	if err != nil || len(unknown) > 0 {
		t.Fatalf("apply: err %v, unknown %v", err, unknown)
	}
	return changed
}

func withoutUpdatedAt(rows []Row) []Row {
	for i := range rows {
		rows[i].UpdatedAt = time.Time{}
	}
	return rows
}

func TestReplay(t *testing.T) {
	for league := range fixtureSubcategory {
		t.Run(league, func(t *testing.T) {
			b := loadBoard(t, league, "start")
			start := b.Games()
			for _, f := range loadFrames(t, league) {
				mustApply(t, b, f)
			}
			got, want := withoutUpdatedAt(b.Games()), loadBoard(t, league, "end").Games()
			if !reflect.DeepEqual(got, want) {
				t.Errorf("replay != end\ngot  %s\nwant %s", js(got), js(want))
			}
			if reflect.DeepEqual(start, want) {
				t.Errorf("fixture has no row changes between start and end")
			}
			for _, r := range got {
				for _, p := range []*Price{r.Spread.Away, r.Spread.Home, r.Total.Over, r.Total.Under, r.Moneyline.Away, r.Moneyline.Home} {
					if p != nil && !validAmerican(p.American) {
						t.Errorf("%s: american %q", r.ID, p.American)
					}
				}
			}
		})
	}
}

func js(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// frame wraps one part in a full update envelope.
func frame(kind, part string) string {
	parts := map[string]string{"add": "{}", "change": "{}", "remove": "{}"}
	parts[kind] = part
	return fmt.Sprintf(`{"data":{"add":%s,"change":%s,"remove":%s}}`, parts["add"], parts["change"], parts["remove"])
}

// In the MLB fixture, the `change` frames with replacedSelectionId are resends whose old ID is already
// gone (replay covers them). This rewrites the first one into a real move off a held selection, as a
// `change` and as an `add`; both omit marketId and outcomeType.
func TestLineMove(t *testing.T) {
	const oldID, newID = "0OU86432255O750_1", "0OU86432255O800_1"
	var resend []byte
	for _, f := range loadFrames(t, "mlb") {
		var u updateData
		if err := json.Unmarshal(f, &u); err != nil {
			t.Fatal(err)
		}
		if c := u.Data.Change.Selections; len(c) == 1 && c[0].v.ReplacedSelectionID == "0OU86432255O700_1" {
			resend = f
			break
		}
	}
	if resend == nil {
		t.Fatal("resend frame not found")
	}
	change := strings.NewReplacer(
		`"id":"0OU86432255O750_1"`, `"id":"`+newID+`"`,
		`"replacedSelectionId":"0OU86432255O700_1"`, `"replacedSelectionId":"`+oldID+`"`,
		`"points":7.5`, `"points":8`,
	).Replace(string(resend))
	add := strings.NewReplacer(`"add":`, `"change":`, `"change":`, `"add":`).Replace(change)

	for kind, move := range map[string]string{"change": change, "add": add} {
		t.Run(kind, func(t *testing.T) {
			b := loadBoard(t, "mlb", "start")
			old := b.selections[oldID]
			if changed := mustApply(t, b, []byte(move)); len(changed) != 1 {
				t.Fatalf("changed %v, want the moved game", changed)
			}
			got := b.selections[newID]
			if hasKey(b.selections, oldID) || got.MarketID != old.MarketID || got.OutcomeType != old.OutcomeType {
				t.Fatalf("after move: old held %v, new %+v", hasKey(b.selections, oldID), got)
			}
			r, _ := b.Row(b.markets[got.MarketID].EventID)
			if r.Total.Over == nil || *r.Total.Over.Line != 8 {
				t.Fatalf("row over %+v, want line 8", r.Total.Over)
			}

			before := *b
			if changed := mustApply(t, b, []byte(move)); len(changed) != 0 {
				t.Fatalf("repeat changed %v", changed)
			}
			if !reflect.DeepEqual(before, *b) {
				t.Fatal("repeat after the old ID is gone changed the board")
			}
		})
	}
}

func TestExplicitNull(t *testing.T) {
	b := loadBoard(t, "nfl", "start")
	const id = "0HC84695622N1150_3"
	x := b.selections[id]
	mustApply(t, b, []byte(frame("change", `{"selections":[{"id":"`+id+`","displayOdds":null,"trueOdds":null}]}`)))
	got := b.selections[id]
	if got.DisplayOdds != nil || got.TrueOdds != nil || got.MarketID != x.MarketID || toPrice(got) != nil {
		t.Fatalf("after nulls %+v", got)
	}
}

func TestInPlaySuspension(t *testing.T) {
	b := loadBoard(t, "npb", "start")
	id := b.Games()[0].ID
	var suspended, reopened bool
	for _, f := range loadFrames(t, "npb") {
		mustApply(t, b, f)
		r, _ := b.Row(id)
		ml := r.Moneyline
		switch {
		case r.Suspended.Moneyline && ml.Away == nil && ml.Home == nil:
			suspended = true
		case suspended && !r.Suspended.Moneyline && ml.Away != nil && ml.Home != nil:
			reopened = true
		}
	}
	if !suspended || !reopened {
		t.Fatalf("suspended with no prices %v, then priced again %v", suspended, reopened)
	}
}

func TestRejected(t *testing.T) {
	frames := []string{
		`garbage`,
		`null`,
		`{}`,
		`{"data":null}`,
		`{"data":{"add":{},"change":{}}}`,
		frame("change", `{"selections":[{"id":"0ML86426421_3","trueOdds":"abc"}]}`),
		frame("add", `{"markets":[{"eventId":"1"}]}`),
		frame("change", `{"events":[{"status":"STARTED"}]}`),
		frame("change", `{"selections":[null]}`),
	}
	for _, f := range frames {
		b := loadBoard(t, "nfl", "start")
		if _, _, err := b.ApplyUpdate([]byte(f), at); err == nil || strings.Contains(err.Error(), "panic") {
			t.Errorf("%s: want a validation error, got %v", f, err)
		}
		if !reflect.DeepEqual(b, loadBoard(t, "nfl", "start")) {
			t.Errorf("%s: board changed", f)
		}
	}
	for _, s := range []string{`garbage`, `{}`, `{"events":[{"name":"x"}]}`} {
		b := loadBoard(t, "nfl", "start")
		if _, err := b.ApplySnapshot([]byte(s), at); err == nil {
			t.Errorf("snapshot %s: accepted", s)
		}
		if !reflect.DeepEqual(b, loadBoard(t, "nfl", "start")) {
			t.Errorf("snapshot %s: board changed", s)
		}
	}

	// A zero Board has nil maps, so adding to it panics.
	var b Board
	if _, _, err := b.ApplyUpdate([]byte(frame("add", `{"events":[{"id":"1"}]}`)), at); err == nil {
		t.Fatal("panic not turned into an error")
	}
	if !reflect.DeepEqual(b, Board{}) {
		t.Fatal("board changed after a panic")
	}
}

func TestIdempotent(t *testing.T) {
	for league := range fixtureSubcategory {
		once, twice := loadBoard(t, league, "start"), loadBoard(t, league, "start")
		for _, f := range loadFrames(t, league) {
			mustApply(t, once, f)
			mustApply(t, twice, f)
			mustApply(t, twice, f)
		}
		if !reflect.DeepEqual(once, twice) {
			t.Errorf("%s: applying each frame twice differs from once", league)
		}
	}
}

func TestVenueRole(t *testing.T) {
	b := loadBoard(t, "nfl", "start")
	r, _ := b.Row("34118180") // participants list Home first
	if r.Away != "ATL Falcons" || r.Home != "GB Packers" {
		t.Fatalf("away %q home %q", r.Away, r.Home)
	}
	f := frame("change", `{"events":[{"id":"34118180","participants":[{"name":"GB Packers"},{"name":"ATL Falcons","venueRole":"Away"}]}]}`)
	changed := mustApply(t, b, []byte(f))
	if _, ok := b.Row("34118180"); ok || !slices.Equal(changed, []string{"34118180"}) || b.Skipped() != 1 {
		t.Fatalf("row kept %v, changed %v, skipped %d", ok, changed, b.Skipped())
	}
}

func TestOdds(t *testing.T) {
	b := loadBoard(t, "nfl", "start")
	p := toPrice(b.selections["0HC84695622N1150_3"]) // DK shows "−108"
	if p.American != "-108" || *p.Line != -11.5 || p.Decimal != 1.92592593 {
		t.Fatalf("%+v", p)
	}
	x := b.selections["0HC84695622N1150_3"]
	x.DisplayOdds = &displayOdds{American: "−999"}
	if p := toPrice(x); p.American != "-999" {
		t.Fatalf("american %q, want DK's display normalised", p.American)
	}
	x.DisplayOdds = nil
	if p := toPrice(x); p.American != "-108" {
		t.Fatalf("derived %q", p.American)
	}
}

func TestUnknownID(t *testing.T) {
	b := loadBoard(t, "nfl", "start")
	f := frame("change", `{"selections":[{"id":"nope","trueOdds":2}],"events":[{"id":"34118180","status":"STARTED"}]}`)
	changed, unknown, err := b.ApplyUpdate([]byte(f), at)
	if err != nil || !slices.Equal(unknown, []string{"nope"}) || !slices.Equal(changed, []string{"34118180"}) {
		t.Fatalf("changed %v, unknown %v, err %v", changed, unknown, err)
	}
	if r, _ := b.Row("34118180"); !r.Live {
		t.Fatal("the known part wasn't applied")
	}
}

// Stage 5 of README's latency table: decode and apply one frame, then encode its SSE patch.
func BenchmarkFrame(b *testing.B) {
	for league := range fixtureSubcategory {
		b.Run(league, func(b *testing.B) {
			frames := loadFrames(b, league)
			var applied int
			for b.Loop() {
				b.StopTimer()
				board := loadBoard(b, league, "start")
				b.StartTimer()
				for _, f := range frames {
					changed, _, err := board.ApplyUpdate(f, at)
					if err != nil {
						b.Fatal(err)
					}
					var rows []Row
					for _, id := range changed {
						if r, ok := board.Row(id); ok {
							rows = append(rows, r)
						}
					}
					sseFrame("patch", patchEvent{Rows: rows})
					applied++
				}
			}
			b.ReportMetric(float64(b.Elapsed().Microseconds())/float64(applied), "µs/frame")
			b.ReportMetric(float64(len(loadBoard(b, league, "start").Games())), "games")
		})
	}
}
