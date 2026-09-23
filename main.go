package main

import (
	"context"
	"log/slog"
	"os"
	"time"
)

// ponytail: step 1 probe only; step 5 replaces this with the server.
func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	ok := true

	start := time.Now()
	body, err := fetchSnapshot(ctx, dkClient, snapshotURL)
	if err != nil {
		slog.Error("snapshot", "err", err, "took", time.Since(start))
		ok = false
	} else {
		slog.Info("snapshot", "bytes", len(body), "took", time.Since(start))
	}

	start = time.Now()
	conn, ack, err := dialAndSubscribe(ctx, dkClient, socketURL, nflSubscription)
	if err != nil {
		slog.Error("socket", "err", err, "took", time.Since(start))
		ok = false
	} else {
		slog.Info("socket subscribed", "took", time.Since(start), "publishTimestamp", string(ack.WebsocketPublishTimestamp))
		conn.CloseNow()
	}

	if !ok {
		os.Exit(1)
	}
}
