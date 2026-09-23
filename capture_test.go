//go:build capture

package main

import (
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// TestCapture records a league's raw socket frames and every distinct snapshot into captures/, for
// step 2's fixtures and snapshotLag measurement. It is a tool, not a test:
//
//	CAPTURE_LEAGUE=84240 CAPTURE_FOR=30m go test -tags capture -run TestCapture -timeout 0 -v
func TestCapture(t *testing.T) {
	league := envOr("CAPTURE_LEAGUE", leagueID)
	dur, err := time.ParseDuration(envOr("CAPTURE_FOR", "10m"))
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join("captures", league+"-"+time.Now().UTC().Format("20060102T150405Z"))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), dur)
	defer cancel()

	url := leaguesURL + league
	body, err := fetchSnapshot(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	var snap struct {
		SubscriptionPartials map[string]subscriptionQuery `json:"subscriptionPartials"`
	}
	if err := json.Unmarshal(body, &snap); err != nil {
		t.Fatal(err)
	}
	q, ok := snap.SubscriptionPartials["league-events-"+league]
	if !ok {
		t.Fatalf("no league-events-%s partial", league)
	}

	frames := newJSONL(t, filepath.Join(dir, "frames.jsonl"))
	snaps := newJSONL(t, filepath.Join(dir, "snapshots.jsonl"))
	var wg sync.WaitGroup
	wg.Go(func() { captureFrames(ctx, q, frames) })
	captureSnapshots(ctx, t, url, dir, snaps)
	wg.Wait()
	t.Logf("wrote %s", dir)
}

type frameLine struct {
	Sub  int             `json:"sub"`
	Recv time.Time       `json:"recv"`
	Msg  json.RawMessage `json:"msg,omitempty"`
	Text string          `json:"text,omitempty"`
	Err  string          `json:"err,omitempty"`
}

func captureFrames(ctx context.Context, q subscriptionQuery, out *jsonl) {
	for sub := 1; ctx.Err() == nil; sub++ {
		conn, ack, err := dialAndSubscribe(ctx, q)
		if err != nil {
			out.write(frameLine{Sub: sub, Recv: time.Now(), Err: err.Error()})
			sleepCtx(ctx, time.Second)
			continue
		}
		raw, _ := json.Marshal(ack)
		out.write(frameLine{Sub: sub, Recv: time.Now(), Msg: raw})
		for {
			_, raw, err := conn.Read(ctx)
			recv := time.Now()
			if err != nil {
				out.write(frameLine{Sub: sub, Recv: recv, Err: err.Error()})
				conn.CloseNow()
				break
			}
			if json.Valid(raw) {
				out.write(frameLine{Sub: sub, Recv: recv, Msg: raw})
			} else {
				out.write(frameLine{Sub: sub, Recv: recv, Text: string(raw)})
			}
		}
	}
}

type snapshotLine struct {
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
	SHA   string    `json:"sha,omitempty"`
	Err   string    `json:"err,omitempty"`
}

// captureSnapshots polls with starts 2 s apart, the scheduler's spacing, and keeps each distinct body once.
func captureSnapshots(ctx context.Context, t *testing.T, url, dir string, out *jsonl) {
	seen := map[string]bool{}
	for {
		start := time.Now()
		body, err := fetchSnapshot(ctx, url)
		line := snapshotLine{Start: start, End: time.Now()}
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) && ctx.Err() != nil {
				return
			}
			line.Err = err.Error()
		} else {
			sum := sha256.Sum256(body)
			line.SHA = hex.EncodeToString(sum[:8])
			if !seen[line.SHA] {
				seen[line.SHA] = true
				if err := writeGzip(filepath.Join(dir, "snap-"+line.SHA+".json.gz"), body); err != nil {
					t.Error(err)
				}
			}
		}
		out.write(line)
		sleepCtx(ctx, time.Until(start.Add(2*time.Second)))
		if ctx.Err() != nil {
			return
		}
	}
}

type jsonl struct {
	mu  sync.Mutex
	enc *json.Encoder
}

func newJSONL(t *testing.T, path string) *jsonl {
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return &jsonl{enc: json.NewEncoder(f)}
}

func (j *jsonl) write(v any) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.enc.Encode(v)
}

func writeGzip(path string, b []byte) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	zw := gzip.NewWriter(f)
	if _, err := zw.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := zw.Close(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func sleepCtx(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
