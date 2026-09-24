package main

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"sync"
	"time"
)

const (
	heartbeatEvery = 15 * time.Second
	clientBuffer   = 32
	writeTimeout   = 10 * time.Second
)

// Hub fans board changes out to SSE clients. Rows and clients share one mutex, so a new client's
// board and the patches after it can't miss or repeat a change; see PROJECT_PLAN.md "Fan-out".
type Hub struct {
	Delay func() float64 // p50 ms from DK creating a change to it leaving us, for the status line

	mu      sync.Mutex
	rows    map[string]Row
	status  Status
	clients map[chan []byte]struct{}
}

func NewHub() *Hub {
	return &Hub{rows: map[string]Row{}, status: Status{State: "connecting"}, clients: map[chan []byte]struct{}{}}
}

type statusEvent struct {
	Status
	DelayP50ms float64 `json:"delayP50ms,omitempty"`
}

type boardEvent struct {
	Rows   []Row       `json:"rows"`
	Status statusEvent `json:"status"`
}

type patchEvent struct {
	Rows    []Row    `json:"rows,omitempty"`
	Removed []string `json:"removed,omitempty"`
}

func sseFrame(event string, v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err) // our own types always encode
	}
	return fmt.Appendf(nil, "event: %s\ndata: %s\n\n", event, b)
}

// Publish is Feed.OnPatch.
func (h *Hub) Publish(rows []Row, removed []string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, r := range rows {
		h.rows[r.ID] = r
	}
	for _, id := range removed {
		delete(h.rows, id)
	}
	h.broadcast(sseFrame("patch", patchEvent{rows, removed}))
}

// SetStatus is Feed.OnStatus.
func (h *Hub) SetStatus(st Status) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.status = st
	h.broadcast(sseFrame("status", h.statusEvent()))
}

// broadcast never blocks: a client whose buffer is full is dropped, and reconnects to a fresh board.
func (h *Hub) broadcast(msg []byte) {
	for c := range h.clients {
		select {
		case c <- msg:
		default:
			delete(h.clients, c)
			close(c)
		}
	}
}

func (h *Hub) statusEvent() statusEvent {
	ev := statusEvent{Status: h.status}
	if h.Delay != nil {
		ev.DelayP50ms = h.Delay()
	}
	return ev
}

func (h *Hub) subscribe() chan []byte {
	h.mu.Lock()
	defer h.mu.Unlock()
	rows := append([]Row{}, slices.SortedFunc(maps.Values(h.rows), byStart)...) // [] not null, for app.js
	c := make(chan []byte, clientBuffer)
	c <- sseFrame("board", boardEvent{rows, h.statusEvent()})
	h.clients[c] = struct{}{}
	return c
}

func (h *Hub) unsubscribe(c chan []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.clients[c]; ok {
		delete(h.clients, c)
		close(c)
	}
}

func (h *Hub) heartbeat() []byte {
	h.mu.Lock()
	defer h.mu.Unlock()
	return sseFrame("status", h.statusEvent())
}

func (h *Hub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("X-Accel-Buffering", "no")
	rc := http.NewResponseController(w)
	c := h.subscribe()
	defer h.unsubscribe(c)
	hb := time.NewTicker(heartbeatEvery)
	defer hb.Stop()
	for {
		var msg []byte
		select {
		case <-r.Context().Done():
			return
		case m, ok := <-c:
			if !ok {
				return
			}
			msg = m
		case <-hb.C:
			msg = h.heartbeat()
		}
		rc.SetWriteDeadline(time.Now().Add(writeTimeout))
		if _, err := w.Write(msg); err != nil {
			return
		}
		if rc.Flush() != nil {
			return
		}
	}
}
