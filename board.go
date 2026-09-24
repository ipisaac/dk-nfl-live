package main

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"
)

// DK wire types keep only the fields we use. Fields DK may omit or null (the snapshot omits a false
// isSuspended) are pointers.
type event struct {
	ID             string        `json:"id"`
	StartEventDate time.Time     `json:"startEventDate"`
	Status         string        `json:"status"`
	Participants   []participant `json:"participants"`
}

type participant struct {
	Name      string `json:"name"`
	VenueRole string `json:"venueRole"`
}

type market struct {
	ID            string      `json:"id"`
	EventID       string      `json:"eventId"`
	SubcategoryID json.Number `json:"subcategoryId"` // a number in snapshots, a string in frames
	Main          *bool       `json:"main"`
	MarketType    *marketType `json:"marketType"`
	IsSuspended   *bool       `json:"isSuspended"`
}

type marketType struct {
	Name string `json:"name"`
}

type selection struct {
	ID          string       `json:"id"`
	MarketID    string       `json:"marketId"`
	OutcomeType string       `json:"outcomeType"`
	Points      *float64     `json:"points"`
	TrueOdds    *float64     `json:"trueOdds"`
	DisplayOdds *displayOdds `json:"displayOdds"`
	Tags        []string     `json:"tags"`
}

type displayOdds struct {
	American string `json:"american"`
}

type selectionUpdate struct {
	selection
	ReplacedSelectionID string `json:"replacedSelectionId"`
}

// sent is an entity from a frame plus the top-level keys the frame sent, so a merge copies exactly
// those, explicit nulls included.
type sent[T any] struct {
	v    T
	keys map[string]json.RawMessage
}

func (s *sent[T]) UnmarshalJSON(b []byte) error {
	if err := json.Unmarshal(b, &s.keys); err != nil {
		return err
	}
	return json.Unmarshal(b, &s.v)
}

// updateData is a socket message's `data`. Every update DK sent has all three parts.
type updateData struct {
	Data *struct {
		Add *struct {
			Events     []event                 `json:"events"`
			Markets    []market                `json:"markets"`
			Selections []sent[selectionUpdate] `json:"selections"`
		} `json:"add"`
		Change *struct {
			Events     []sent[event]           `json:"events"`
			Markets    []sent[market]          `json:"markets"`
			Selections []sent[selectionUpdate] `json:"selections"`
		} `json:"change"`
		Remove *struct {
			Events     []string `json:"events"`
			Markets    []string `json:"markets"`
			Selections []string `json:"selections"`
		} `json:"remove"`
	} `json:"data"`
	Metadata json.RawMessage `json:"metadata"` // timing only; see dkTime
}

var errEnvelope = errors.New("no data.add, data.change and data.remove")

type Price struct {
	Line     *float64 `json:"line,omitempty"`
	American string   `json:"american"`
	Decimal  float64  `json:"decimal,omitempty"`
}

type Sides struct {
	Away *Price `json:"away"`
	Home *Price `json:"home"`
}

type OverUnder struct {
	Over  *Price `json:"over"`
	Under *Price `json:"under"`
}

type Suspended struct {
	Spread    bool `json:"spread"`
	Total     bool `json:"total"`
	Moneyline bool `json:"moneyline"`
}

type Row struct {
	ID        string    `json:"id"`
	Away      string    `json:"away"`
	Home      string    `json:"home"`
	Start     time.Time `json:"start,omitzero"` // zero if a rebuilt event lost its start time
	Live      bool      `json:"live"`
	Spread    Sides     `json:"spread"`
	Total     OverUnder `json:"total"`
	Moneyline Sides     `json:"moneyline"`
	Suspended Suspended `json:"suspended"`
	UpdatedAt time.Time `json:"updatedAt,omitzero"` // when a price last moved; zero if not seen to move
}

// Board holds DK's entities flat by ID and the rows built from them. Every Apply builds a new state
// and swaps it in only on success, so a rejected snapshot or frame leaves the board untouched.
type Board struct {
	subcategory string
	events      map[string]event
	markets     map[string]market
	selections  map[string]selection
	rows        map[string]Row
	skipped     int
}

func NewBoard(subcategory string) *Board {
	return &Board{
		subcategory: subcategory,
		events:      map[string]event{},
		markets:     map[string]market{},
		selections:  map[string]selection{},
		rows:        map[string]Row{},
	}
}

var errMissingID = errors.New("entity without id")

// ApplySnapshot replaces the board with a league snapshot and returns the IDs of games whose rows
// changed or went away.
func (b *Board) ApplySnapshot(body []byte, at time.Time) ([]string, error) {
	next, err := b.parseSnapshot(body)
	if err != nil {
		return nil, err
	}
	return b.replace(next, at), nil
}

// parseSnapshot builds and validates a board from a league snapshot, leaving b untouched.
func (b *Board) parseSnapshot(body []byte) (next *Board, err error) {
	defer recoverTo(&err)
	var s struct {
		Events     []event     `json:"events"`
		Markets    []market    `json:"markets"`
		Selections []selection `json:"selections"`
	}
	if err := json.Unmarshal(body, &s); err != nil {
		return nil, fmt.Errorf("snapshot: %w", err)
	}
	next = NewBoard(b.subcategory)
	for _, e := range s.Events {
		next.events[e.ID] = e
	}
	for _, m := range s.Markets {
		next.markets[m.ID] = m
	}
	for _, x := range s.Selections {
		next.selections[x.ID] = x
	}
	if hasKey(next.events, "") || hasKey(next.markets, "") || hasKey(next.selections, "") {
		return nil, fmt.Errorf("snapshot: %w", errMissingID)
	}
	if next.rebuild(nil, time.Time{}); len(next.rows) == 0 {
		return nil, errors.New("snapshot: no games")
	}
	return next, nil
}

// replace swaps in next and returns the IDs of games whose rows changed or went away.
func (b *Board) replace(next *Board, at time.Time) []string {
	changed := next.rebuild(b.rows, at)
	*b = *next
	return changed
}

// ApplyUpdate applies one socket message's `data`. It returns the IDs of games whose rows changed or
// went away, and the IDs of `change` parts it skipped because it doesn't hold them; the board then
// needs a resync.
func (b *Board) ApplyUpdate(data []byte, at time.Time) (changed, unknown []string, err error) {
	u, err := decodeUpdate(data)
	if err != nil {
		return nil, nil, err
	}
	return b.applyUpdate(u, at)
}

func decodeUpdate(data []byte) (*updateData, error) {
	var u updateData
	if err := json.Unmarshal(data, &u); err != nil {
		return nil, fmt.Errorf("update: %w", err)
	}
	if d := u.Data; d == nil || d.Add == nil || d.Change == nil || d.Remove == nil {
		return nil, fmt.Errorf("update: %w", errEnvelope)
	}
	return &u, nil
}

func (b *Board) applyUpdate(u *updateData, at time.Time) (changed, unknown []string, err error) {
	defer recoverTo(&err)
	next := &Board{
		subcategory: b.subcategory,
		events:      maps.Clone(b.events),
		markets:     maps.Clone(b.markets),
		selections:  maps.Clone(b.selections),
	}
	if unknown, err = next.apply(u); err != nil {
		return nil, nil, fmt.Errorf("update: %w", err)
	}
	changed = next.rebuild(b.rows, at)
	*b = *next
	return changed, unknown, nil
}

func recoverTo(err *error) {
	if r := recover(); r != nil {
		*err = fmt.Errorf("panic: %v", r)
	}
}

func hasKey[V any](m map[string]V, k string) bool {
	_, ok := m[k]
	return ok
}

func (b *Board) apply(u *updateData) (unknown []string, err error) {
	d := u.Data
	for _, e := range d.Add.Events {
		if e.ID == "" {
			return nil, errMissingID
		}
		b.events[e.ID] = e
	}
	for _, m := range d.Add.Markets {
		if m.ID == "" {
			return nil, errMissingID
		}
		b.markets[m.ID] = m
	}
	for _, a := range d.Add.Selections {
		x := a.v
		if x.ID == "" {
			return nil, errMissingID
		}
		b.moveReplaced(x)
		if cur, ok := b.selections[x.ID]; ok && x.ReplacedSelectionID != "" {
			merge(&cur, x.selection, a.keys)
			b.selections[x.ID] = cur
		} else {
			b.selections[x.ID] = x.selection
		}
	}

	for _, c := range d.Change.Events {
		if !mergeInto(b.events, c.v.ID, c.v, c.keys, &unknown) {
			return nil, errMissingID
		}
	}
	for _, c := range d.Change.Markets {
		if !mergeInto(b.markets, c.v.ID, c.v, c.keys, &unknown) {
			return nil, errMissingID
		}
	}
	for _, c := range d.Change.Selections {
		b.moveReplaced(c.v)
		if !mergeInto(b.selections, c.v.ID, c.v.selection, c.keys, &unknown) {
			return nil, errMissingID
		}
	}

	for _, id := range d.Remove.Events {
		delete(b.events, id)
	}
	for _, id := range d.Remove.Markets {
		delete(b.markets, id)
	}
	for _, id := range d.Remove.Selections {
		delete(b.selections, id)
	}
	return unknown, nil
}

// moveReplaced gives a line move's new ID the old selection, so it inherits the fields the frame
// omits. A resend whose old ID is gone, or whose new ID is already held, only drops the old ID.
func (b *Board) moveReplaced(x selectionUpdate) {
	old, ok := b.selections[x.ReplacedSelectionID]
	if x.ReplacedSelectionID == "" || !ok {
		return
	}
	delete(b.selections, x.ReplacedSelectionID)
	if !hasKey(b.selections, x.ID) {
		b.selections[x.ID] = old
	}
}

// mergeInto merges a change into the entity it names, or records the ID as unknown. It reports false
// for a change without an ID.
func mergeInto[T any](m map[string]T, id string, c T, keys map[string]json.RawMessage, unknown *[]string) bool {
	if id == "" {
		return false
	}
	e, ok := m[id]
	if !ok {
		*unknown = append(*unknown, id)
		return true
	}
	merge(&e, c, keys)
	m[id] = e
	return true
}

// merge copies the fields whose JSON keys are in keys over e: DK's `change` is a top-level merge of
// absolute values.
func merge[T, V any](e *T, c T, keys map[string]V) {
	dst, src := reflect.ValueOf(e).Elem(), reflect.ValueOf(c)
	for i := range src.NumField() {
		if hasKey(keys, jsonName(src.Type().Field(i))) {
			dst.Field(i).Set(src.Field(i))
		}
	}
}

// rebuild recomputes every row and returns the IDs that differ from prev.
// ponytail: full rebuild per frame, ~100 markets; index by event if a league grows past thousands.
func (b *Board) rebuild(prev map[string]Row, at time.Time) (changed []string) {
	kinds := map[string]map[string]market{} // event ID → kind → market
	for _, m := range b.markets {
		kind := marketKinds[marketTypeName(m)]
		if kind == "" || m.SubcategoryID.String() != b.subcategory || (m.Main != nil && !*m.Main) {
			continue
		}
		if kinds[m.EventID] == nil {
			kinds[m.EventID] = map[string]market{}
		}
		kinds[m.EventID][kind] = m
	}
	candidates := map[[2]string][]selection{} // {market ID, outcome type}
	for _, x := range b.selections {
		k := [2]string{x.MarketID, x.OutcomeType}
		candidates[k] = append(candidates[k], x)
	}
	price := func(m market, outcome string) *Price {
		x, ok := choose(candidates[[2]string{m.ID, outcome}])
		if !ok {
			return nil
		}
		return toPrice(x)
	}

	b.rows = make(map[string]Row, len(b.events))
	b.skipped = 0
	for id, e := range b.events {
		r := Row{ID: id, Start: e.StartEventDate, Live: e.Status == "STARTED"}
		for _, p := range e.Participants {
			switch p.VenueRole {
			case "Home":
				r.Home = p.Name
			case "Away":
				r.Away = p.Name
			}
		}
		if r.Home == "" || r.Away == "" {
			b.skipped++
			continue
		}
		if m, ok := kinds[id]["spread"]; ok {
			r.Spread = Sides{Away: price(m, "Away"), Home: price(m, "Home")}
			r.Suspended.Spread = isSuspended(m)
		}
		if m, ok := kinds[id]["total"]; ok {
			r.Total = OverUnder{Over: price(m, "Over"), Under: price(m, "Under")}
			r.Suspended.Total = isSuspended(m)
		}
		if m, ok := kinds[id]["moneyline"]; ok {
			r.Moneyline = Sides{Away: price(m, "Away"), Home: price(m, "Home")}
			r.Suspended.Moneyline = isSuspended(m)
		}
		old, had := prev[id]
		r.UpdatedAt = old.UpdatedAt
		if had && !samePrices(old, r) {
			r.UpdatedAt = at
		}
		if !had || !reflect.DeepEqual(old, r) {
			changed = append(changed, id)
		}
		b.rows[id] = r
	}
	for id := range prev {
		if !hasKey(b.rows, id) {
			changed = append(changed, id)
		}
	}
	slices.Sort(changed)
	return changed
}

var marketKinds = map[string]string{
	"Moneyline": "moneyline",
	"Spread":    "spread",
	"Run Line":  "spread", // baseball, for the MLB/NPB fixtures
	"Total":     "total",
}

func marketTypeName(m market) string {
	if m.MarketType == nil {
		return ""
	}
	return m.MarketType.Name
}

func isSuspended(m market) bool { return m.IsSuspended != nil && *m.IsSuspended }

func samePrices(a, b Row) bool {
	return reflect.DeepEqual(a.Spread, b.Spread) && reflect.DeepEqual(a.Total, b.Total) && reflect.DeepEqual(a.Moneyline, b.Moneyline)
}

// choose takes the only selection present, else the only one tagged MainPointLine.
func choose(xs []selection) (selection, bool) {
	if len(xs) == 1 {
		return xs[0], true
	}
	var main []selection
	for _, x := range xs {
		if slices.Contains(x.Tags, "MainPointLine") {
			main = append(main, x)
		}
	}
	if len(main) == 1 {
		return main[0], true
	}
	return selection{}, false
}

func toPrice(x selection) *Price {
	p := &Price{Line: x.Points}
	if x.TrueOdds != nil && *x.TrueOdds > 1 {
		p.Decimal = *x.TrueOdds
	}
	if x.DisplayOdds != nil {
		p.American = strings.ReplaceAll(x.DisplayOdds.American, "−", "-")
	}
	if !validAmerican(p.American) {
		if p.Decimal == 0 {
			return nil
		}
		p.American = americanFromDecimal(p.Decimal)
	}
	return p
}

func validAmerican(s string) bool {
	if len(s) < 2 || (s[0] != '+' && s[0] != '-') {
		return false
	}
	n, err := strconv.Atoi(s[1:])
	return err == nil && n >= 100
}

func americanFromDecimal(d float64) string {
	if d >= 2 {
		return fmt.Sprintf("+%d", int(math.Round((d-1)*100)))
	}
	return fmt.Sprintf("-%d", int(math.Round(100/(d-1))))
}

// Games returns the rows by start time.
func (b *Board) Games() []Row {
	rows := slices.Collect(maps.Values(b.rows))
	slices.SortFunc(rows, byStart)
	return rows
}

func byStart(x, y Row) int {
	return cmp.Or(x.Start.Compare(y.Start), strings.Compare(x.ID, y.ID))
}

// Row reports a game's row; false means the game is gone.
func (b *Board) Row(id string) (Row, bool) {
	r, ok := b.rows[id]
	return r, ok
}

// Skipped is how many events have no row because a participant lacks a home/away venueRole.
func (b *Board) Skipped() int { return b.skipped }
