package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"math/rand/v2"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

const (
	site          = "dkcaon"
	leagueID      = "88808"
	subcategoryID = "4518"
	leaguesURL    = "https://sportsbook-nash.draftkings.com/api/sportscontent/" + site + "/v1/leagues/"
	snapshotURL   = leaguesURL + leagueID
	socketURL     = "wss://sportsbook-ws-ca-on.draftkings.com/websocket?format=json&locale=en"

	userAgent       = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/140.0.0.0 Safari/537.36"
	dkOrigin        = "https://sportsbook.draftkings.com"
	clientVersion   = "2636.2.1.11"
	netTimeout      = 5 * time.Second
	maxSnapshotSize = 8 << 20

	// snapshotLag is the longest delay between a frame reaching our socket and a snapshot request
	// showing it: step 2 measured 4.2 s, plus 2 s sampling and margin.
	snapshotLag     = 8 * time.Second
	lostFrameAfter  = 10 * time.Second
	syncFresh       = 15 * time.Second
	healthyWithin   = 25 * time.Second
	pingEvery       = 15 * time.Second
	resyncEvery     = 60 * time.Second
	snapshotSpacing = 2 * time.Second
	snapshotMaxWait = 30 * time.Second
	reconnectBase   = 500 * time.Millisecond
	reconnectMax    = 15 * time.Second
	healthyReset    = 60 * time.Second
	redialGrace     = time.Second
)

var dkClient = &http.Client{Timeout: netTimeout}

// subscriptionQuery is one of a snapshot's subscriptionPartials.
type subscriptionQuery struct {
	Entity         string `json:"entity"`
	Query          string `json:"query"`
	IncludeMarkets string `json:"includeMarkets"`
}

// nflSubscription is the snapshot's own partial, subscriptionPartials["league-events-88808"].
var nflSubscription = subscriptionQuery{
	Entity:         "events",
	Query:          "$filter=leagueId eq '" + leagueID + "' and clientMetadata/Subcategories/any(s: s/Id eq '" + subcategoryID + "')&$orderBy=startEventDate asc",
	IncludeMarkets: "$filter=tags/all(t: t ne 'SportcastBetBuilder') and clientMetadata/subCategoryId eq '" + subcategoryID + "'",
}

func fetchSnapshot(ctx context.Context, client *http.Client, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Accept-Language", "en-CA,en;q=0.9") // Akamai 403s without it
	req.Header.Set("Origin", dkOrigin)
	req.Header.Set("Referer", dkOrigin+"/")

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxSnapshotSize+1))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("snapshot: status %d", resp.StatusCode)
	}
	if len(body) > maxSnapshotSize {
		return nil, fmt.Errorf("snapshot: body over %d bytes", maxSnapshotSize)
	}
	return body, nil
}

type rpcRequest struct {
	JSONRPC string    `json:"jsonrpc"`
	Method  string    `json:"method"`
	ID      string    `json:"id"`
	Params  subscribe `json:"params"`
}

type subscribe struct {
	Entity           string            `json:"entity"`
	QueryParams      queryParams       `json:"queryParams"`
	ForwardedHeaders map[string]string `json:"forwardedHeaders"`
	ClientMetadata   map[string]string `json:"clientMetadata"`
	JWT              string            `json:"jwt"`
	SiteName         string            `json:"siteName"`
}

type queryParams struct {
	Query          string `json:"query"`
	IncludeMarkets string `json:"includeMarkets"`
	InitialData    bool   `json:"initialData"`
	Projection     string `json:"projection"`
	Locale         string `json:"locale"`
}

type socketMessage struct {
	ID                        string          `json:"id"`
	Event                     string          `json:"event"`
	Data                      json.RawMessage `json:"data"`
	WebsocketPublishTimestamp json.RawMessage `json:"websocketPublishTimestamp"`
}

const subscribeID = "nfl-1"

// dialAndSubscribe returns once DK has acked the subscription.
func dialAndSubscribe(ctx context.Context, client *http.Client, url string, q subscriptionQuery) (*websocket.Conn, socketMessage, error) {
	conn, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{
		HTTPClient: client,
		HTTPHeader: http.Header{"Origin": {dkOrigin}, "User-Agent": {userAgent}},
	})
	if err != nil {
		return nil, socketMessage{}, err
	}
	conn.SetReadLimit(maxSnapshotSize)

	sub := rpcRequest{JSONRPC: "2.0", Method: "subscribe", ID: subscribeID, Params: subscribe{
		Entity: q.Entity,
		QueryParams: queryParams{
			Query:          q.Query,
			IncludeMarkets: q.IncludeMarkets,
			Projection:     "sportsbook",
			Locale:         "en-US",
		},
		ForwardedHeaders: map[string]string{},
		ClientMetadata:   map[string]string{"feature": "league", "X-Client-Name": "web", "X-Client-Version": clientVersion},
		SiteName:         site,
	}}
	msg, err := json.Marshal(sub)
	if err == nil {
		err = conn.Write(ctx, websocket.MessageText, msg)
	}
	if err != nil {
		conn.CloseNow()
		return nil, socketMessage{}, err
	}

	for {
		_, raw, err := conn.Read(ctx)
		if err != nil {
			conn.CloseNow()
			return nil, socketMessage{}, err
		}
		var m socketMessage
		if json.Unmarshal(raw, &m) == nil && m.ID == subscribeID && m.Event == "subscribed" {
			return conn, m, nil
		}
	}
}

// Status is what the page shows about freshness; see PROJECT_PLAN.md "Freshness".
type Status struct {
	State      string    `json:"state"` // connecting, live, polling, stale
	StaleSince time.Time `json:"staleSince,omitzero"`
}

// Health is the /healthz body.
type Health struct {
	Status        Status    `json:"status"`
	Synced        bool      `json:"synced"`
	ResyncFrom    time.Time `json:"resyncFrom,omitzero"`
	LastSync      time.Time `json:"lastSync,omitzero"`
	SnapshotAt    time.Time `json:"snapshotAt,omitzero"`
	SnapshotErr   string    `json:"snapshotErr,omitempty"`
	Subscribed    bool      `json:"subscribed"`
	Subscriptions int       `json:"subscriptions"`
	SocketErr     string    `json:"socketErr,omitempty"`
	Frames        int       `json:"frames"`
	Rejected      int       `json:"rejected"`
	Skipped       int       `json:"skipped"`
	Misses        int       `json:"misses"`
	LagP50ms      float64   `json:"lagP50ms"`
	LagP95ms      float64   `json:"lagP95ms"`
	DelayP50ms    float64   `json:"delayP50ms"`
	DelayP95ms    float64   `json:"delayP95ms"`
	SocketRTTms   float64   `json:"socketRttMs"`
}

// Messages to the apply goroutine, in arrival order.
type (
	frameMsg struct {
		sub             int
		data            json.RawMessage
		recv, published time.Time
	}
	ackMsg struct {
		sub int
		at  time.Time
	}
	heardMsg struct {
		sub int
		at  time.Time
		rtt time.Duration // a ping's round trip; 0 for other messages
	}
	lostMsg struct {
		sub    int
		at     time.Time
		err    error
		redial bool // the subscription was healthy, so the socket redials at once
	}
	snapshotMsg struct {
		body  []byte
		err   error
		start time.Time
		done  chan<- error
	}
)

var errForcedReconnect = errors.New("forced reconnect")

// Feed keeps a Board in sync with DK. One apply goroutine (Run) owns everything below `in` and calls
// OnPatch and OnStatus; see PROJECT_PLAN.md "Ordering and consistency".
type Feed struct {
	snapshotURL, socketURL string
	client                 *http.Client
	OnPatch                func(rows []Row, removed []string)
	OnStatus               func(Status)

	in   chan any
	kick chan struct{} // a pending snapshot request
	back chan struct{} // the socket acked: the network is back, so drop the snapshot backoff
	kill chan int      // force-reconnect this subscription

	board      *Board
	ev         *evidence
	sub        int
	subscribed bool
	lastHeard  time.Time
	synced     bool
	resyncFrom time.Time
	lastSync   time.Time
	lastLive   time.Time
	firstMiss  map[string]time.Time
	drift      int
	status     Status
	h          Health
	socketRTT  time.Duration
	// Set for redialGrace after a live board's healthy socket closes. Until the redial acks, snapshots
	// wait: one sent now can't hold what the redial misses, and would push the post-ack one back by
	// snapshotSpacing. Status stays polling, not stale, until it fires.
	redialWait <-chan time.Time

	mu     sync.Mutex
	shared Health
	lags   durations
	delays durations
}

func NewFeed() *Feed { return newFeed(snapshotURL, socketURL, dkClient) }

func newFeed(snapshotURL, socketURL string, client *http.Client) *Feed {
	return &Feed{
		snapshotURL: snapshotURL,
		socketURL:   socketURL,
		client:      client,
		in:          make(chan any, 256),
		kick:        make(chan struct{}, 1),
		back:        make(chan struct{}, 1),
		kill:        make(chan int, 1),
		board:       NewBoard(subcategoryID),
		ev:          newEvidence(),
	}
}

func (f *Feed) Run(ctx context.Context) {
	var wg sync.WaitGroup
	defer wg.Wait()
	wg.Go(func() { f.socket(ctx) })
	wg.Go(func() { f.schedule(ctx) })
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	resync := time.NewTicker(resyncEvery)
	defer resync.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case m := <-f.in:
			f.handle(m)
		case <-tick.C:
		case <-f.redialWait:
			f.redialWait = nil
		case <-resync.C:
			f.request()
		}
		f.updateStatus(time.Now())
	}
}

func (f *Feed) handle(m any) {
	switch m := m.(type) {
	case frameMsg:
		f.onFrame(m)
	case ackMsg:
		f.onAck(m.sub, m.at)
	case heardMsg:
		if m.sub == f.sub {
			f.lastHeard = m.at
			if m.rtt > 0 {
				f.socketRTT = m.rtt
				f.h.SocketRTTms = ms(m.rtt)
			}
		}
	case lostMsg:
		f.onLost(m)
	case snapshotMsg:
		m.done <- f.onSnapshot(m.body, m.err, m.start, time.Now())
	}
}

func (f *Feed) onAck(sub int, at time.Time) {
	f.sub, f.subscribed, f.lastHeard = sub, true, at
	f.ev, f.firstMiss, f.drift = newEvidence(), nil, 0
	f.h.Subscriptions++
	f.h.SocketErr = ""
	select {
	case f.back <- struct{}{}:
	default:
	}
	f.needResync(at)
}

func (f *Feed) onLost(m lostMsg) {
	slog.Warn("socket", "sub", m.sub, "err", m.err)
	f.h.SocketErr = m.err.Error()
	f.redialWait = nil
	if m.redial && f.status.State == "live" {
		f.redialWait = time.After(redialGrace)
	}
	if m.sub == f.sub {
		f.subscribed, f.synced = false, false
	}
	f.request()
}

func (f *Feed) onFrame(m frameMsg) {
	if !f.subscribed || m.sub != f.sub {
		return
	}
	f.lastHeard = m.recv
	if !m.published.IsZero() {
		f.record(&f.lags, m.recv.Sub(m.published))
	}
	before := f.board.selections
	u, err := decodeUpdate(m.data)
	var changed, unknown []string
	if err == nil {
		changed, unknown, err = f.board.applyUpdate(u, m.recv)
	}
	if err != nil {
		slog.Warn("frame rejected", "err", err)
		f.h.Rejected++
		f.needResync(m.recv)
		return
	}
	f.h.Frames++
	f.ev.record(u, before, f.board, m.recv)
	if len(unknown) > 0 {
		slog.Warn("frame names unknown IDs", "ids", unknown)
		f.h.Skipped += len(unknown)
		f.needResync(m.recv)
	}
	f.publish(changed)
	// From DK creating the change to it leaving us, without comparing clocks: DK's own two
	// timestamps, half the socket round trip, and our processing. No sample until a ping has
	// measured the round trip.
	if d, ok := dkTime(u.Metadata, m.published); ok && f.socketRTT > 0 {
		f.record(&f.delays, d+f.socketRTT/2+time.Since(m.recv))
	}
}

// dkTime is how long DK took from creating a change to publishing it, both on DK's clock. Missing,
// malformed or out-of-order timestamps skip the measurement, never the odds.
func dkTime(metadata json.RawMessage, published time.Time) (time.Duration, bool) {
	var md struct {
		CreatedTime time.Time `json:"createdTime"`
	}
	if json.Unmarshal(metadata, &md) != nil || md.CreatedTime.IsZero() || published.IsZero() {
		return 0, false
	}
	d := published.Sub(md.CreatedTime)
	return d, d >= 0
}

func (f *Feed) needResync(from time.Time) {
	f.synced, f.resyncFrom = false, from
	f.request()
}

// onSnapshot commits a snapshot requested at start; see PROJECT_PLAN.md "Snapshot commit".
func (f *Feed) onSnapshot(body []byte, err error, start, now time.Time) error {
	f.h.SnapshotAt = now
	var next *Board
	if err == nil {
		next, err = f.board.parseSnapshot(body)
	}
	if err != nil {
		slog.Warn("snapshot", "err", err)
		f.h.SnapshotErr = err.Error()
		return err
	}
	f.h.SnapshotErr = ""

	cut := start.Add(-snapshotLag)
	cutOK := f.resyncFrom.IsZero() || !cut.Before(f.resyncFrom)
	var misses []string
	if f.subscribed {
		if !f.resyncFrom.IsZero() && cutOK {
			f.ev.dropBefore(f.resyncFrom)
		}
		misses = f.ev.check(next, cut)
		f.ev.overlay(next)
	}
	wasSynced := f.subscribed && f.synced
	changed := f.board.replace(next, now)
	f.lastSync = now
	f.synced = f.subscribed && cutOK && len(misses) == 0
	if f.synced {
		f.resyncFrom = time.Time{}
	}
	f.h.Misses += len(misses)
	if len(misses) > 0 {
		slog.Warn("snapshot disagrees with the socket", "misses", misses)
	}

	if f.lostFrame(misses, start) {
		f.reconnect("a field still missing after 10 s")
	}
	if wasSynced && len(changed) > 0 {
		f.drift++
	} else {
		f.drift = 0
	}
	if f.drift >= 2 {
		f.reconnect("two resyncs in a row changed the board")
	}
	f.publish(changed)
	return nil
}

// lostFrame tracks each miss from the first snapshot that showed it, and reports one still missing in
// a snapshot requested lostFrameAfter later: a late frame doesn't stay late that long.
func (f *Feed) lostFrame(misses []string, start time.Time) bool {
	seen := make(map[string]time.Time, len(misses))
	lost := false
	for _, k := range misses {
		first, ok := f.firstMiss[k]
		if !ok {
			first = start
		}
		seen[k] = first
		lost = lost || start.Sub(first) >= lostFrameAfter
	}
	f.firstMiss = seen
	return lost
}

func (f *Feed) reconnect(why string) {
	slog.Warn("forcing a socket reconnect", "sub", f.sub, "why", why)
	f.drift = 0
	select {
	case f.kill <- f.sub:
	default:
	}
}

// request asks the scheduler for a snapshot. It never blocks, and requests coalesce.
func (f *Feed) request() {
	if f.redialWait != nil && !f.subscribed {
		return
	}
	select {
	case f.kick <- struct{}{}:
	default:
	}
}

func (f *Feed) publish(changed []string) {
	if len(changed) == 0 || f.OnPatch == nil {
		return
	}
	var rows []Row
	var removed []string
	for _, id := range changed {
		if r, ok := f.board.Row(id); ok {
			rows = append(rows, r)
		} else {
			removed = append(removed, id)
		}
	}
	f.OnPatch(rows, removed)
}

func (f *Feed) currentStatus(now time.Time) Status {
	healthy := f.subscribed && now.Sub(f.lastHeard) <= healthyWithin
	switch {
	case f.lastSync.IsZero():
		return Status{State: "connecting"}
	case healthy && f.synced:
		f.lastLive = now
		return Status{State: "live"}
	case now.Sub(f.lastSync) <= syncFresh || f.redialWait != nil:
		return Status{State: "polling"}
	}
	return Status{State: "stale", StaleSince: laterOf(f.lastLive, f.lastSync)}
}

// updateStatus publishes status changes and polls while not live.
func (f *Feed) updateStatus(now time.Time) {
	st := f.currentStatus(now)
	if st.State != "live" {
		f.request()
	}
	if st != f.status {
		slog.Info("status", "state", st.State, "staleSince", st.StaleSince)
		f.status = st
		if f.OnStatus != nil {
			f.OnStatus(st)
		}
	}
	f.h.Status, f.h.Synced, f.h.ResyncFrom, f.h.LastSync, f.h.Subscribed = st, f.synced, f.resyncFrom, f.lastSync, f.subscribed
	f.mu.Lock()
	f.shared = f.h
	f.mu.Unlock()
}

func laterOf(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

// durations keeps the last 1,024 samples.
type durations struct {
	d [1024]time.Duration
	n int
}

func (r *durations) sorted() []time.Duration {
	s := slices.Clone(r.d[:min(r.n, len(r.d))])
	slices.Sort(s)
	return s
}

func percentiles(s []time.Duration) (p50, p95 float64) {
	if len(s) == 0 {
		return 0, 0
	}
	return ms(s[(len(s)-1)*50/100]), ms(s[(len(s)-1)*95/100])
}

func ms(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }

func (f *Feed) record(r *durations, d time.Duration) {
	f.mu.Lock()
	r.d[r.n%len(r.d)] = d
	r.n++
	f.mu.Unlock()
}

// Health is safe to call from any goroutine.
func (f *Feed) Health() Health {
	f.mu.Lock()
	h := f.shared
	lags, delays := f.lags.sorted(), f.delays.sorted()
	f.mu.Unlock()
	h.LagP50ms, h.LagP95ms = percentiles(lags)
	h.DelayP50ms, h.DelayP95ms = percentiles(delays)
	return h
}

// schedule runs every snapshot request: one in flight, starts at least snapshotSpacing apart, and
// full-jitter backoff after failures until the next success or subscribe ack.
func (f *Feed) schedule(ctx context.Context) {
	var last time.Time
	wait, failures := snapshotSpacing, 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-f.kick:
		}
		for d := time.Until(last.Add(wait)); d > 0; d = time.Until(last.Add(wait)) {
			t := time.NewTimer(d)
			select {
			case <-ctx.Done():
				t.Stop()
				return
			case <-f.back:
				wait, failures = snapshotSpacing, 0
			case <-t.C:
			}
			t.Stop()
		}
		last = time.Now()
		body, err := fetchSnapshot(ctx, f.client, f.snapshotURL)
		done := make(chan error, 1)
		if !f.send(ctx, snapshotMsg{body, err, last, done}) {
			return
		}
		select {
		case <-ctx.Done():
			return
		case err = <-done:
		}
		if err == nil {
			wait, failures = snapshotSpacing, 0
		} else {
			failures++
			wait = max(snapshotSpacing, backoff(snapshotSpacing, snapshotMaxWait, failures))
		}
	}
}

// backoff is full jitter: random up to base·2ⁿ, capped.
func backoff(base, limit time.Duration, n int) time.Duration {
	return rand.N(min(limit, base<<min(n, 16)))
}

func (f *Feed) socket(ctx context.Context) {
	failures := 0
	for sub := 1; ; sub++ {
		dialCtx, cancel := context.WithTimeout(ctx, netTimeout)
		conn, _, err := dialAndSubscribe(dialCtx, f.client, f.socketURL, nflSubscription)
		cancel()
		healthy := false
		if err == nil {
			acked := time.Now()
			f.send(ctx, ackMsg{sub, acked})
			err = f.read(ctx, conn, sub)
			healthy = time.Since(acked) >= healthyReset
		}
		if ctx.Err() != nil {
			return
		}
		f.send(ctx, lostMsg{sub, time.Now(), err, healthy})
		if healthy {
			failures = 0 // DK closes every socket after 30 min: redial at once
			continue
		}
		if !sleepCtx(ctx, backoff(reconnectBase, reconnectMax, failures)) {
			return
		}
		failures++
	}
}

// read forwards one subscription's messages until the connection fails, a ping goes unanswered, or
// the apply goroutine forces a reconnect.
func (f *Feed) read(ctx context.Context, conn *websocket.Conn, sub int) error {
	ctx, cancel := context.WithCancelCause(ctx)
	var wg sync.WaitGroup
	defer func() {
		cancel(nil)
		conn.CloseNow()
		wg.Wait()
	}()
	wg.Go(func() { f.ping(ctx, cancel, conn, sub) })
	for {
		_, raw, err := conn.Read(ctx)
		recv := time.Now()
		if err != nil {
			if cause := context.Cause(ctx); cause != nil {
				return cause
			}
			return err
		}
		var m socketMessage
		if json.Unmarshal(raw, &m) == nil && m.Event != "update" {
			f.send(ctx, heardMsg{sub: sub, at: recv})
			continue
		}
		var published time.Time
		json.Unmarshal(m.WebsocketPublishTimestamp, &published)
		f.send(ctx, frameMsg{sub, m.Data, recv, published}) // an unparseable message has no data and is rejected
	}
}

func (f *Feed) ping(ctx context.Context, cancel context.CancelCauseFunc, conn *websocket.Conn, sub int) {
	t := time.NewTicker(pingEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case k := <-f.kill:
			if k == sub {
				cancel(errForcedReconnect)
				return
			}
		case <-t.C:
			pingCtx, done := context.WithTimeout(ctx, netTimeout)
			start := time.Now()
			err := conn.Ping(pingCtx)
			done()
			if err != nil {
				cancel(fmt.Errorf("ping: %w", err))
				return
			}
			f.send(ctx, heardMsg{sub, time.Now(), time.Since(start)})
		}
	}
}

func (f *Feed) send(ctx context.Context, m any) bool {
	select {
	case f.in <- m:
		return true
	case <-ctx.Done():
		return false
	}
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// evidence is what the current subscription's frames set, per entity.
type evidence struct {
	events     entries[event]
	markets    entries[market]
	selections entries[selection]
}

// entry holds an entity's value after the frame that last touched it, and the receive time of the
// frame that last set each field. A tombstone has only `gone`.
type entry[T any] struct {
	v    T
	at   map[string]time.Time // JSON field name → receive time
	gone time.Time
}

type entries[T canonical[T]] map[string]*entry[T]

// canonical values normalise what DK spells differently in frames and snapshots, and give the
// skeleton an entity is rebuilt from when a snapshot lacks it: the fields that never change under its
// ID. Every other field needs surviving evidence.
type canonical[T any] interface {
	canon() T
	skeleton() T
}

func (e event) canon() event {
	e.StartEventDate = e.StartEventDate.UTC()
	return e
}

func (m market) canon() market {
	main, suspended := m.Main == nil || *m.Main, isSuspended(m)
	m.Main, m.IsSuspended = &main, &suspended
	return m
}

func (e event) skeleton() event {
	return event{ID: e.ID, Participants: e.Participants}
}

// A market whose suspension has no evidence shows as suspended.
func (m market) skeleton() market {
	suspended := true
	return market{ID: m.ID, EventID: m.EventID, SubcategoryID: m.SubcategoryID, Main: m.Main, MarketType: m.MarketType, IsSuspended: &suspended}
}

func (x selection) skeleton() selection {
	return selection{ID: x.ID, MarketID: x.MarketID, OutcomeType: x.OutcomeType}
}

func (x selection) canon() selection {
	main := slices.Contains(x.Tags, "MainPointLine")
	x.Tags = nil
	if main {
		x.Tags = []string{"MainPointLine"}
	}
	return x
}

func newEvidence() *evidence {
	return &evidence{entries[event]{}, entries[market]{}, entries[selection]{}}
}

// record notes what an applied frame set, reading values back from the board it produced. before is
// the board's selections before the frame, to tell a line move from a resend.
func (ev *evidence) record(u *updateData, before map[string]selection, b *Board, at time.Time) {
	d := u.Data
	for _, e := range d.Add.Events {
		ev.events.add(e.ID, b.events, at)
	}
	for _, m := range d.Add.Markets {
		ev.markets.add(m.ID, b.markets, at)
	}
	for _, a := range d.Add.Selections {
		old, id := a.v.ReplacedSelectionID, a.v.ID
		ev.move(old, id, before, at)
		if old != "" && (hasKey(before, old) || hasKey(before, id)) { // Board.apply merged it
			ev.selections.change(id, b.selections, a.keys, at)
		} else {
			ev.selections.add(id, b.selections, at)
		}
	}
	for _, c := range d.Change.Events {
		ev.events.change(c.v.ID, b.events, c.keys, at)
	}
	for _, c := range d.Change.Markets {
		ev.markets.change(c.v.ID, b.markets, c.keys, at)
	}
	for _, c := range d.Change.Selections {
		ev.move(c.v.ReplacedSelectionID, c.v.ID, before, at)
		ev.selections.change(c.v.ID, b.selections, c.keys, at)
	}
	for _, id := range d.Remove.Events {
		ev.events[id] = &entry[event]{gone: at}
	}
	for _, id := range d.Remove.Markets {
		ev.markets[id] = &entry[market]{gone: at}
	}
	for _, id := range d.Remove.Selections {
		ev.selections[id] = &entry[selection]{gone: at}
	}
}

// move gives a line move's new ID the old ID's evidence and tombstones the old ID.
func (ev *evidence) move(old, id string, before map[string]selection, at time.Time) {
	if old == "" || !hasKey(before, old) {
		return
	}
	if e := ev.selections[old]; e != nil && e.gone.IsZero() && ev.selections[id] == nil {
		ev.selections[id] = &entry[selection]{v: e.v, at: maps.Clone(e.at)}
	}
	ev.selections[old] = &entry[selection]{gone: at}
}

func (es entries[T]) add(id string, board map[string]T, at time.Time) {
	v, ok := board[id]
	if !ok {
		return
	}
	e := &entry[T]{v: v, at: map[string]time.Time{}}
	for _, name := range jsonFields(reflect.TypeFor[T]()) {
		e.at[name] = at
	}
	es[id] = e
}

// change skips an ID the board doesn't hold: the board skipped that part too.
func (es entries[T]) change(id string, board map[string]T, keys map[string]json.RawMessage, at time.Time) {
	v, ok := board[id]
	if !ok {
		return
	}
	e := es[id]
	if e == nil || !e.gone.IsZero() {
		e = &entry[T]{at: map[string]time.Time{}}
		es[id] = e
	}
	e.v = v
	for _, name := range jsonFields(reflect.TypeFor[T]()) {
		if hasKey(keys, name) {
			e.at[name] = at
		}
	}
}

func (ev *evidence) dropBefore(t time.Time) {
	ev.events.dropBefore(t)
	ev.markets.dropBefore(t)
	ev.selections.dropBefore(t)
}

func (es entries[T]) dropBefore(t time.Time) {
	for id, e := range es {
		if !e.gone.IsZero() {
			if e.gone.Before(t) {
				delete(es, id)
			}
			continue
		}
		maps.DeleteFunc(e.at, func(_ string, at time.Time) bool { return at.Before(t) })
		if len(e.at) == 0 {
			delete(es, id)
		}
	}
}

// check lists the fields and tombstones received before cut that the snapshot doesn't match.
func (ev *evidence) check(next *Board, cut time.Time) []string {
	return slices.Concat(
		ev.events.check("event", next.events, cut),
		ev.markets.check("market", next.markets, cut),
		ev.selections.check("selection", next.selections, cut),
	)
}

func (es entries[T]) check(kind string, snap map[string]T, cut time.Time) (misses []string) {
	for id, e := range es {
		s, held := snap[id]
		if !e.gone.IsZero() {
			if held && e.gone.Before(cut) {
				misses = append(misses, kind+" "+id+" removed")
			}
			continue
		}
		for name, at := range e.at {
			if at.Before(cut) && (!held || !sameField(s, e.v, name)) {
				misses = append(misses, kind+" "+id+" "+name)
			}
		}
	}
	return misses
}

func sameField[T canonical[T]](a, b T, name string) bool {
	va, vb := reflect.ValueOf(a.canon()), reflect.ValueOf(b.canon())
	for i := range va.NumField() {
		if jsonName(va.Type().Field(i)) == name {
			return reflect.DeepEqual(va.Field(i).Interface(), vb.Field(i).Interface())
		}
	}
	return false
}

// overlay puts every remaining field and tombstone on a snapshot's board.
func (ev *evidence) overlay(next *Board) {
	ev.events.overlay(next.events)
	ev.markets.overlay(next.markets)
	ev.selections.overlay(next.selections)
}

func (es entries[T]) overlay(snap map[string]T) {
	for id, e := range es {
		s, held := snap[id]
		switch {
		case !e.gone.IsZero():
			delete(snap, id)
		case held:
			merge(&s, e.v, e.at)
			snap[id] = s
		default:
			v := e.v.skeleton()
			merge(&v, e.v, e.at)
			snap[id] = v
		}
	}
}

func jsonFields(t reflect.Type) []string {
	var names []string
	for i := range t.NumField() {
		if name := jsonName(t.Field(i)); name != "id" {
			names = append(names, name)
		}
	}
	return names
}

func jsonName(f reflect.StructField) string {
	name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
	return name
}
