package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

type sseEvent struct{ name, data string }

func parseEvent(t *testing.T, msg string) sseEvent {
	t.Helper()
	name, data, ok := strings.Cut(strings.TrimSuffix(msg, "\n\n"), "\n")
	if !ok || !strings.HasPrefix(name, "event: ") || !strings.HasPrefix(data, "data: ") {
		t.Fatalf("bad frame %q", msg)
	}
	return sseEvent{strings.TrimPrefix(name, "event: "), strings.TrimPrefix(data, "data: ")}
}

// A client's board plus its patches, in order, must give the hub's final rows, however publishes and
// subscribes interleave.
func TestHandoff(t *testing.T) {
	base := loadBoard(t, "nfl", "start").Games()
	h := NewHub()
	h.Publish(base, nil)

	const nClients, nPatches = 50, 25 // under clientBuffer, so no client is dropped
	clients := make([]chan []byte, nClients)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range clients {
		wg.Go(func() {
			<-start
			clients[i] = h.subscribe()
		})
	}
	wg.Go(func() {
		<-start
		for i := range nPatches {
			r := base[i]
			if i%5 == 4 {
				h.Publish(nil, []string{r.ID})
				continue
			}
			r.Moneyline.Home = &Price{American: fmt.Sprint(100 + i)}
			h.Publish([]Row{r}, nil)
		}
	})
	close(start)
	wg.Wait()
	want := js(h.rows)

	for i, c := range clients {
		h.unsubscribe(c)
		rows := map[string]Row{}
		n := 0
		for msg := range c {
			ev := parseEvent(t, string(msg))
			switch {
			case n == 0 && ev.name == "board":
				var b boardEvent
				if err := json.Unmarshal([]byte(ev.data), &b); err != nil {
					t.Fatal(err)
				}
				for _, r := range b.Rows {
					rows[r.ID] = r
				}
			case n > 0 && ev.name == "patch":
				var p patchEvent
				if err := json.Unmarshal([]byte(ev.data), &p); err != nil {
					t.Fatal(err)
				}
				for _, r := range p.Rows {
					rows[r.ID] = r
				}
				for _, id := range p.Removed {
					delete(rows, id)
				}
			default:
				t.Fatalf("client %d: message %d is %s", i, n, ev.name)
			}
			n++
		}
		if got := js(rows); got != want {
			t.Fatalf("client %d: board + patches != final\ngot  %s\nwant %s", i, got, want)
		}
	}
}

func TestSlowClient(t *testing.T) {
	h := NewHub()
	c := h.subscribe()
	for range clientBuffer {
		h.Publish(nil, []string{"x"})
	}
	n := 0
	for range c {
		n++
	}
	if n != clientBuffer || len(h.clients) != 0 {
		t.Fatalf("got %d messages, %d clients left; want the full buffer, then dropped", n, len(h.clients))
	}
}

func TestHeartbeat(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := &pipeListener{conns: make(chan net.Conn), done: make(chan struct{})}
		srv := &http.Server{Handler: NewHub()}
		go srv.Serve(l)
		tr := &http.Transport{DialContext: l.dial}
		ctx, cancel := context.WithCancel(t.Context())
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://hub/events", nil)
		resp, err := (&http.Client{Transport: tr}).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			cancel()
			resp.Body.Close()
			tr.CloseIdleConnections()
			srv.Close()
		}()
		if ct, cc := resp.Header.Get("Content-Type"), resp.Header.Get("Cache-Control"); ct != "text/event-stream" || cc != "no-cache, no-transform" {
			t.Fatalf("content-type %q, cache-control %q", ct, cc)
		}

		rd := bufio.NewReader(resp.Body)
		next := func() sseEvent {
			var msg strings.Builder
			for !strings.HasSuffix(msg.String(), "\n\n") {
				line, err := rd.ReadString('\n')
				if err != nil {
					t.Fatal(err)
				}
				msg.WriteString(line)
			}
			return parseEvent(t, msg.String())
		}
		begin := time.Now()
		if ev := next(); ev.name != "board" {
			t.Fatalf("first event %s", ev.name)
		}
		for i := 1; i <= 3; i++ {
			ev := next()
			if ev.name != "status" || !strings.Contains(ev.data, `"state":"connecting"`) {
				t.Fatalf("heartbeat %d: %+v", i, ev)
			}
			if got := time.Since(begin); got != time.Duration(i)*heartbeatEvery {
				t.Fatalf("heartbeat %d at %v", i, got)
			}
		}
	})
}

func TestEmptyBoard(t *testing.T) {
	ev := parseEvent(t, string(<-NewHub().subscribe()))
	if ev.name != "board" || !strings.Contains(ev.data, `"rows":[]`) {
		t.Fatalf("bootstrap before the first snapshot: %+v", ev)
	}
}

func TestAppJS(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH")
	}
	if out, err := exec.Command(node, "web_test.js").CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
}

func TestRoutes(t *testing.T) {
	h := routes(NewFeed(), NewHub())
	for path, want := range map[string]string{"/": `src="app.js"`, "/app.js": "EventSource", "/healthz": `"status"`} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK || rec.Header().Get("Content-Security-Policy") != csp || !strings.Contains(rec.Body.String(), want) {
			t.Errorf("%s: %d, CSP %q, body lacks %q", path, rec.Code, rec.Header().Get("Content-Security-Policy"), want)
		}
		if path == "/app.js" {
			for _, sink := range []string{"innerHTML", "outerHTML", "insertAdjacentHTML", "document.write"} {
				if strings.Contains(rec.Body.String(), sink) {
					t.Errorf("app.js uses %s; DK strings go through textContent only", sink)
				}
			}
		}
	}
}
