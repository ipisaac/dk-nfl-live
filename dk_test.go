package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/coder/websocket"
)

func TestMain(m *testing.M) {
	flag.Parse()
	if !testing.Verbose() {
		slog.SetDefault(slog.New(slog.DiscardHandler))
	}
	os.Exit(m.Run())
}

// sel is an NFL spread selection; the fixture shows it at −108.
const sel = "0HC84695622N1150_3"

var t0 = time.Date(2026, 9, 27, 17, 0, 0, 0, time.UTC)

func sec(s float64) time.Time { return t0.Add(time.Duration(s * float64(time.Second))) }

func decimal(american string) float64 {
	n, _ := strconv.ParseFloat(american, 64)
	if n > 0 {
		return 1 + n/100
	}
	return 1 - 100/n
}

// snapWith is the NFL start fixture with sel priced at american.
func snapWith(t *testing.T, american string) []byte {
	t.Helper()
	return snapAs(t, american, sel)
}

// snapAs is snapWith with sel renamed to id, or left out if id is "".
func snapAs(t *testing.T, american, id string) []byte {
	t.Helper()
	body, err := os.ReadFile("testdata/nfl-start.json")
	if err != nil {
		t.Fatal(err)
	}
	var s map[string]any
	if err := json.Unmarshal(body, &s); err != nil {
		t.Fatal(err)
	}
	sels := s["selections"].([]any)
	for i, x := range sels {
		if x := x.(map[string]any); x["id"] == sel {
			x["id"], x["trueOdds"], x["displayOdds"] = id, decimal(american), map[string]any{"american": american}
			if id == "" {
				s["selections"] = slices.Delete(sels, i, i+1)
			}
			break
		}
	}
	out, _ := json.Marshal(s)
	return out
}

func priceFrame(american string) string {
	return frame("change", fmt.Sprintf(`{"selections":[{"id":%q,"trueOdds":%v,"displayOdds":{"american":%q}}]}`, sel, decimal(american), american))
}

const rejectedFrame = `{"data":{"change":{}}}`

func onFrame(f *Feed, at float64, data string) {
	f.onFrame(frameMsg{sub: f.sub, data: json.RawMessage(data), recv: sec(at)})
}

func commit(t *testing.T, f *Feed, american string, start float64) {
	t.Helper()
	if err := f.onSnapshot(snapWith(t, american), nil, sec(start), sec(start+0.3)); err != nil {
		t.Fatal(err)
	}
}

func shown(f *Feed) string {
	return shownAs(f, sel)
}

func shownAs(f *Feed, id string) string {
	x, ok := f.board.selections[id]
	if !ok {
		return "missing"
	}
	if p := toPrice(x); p != nil {
		return p.American
	}
	return "unpriced"
}

func want(t *testing.T, f *Feed, american string, synced bool) {
	t.Helper()
	if got := shown(f); got != american || f.synced != synced {
		t.Fatalf("shown %s synced %v, want %s %v", got, f.synced, american, synced)
	}
}

// syncedFeed is subscribed at t=0 and synced by a snapshot requested at t=8.
func syncedFeed(t *testing.T) *Feed {
	f := newFeed("", "", nil)
	f.onAck(1, sec(0))
	commit(t, f, "-108", 0)
	want(t, f, "-108", false)
	commit(t, f, "-108", 8)
	want(t, f, "-108", true)
	return f
}

func TestOverlay(t *testing.T) {
	f := newFeed("", "", nil)
	f.onAck(1, sec(0))
	commit(t, f, "-108", 0)
	onFrame(f, 1, priceFrame("+110"))
	commit(t, f, "-108", 2) // cut before the frame and the ack
	want(t, f, "+110", false)
	commit(t, f, "-108", 9) // cut at the frame
	want(t, f, "+110", true)
}

// The delay adds DK's internal time and half the socket round trip, never our clock against DK's.
func TestDelay(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := syncedFeed(t)
		frame := func(price, created string) {
			data := strings.Replace(priceFrame(price), `{"data":`, `{"metadata":{"createdTime":`+created+`},"data":`, 1)
			f.onFrame(frameMsg{sub: 1, data: json.RawMessage(data), recv: time.Now(), published: sec(9.05)})
		}
		frame("+110", `"2026-09-27T17:00:09Z"`)
		if got := f.Health().DelayP50ms; got != 0 {
			t.Fatalf("delay %v ms before any ping", got)
		}
		f.handle(heardMsg{sub: 1, at: sec(9), rtt: 20 * time.Millisecond})
		frame("+115", `"not a time"`)
		want(t, f, "+115", true)
		frame("+120", `"2026-09-27T17:00:10Z"`) // after publishing
		if got := f.Health().DelayP50ms; got != 0 {
			t.Fatalf("delay %v ms from bad timestamps", got)
		}
		frame("+125", `"2026-09-27T17:00:09Z"`)
		if got := f.Health().DelayP50ms; got != 60 {
			t.Fatalf("delay %v ms, want 50 (DK) + 10 (socket) + 0 (fake clock)", got)
		}
	})
}

func TestStaleSnapshot(t *testing.T) {
	f := syncedFeed(t)
	onFrame(f, 10, priceFrame("+110"))
	commit(t, f, "-108", 19) // cut after the frame, still without it
	want(t, f, "+110", false)
	if f.h.Misses != 2 { // trueOdds and displayOdds
		t.Fatalf("misses %d", f.h.Misses)
	}
	commit(t, f, "-108", 25)
	if len(f.kill) != 0 {
		t.Fatal("reconnect 6 s after the first miss")
	}
	commit(t, f, "-108", 29)
	if len(f.kill) != 1 || <-f.kill != 1 {
		t.Fatal("no reconnect 10 s after the first miss")
	}

	f.onLost(1, errForcedReconnect)
	f.onAck(2, sec(30))
	commit(t, f, "-108", 30.5)
	want(t, f, "-108", false)
	commit(t, f, "-108", 38.5)
	want(t, f, "-108", true)
}

func TestRejectedLaterFrame(t *testing.T) {
	for _, failed := range []bool{false, true} {
		t.Run(fmt.Sprint("second snapshot fails: ", failed), func(t *testing.T) {
			f := syncedFeed(t)
			onFrame(f, 10, priceFrame("+110"))
			onFrame(f, 11, rejectedFrame)
			commit(t, f, "+120", 11.5) // cut before the rejection: keep the frame's value
			want(t, f, "+110", false)
			if failed {
				if f.onSnapshot([]byte("garbage"), nil, sec(19), sec(19.3)) == nil {
					t.Fatal("garbage committed")
				}
				if e := f.ev.selections[sel]; e == nil || e.at["trueOdds"] != sec(10) {
					t.Fatalf("evidence after a failed commit: %+v", e)
				}
				return
			}
			commit(t, f, "+120", 19) // cut at the rejection
			want(t, f, "+120", true)
			if e := f.ev.selections[sel]; e != nil {
				t.Fatalf("superseded evidence kept: %+v", e)
			}
		})
	}
}

func TestPartialMerge(t *testing.T) {
	f := syncedFeed(t)
	onFrame(f, 10, priceFrame("+110"))
	onFrame(f, 11, rejectedFrame)
	onFrame(f, 12, frame("change", `{"selections":[{"id":"`+sel+`","points":-12.5}]}`))
	if e := f.ev.selections[sel]; e.at["trueOdds"] != sec(10) || e.at["points"] != sec(12) {
		t.Fatalf("field times %v", e.at)
	}
	commit(t, f, "+120", 19.5) // cut 11.5: drops the price, keeps the points
	want(t, f, "+120", true)
	if p := f.board.selections[sel].Points; *p != -12.5 || !slices.Equal(fieldNames(f.ev.selections[sel]), []string{"points"}) {
		t.Fatalf("points %v, evidence %v", *p, f.ev.selections[sel].at)
	}
}

// A replacement add merges like a change, so the price it inherits keeps its time and the rejection
// supersedes it.
func TestReplacementAdd(t *testing.T) {
	const moved = sel + "x"
	f := syncedFeed(t)
	onFrame(f, 10, priceFrame("+110"))
	onFrame(f, 11, rejectedFrame)
	onFrame(f, 12, frame("add", `{"selections":[{"id":"`+moved+`","replacedSelectionId":"`+sel+`","points":-12.5}]}`))
	if err := f.onSnapshot(snapAs(t, "+120", moved), nil, sec(19.5), sec(19.8)); err != nil {
		t.Fatal(err)
	}
	if got := shownAs(f, moved); got != "+120" || !f.synced || *f.board.selections[moved].Points != -12.5 {
		t.Fatalf("shown %s synced %v points %v", got, f.synced, *f.board.selections[moved].Points)
	}
}

// A snapshot without a selection that has later evidence rebuilds it from that evidence alone.
func TestRebuildFromEvidence(t *testing.T) {
	f := syncedFeed(t)
	onFrame(f, 10, priceFrame("+110"))
	onFrame(f, 11, rejectedFrame)
	onFrame(f, 12, frame("change", `{"selections":[{"id":"`+sel+`","points":-12.5}]}`))
	if err := f.onSnapshot(snapAs(t, "-108", ""), nil, sec(19.5), sec(19.8)); err != nil {
		t.Fatal(err)
	}
	want(t, f, "unpriced", true)
	x := f.board.selections[sel]
	if x.MarketID == "" || x.OutcomeType == "" || *x.Points != -12.5 {
		t.Fatalf("rebuilt %+v", x)
	}

	open := false
	markets := entries[market]{"m": {v: market{ID: "m", EventID: "e", IsSuspended: &open}, at: map[string]time.Time{"main": sec(12)}}}
	snap := map[string]market{}
	markets.overlay(snap)
	if m := snap["m"]; !isSuspended(m) || m.EventID != "e" {
		t.Fatalf("rebuilt market %+v, want suspended with no suspension evidence", m)
	}
}

func fieldNames[T any](e *entry[T]) []string {
	var names []string
	for k := range e.at {
		names = append(names, k)
	}
	slices.Sort(names)
	return names
}

func TestLostUpdate(t *testing.T) {
	f := syncedFeed(t)
	onFrame(f, 10, priceFrame("+110"))
	f.onLost(1, errors.New("EOF"))
	f.onAck(2, sec(12))
	commit(t, f, "+120", 12.5) // holds an update the old subscription never delivered
	want(t, f, "+120", false)
}

func TestPreAckSnapshot(t *testing.T) {
	f := newFeed("", "", nil)
	f.onAck(1, sec(10))
	commit(t, f, "-108", 9)
	want(t, f, "-108", false)
	if st := f.currentStatus(sec(10.5)); st.State != "polling" {
		t.Fatalf("status %v", st)
	}
	commit(t, f, "-108", 18)
	if st := f.currentStatus(sec(18.5)); st.State != "live" {
		t.Fatalf("status %v", st)
	}
}

func TestRemovedStaysRemoved(t *testing.T) {
	f := syncedFeed(t)
	mkt := f.board.selections[sel].MarketID
	onFrame(f, 10, frame("remove", `{"markets":["`+mkt+`"]}`))
	commit(t, f, "-108", 19)
	if hasKey(f.board.markets, mkt) || f.synced || f.h.Misses != 1 {
		t.Fatalf("market back %v, synced %v, misses %d", hasKey(f.board.markets, mkt), f.synced, f.h.Misses)
	}
}

func TestDrift(t *testing.T) {
	f := syncedFeed(t)
	commit(t, f, "+120", 68)
	if len(f.kill) != 0 || !f.synced {
		t.Fatal("one drift reconnected or unsynced")
	}
	commit(t, f, "+130", 128)
	if len(f.kill) != 1 {
		t.Fatal("two drifts in a row didn't reconnect")
	}
}

// Replaying the captured frames with evidence and then committing the later capture finds nothing
// the frames set that the snapshot spells differently.
func TestReplayEvidence(t *testing.T) {
	for league := range fixtureSubcategory {
		t.Run(league, func(t *testing.T) {
			f := newFeed("", "", nil)
			f.board = NewBoard(fixtureSubcategory[league])
			f.onAck(1, sec(0))
			read := func(which string) []byte {
				b, err := os.ReadFile("testdata/" + league + "-" + which + ".json")
				if err != nil {
					t.Fatal(err)
				}
				return b
			}
			if err := f.onSnapshot(read("start"), nil, sec(0), sec(0)); err != nil {
				t.Fatal(err)
			}
			frames := loadFrames(t, league)
			for i, d := range frames {
				f.onFrame(frameMsg{sub: 1, data: d, recv: sec(float64(i + 1))})
			}
			if f.h.Frames != len(frames) {
				t.Fatalf("applied %d of %d", f.h.Frames, len(frames))
			}
			start := float64(len(frames)+1) + snapshotLag.Seconds()
			end, _ := f.board.parseSnapshot(read("end"))
			if misses := f.ev.check(end, sec(start).Add(-snapshotLag)); len(misses) > 0 {
				t.Fatalf("misses %v", misses)
			}
			if err := f.onSnapshot(read("end"), nil, sec(start), sec(start)); err != nil || !f.synced {
				t.Fatalf("err %v synced %v", err, f.synced)
			}
		})
	}
}

// fakeDK serves the snapshot and socket; the socket acks, then the test pushes frames.
type fakeDK struct {
	mu          sync.Mutex
	snapshot    func() (status int, body []byte) // status 0 hangs until the client gives up
	delay       time.Duration
	socketUp    bool
	deaf        int // this many sockets stop answering pings after the ack
	all         []*websocket.Conn
	starts      []time.Time
	acks        []time.Time
	inFlight    int
	maxInFlight int
	conn        *websocket.Conn
	acked       chan struct{}
}

func newFakeDK(t *testing.T) *fakeDK {
	body, err := os.ReadFile("testdata/nfl-start.json")
	if err != nil {
		t.Fatal(err)
	}
	return &fakeDK{snapshot: serving(body), socketUp: true, acked: make(chan struct{}, 16)}
}

func serving(body []byte) func() (int, []byte) {
	return func() (int, []byte) { return http.StatusOK, body }
}

func (dk *fakeDK) set(fn func()) {
	dk.mu.Lock()
	defer dk.mu.Unlock()
	fn()
}

func (dk *fakeDK) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/socket" {
		dk.serveSocket(w, r)
		return
	}
	dk.mu.Lock()
	dk.starts = append(dk.starts, time.Now())
	dk.inFlight++
	dk.maxInFlight = max(dk.maxInFlight, dk.inFlight)
	snapshot, delay := dk.snapshot, dk.delay
	dk.mu.Unlock()
	defer dk.set(func() { dk.inFlight-- })

	select {
	case <-time.After(delay):
	case <-r.Context().Done():
		return
	}
	status, body := snapshot()
	if status == 0 {
		<-r.Context().Done()
		return
	}
	w.WriteHeader(status)
	w.Write(body)
}

func (dk *fakeDK) serveSocket(w http.ResponseWriter, r *http.Request) {
	dk.mu.Lock()
	up := dk.socketUp
	dk.mu.Unlock()
	if !up {
		http.Error(w, "down", http.StatusServiceUnavailable)
		return
	}
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		return
	}
	ctx := context.Background()
	_, msg, err := c.Read(ctx)
	if err != nil || !strings.Contains(string(msg), `"method":"subscribe"`) {
		c.CloseNow()
		return
	}
	if c.Write(ctx, websocket.MessageText, []byte(`{"id":"nfl-1","event":"subscribed","data":""}`)) != nil {
		return
	}
	dk.mu.Lock()
	deaf := dk.deaf > 0
	if deaf {
		dk.deaf--
	} else {
		c.CloseRead(ctx) // keeps answering pings
	}
	dk.acks = append(dk.acks, time.Now())
	dk.conn = c
	dk.all = append(dk.all, c)
	dk.mu.Unlock()
	dk.acked <- struct{}{}
}

// push sends an update message whose data is `data` on the current socket.
func (dk *fakeDK) push(t *testing.T, data string) {
	t.Helper()
	dk.mu.Lock()
	c := dk.conn
	dk.mu.Unlock()
	msg := `{"event":"update","data":` + data + `,"websocketPublishTimestamp":"` + time.Now().Format(time.RFC3339Nano) + `"}`
	if err := c.Write(context.Background(), websocket.MessageText, []byte(msg)); err != nil {
		t.Fatal(err)
	}
}

func (dk *fakeDK) drop() {
	dk.mu.Lock()
	defer dk.mu.Unlock()
	dk.conn.CloseNow()
}

// pipeListener serves HTTP over net.Pipe, so synctest's fake time can advance.
type pipeListener struct {
	conns chan net.Conn
	done  chan struct{}
	once  sync.Once
}

func (l *pipeListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *pipeListener) Close() error {
	l.once.Do(func() { close(l.done) })
	return nil
}

func (l *pipeListener) Addr() net.Addr { return &net.TCPAddr{} }

func (l *pipeListener) dial(ctx context.Context, _, _ string) (net.Conn, error) {
	client, server := net.Pipe()
	select {
	case l.conns <- server:
		return client, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-l.done:
		return nil, net.ErrClosed
	}
}

type recorder struct {
	mu     sync.Mutex
	rows   map[string]Row
	states []string
}

func (r *recorder) patch(rows []Row, removed []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, row := range rows {
		r.rows[row.ID] = row
	}
	for _, id := range removed {
		delete(r.rows, id)
	}
}

func (r *recorder) status(s Status) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.states = append(r.states, s.State)
}

func (r *recorder) snapshot() (map[string]Row, []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rows := make(map[string]Row, len(r.rows))
	for k, v := range r.rows {
		rows[k] = v
	}
	return rows, slices.Clone(r.states)
}

func (r *recorder) state() string {
	_, states := r.snapshot()
	if len(states) == 0 {
		return ""
	}
	return states[len(states)-1]
}

// runFeed runs a Feed against dk over in-memory connections until the test ends.
func runFeed(t *testing.T, dk *fakeDK) (*Feed, *recorder) {
	l := &pipeListener{conns: make(chan net.Conn), done: make(chan struct{})}
	srv := &http.Server{Handler: dk}
	go srv.Serve(l)
	tr := &http.Transport{DialContext: l.dial}
	f := newFeed("http://dk/snapshot", "ws://dk/socket", &http.Client{Transport: tr, Timeout: netTimeout})
	rec := &recorder{rows: map[string]Row{}}
	f.OnPatch, f.OnStatus = rec.patch, rec.status
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		f.Run(ctx)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
		tr.CloseIdleConnections()
		srv.Close()
		dk.mu.Lock()
		for _, c := range dk.all {
			c.CloseNow()
		}
		dk.mu.Unlock()
	})
	return f, rec
}

func sleep(d time.Duration) {
	time.Sleep(d)
	synctest.Wait()
}

func wantState(t *testing.T, rec *recorder, state string) {
	t.Helper()
	if got := rec.state(); got != state {
		t.Fatalf("state %s, want %s", got, state)
	}
}

func TestStatusCycle(t *testing.T) {
	failures := map[string]func() (int, []byte){
		"403":     func() (int, []byte) { return http.StatusForbidden, nil },
		"garbage": func() (int, []byte) { return http.StatusOK, []byte("garbage") },
		"hang":    func() (int, []byte) { return 0, nil },
	}
	for name, failure := range failures {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				dk := newFakeDK(t)
				_, rec := runFeed(t, dk)
				sleep(12 * time.Second)
				wantState(t, rec, "live")
				before, _ := rec.snapshot()

				ok := dk.snapshot
				dk.set(func() { dk.snapshot, dk.socketUp = failure, false })
				dk.drop()
				sleep(time.Second)
				wantState(t, rec, "polling")
				sleep(15 * time.Second)
				wantState(t, rec, "stale")
				sleep(44 * time.Second) // 60 s offline
				if after, _ := rec.snapshot(); !reflect.DeepEqual(after, before) || len(after) == 0 {
					t.Fatal("board changed while DK was down")
				}

				// Socket backoff is at most 15 s, and the ack drops the snapshot backoff.
				dk.set(func() { dk.snapshot, dk.socketUp = ok, true })
				sleep(reconnectMax + snapshotLag + 2*snapshotSpacing)
				// Recovery may skip polling when its first good snapshot already qualifies.
				_, states := rec.snapshot()
				if want := []string{"connecting", "polling", "live", "polling", "stale"}; !slices.Equal(states[:5], want) || states[len(states)-1] != "live" || slices.Contains(states[5:], "stale") {
					t.Fatalf("states %v", states)
				}
			})
		})
	}
}

func TestSnapshotAfterAck(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		dk := newFakeDK(t)
		_, rec := runFeed(t, dk)
		sleep(12 * time.Second)
		dk.drop()
		sleep(20 * time.Second)
		wantState(t, rec, "live")
		dk.mu.Lock()
		defer dk.mu.Unlock()
		if len(dk.acks) != 2 {
			t.Fatalf("%d acks", len(dk.acks))
		}
		ack := dk.acks[1]
		if !slices.ContainsFunc(dk.starts, func(s time.Time) bool { return !s.Before(ack) && s.Sub(ack) <= snapshotSpacing }) {
			t.Fatalf("no snapshot request right after the ack at %v: %v", ack, dk.starts)
		}
	})
}

func TestHalfOpenSocket(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		dk := newFakeDK(t)
		dk.deaf = 1
		_, rec := runFeed(t, dk)
		acks := func() int {
			dk.mu.Lock()
			defer dk.mu.Unlock()
			return len(dk.acks)
		}
		sleep(pingEvery + netTimeout - time.Second)
		if acks() != 1 {
			t.Fatal("reconnected before the ping timed out")
		}
		sleep(2 * time.Second)
		if acks() != 2 {
			t.Fatal("no reconnect after an unanswered ping")
		}
		sleep(15 * time.Second)
		wantState(t, rec, "live")
	})
}

func TestUnsyncedHealthySocket(t *testing.T) {
	triggers := map[string]func(*testing.T, *fakeDK){
		"reconnect":      func(_ *testing.T, dk *fakeDK) { dk.drop() },
		"rejected frame": func(t *testing.T, dk *fakeDK) { dk.push(t, rejectedFrame) },
	}
	for name, trigger := range triggers {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				dk := newFakeDK(t)
				f, rec := runFeed(t, dk)
				sleep(12 * time.Second)
				wantState(t, rec, "live")
				dk.set(func() { dk.snapshot = func() (int, []byte) { return http.StatusForbidden, nil } })
				trigger(t, dk)
				sleep(90 * time.Second)
				_, states := rec.snapshot()
				if !slices.Equal(states[2:], []string{"live", "polling", "stale"}) || !f.Health().Subscribed {
					t.Fatalf("states %v, subscribed %v", states, f.Health().Subscribed)
				}
			})
		})
	}
}

func TestScheduler(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		dk := newFakeDK(t)
		dk.delay = time.Second
		dk.snapshot = func() (int, []byte) { return http.StatusInternalServerError, nil }
		f, _ := runFeed(t, dk)
		sleep(100 * time.Millisecond)
		for range 50 {
			f.request()
		}
		dk.push(t, rejectedFrame)
		dk.push(t, frame("change", `{"selections":[{"id":"nope","trueOdds":2}]}`))
		sleep(3 * time.Minute)

		dk.mu.Lock()
		defer dk.mu.Unlock()
		if dk.maxInFlight != 1 {
			t.Fatalf("%d requests in flight at once", dk.maxInFlight)
		}
		var gaps []time.Duration
		for i := 1; i < len(dk.starts); i++ {
			gaps = append(gaps, dk.starts[i].Sub(dk.starts[i-1]))
		}
		if slices.Min(gaps) < snapshotSpacing || slices.Max(gaps) <= 2*snapshotSpacing || slices.Max(gaps) > snapshotMaxWait {
			t.Fatalf("gaps %v", gaps)
		}
	})
}

// An ack proves the network is back, so it ends a long snapshot backoff.
func TestAckEndsBackoff(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		dk := newFakeDK(t)
		dk.snapshot = func() (int, []byte) { return http.StatusInternalServerError, nil }
		runFeed(t, dk)
		sleep(2 * time.Minute)
		lastStart := func() time.Time {
			dk.mu.Lock()
			defer dk.mu.Unlock()
			return dk.starts[len(dk.starts)-1]
		}
		for i := 0; i < 600 && time.Since(lastStart()) < 2*snapshotSpacing; i++ {
			sleep(100 * time.Millisecond)
		}
		if time.Since(lastStart()) < 2*snapshotSpacing {
			t.Fatal("never saw a backoff longer than twice the spacing")
		}
		dk.drop()
		sleep(reconnectBase + 100*time.Millisecond)
		dk.mu.Lock()
		defer dk.mu.Unlock()
		if len(dk.acks) != 2 {
			t.Fatalf("%d acks", len(dk.acks))
		}
		if start := dk.starts[len(dk.starts)-1]; start.Before(dk.acks[1]) || start.Sub(dk.acks[1]) > 50*time.Millisecond {
			t.Fatalf("ack at %v, next snapshot at %v", dk.acks[1], start)
		}
	})
}

func TestQuietLive(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		dk := newFakeDK(t)
		_, rec := runFeed(t, dk)
		sleep(5 * time.Minute)
		if _, states := rec.snapshot(); !slices.Equal(states, []string{"connecting", "polling", "live"}) {
			t.Fatalf("states %v", states)
		}
	})
}

// A frame the snapshots never show: the lost-frame rule reconnects, and the new subscription's
// snapshot sets the board.
func TestPersistentStaleSnapshot(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		dk := newFakeDK(t)
		f, rec := runFeed(t, dk)
		sleep(12 * time.Second)
		dk.push(t, priceFrame("+110"))
		sleep(time.Second)

		var firstMiss, reconnected time.Time
		for range 200 {
			sleep(time.Second)
			if firstMiss.IsZero() && f.Health().Misses > 0 {
				firstMiss = time.Now()
			}
			dk.mu.Lock()
			acks := len(dk.acks)
			dk.mu.Unlock()
			if acks == 2 {
				reconnected = time.Now()
				break
			}
			if !firstMiss.IsZero() && rec.state() == "live" {
				t.Fatal("live on the subscription the snapshots disagree with")
			}
		}
		if firstMiss.IsZero() || reconnected.IsZero() {
			t.Fatalf("first miss %v, reconnected %v", firstMiss, reconnected)
		}
		if d := reconnected.Sub(firstMiss); d < lostFrameAfter-time.Second || d > lostFrameAfter+3*time.Second {
			t.Fatalf("reconnected %v after the first miss", d)
		}
		sleep(15 * time.Second)
		wantState(t, rec, "live")
	})
}

// Real sockets and TLS: the handshake, the subscribe message and a frame reach the board.
func TestRealSockets(t *testing.T) {
	dk := newFakeDK(t)
	srv := httptest.NewTLSServer(dk)
	defer srv.Close()
	f := newFeed(srv.URL+"/snapshot", "wss://"+strings.TrimPrefix(srv.URL, "https://")+"/socket", srv.Client())
	patches := make(chan []Row, 64)
	f.OnPatch = func(rows []Row, _ []string) { patches <- rows }
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		f.Run(ctx)
		close(done)
	}()
	defer func() {
		cancel()
		<-done
	}()

	<-dk.acked
	if rows := <-patches; len(rows) == 0 {
		t.Fatal("empty first patch")
	}
	dk.push(t, priceFrame("+110"))
	for rows := range patches {
		for _, r := range rows {
			for _, p := range []*Price{r.Spread.Away, r.Spread.Home} {
				if p != nil && p.American == "+110" {
					return
				}
			}
		}
	}
}
