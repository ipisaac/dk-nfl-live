package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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
	snapshotTimeout = 5 * time.Second
	maxSnapshotSize = 8 << 20
)

var snapshotClient = &http.Client{Timeout: snapshotTimeout}

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

func fetchSnapshot(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Accept-Language", "en-CA,en;q=0.9") // Akamai 403s without it
	req.Header.Set("Origin", dkOrigin)
	req.Header.Set("Referer", dkOrigin+"/")

	resp, err := snapshotClient.Do(req)
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

// dialAndSubscribe returns once DK has acked the subscription.
func dialAndSubscribe(ctx context.Context, q subscriptionQuery) (*websocket.Conn, socketMessage, error) {
	conn, _, err := websocket.Dial(ctx, socketURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"Origin": {dkOrigin}, "User-Agent": {userAgent}},
	})
	if err != nil {
		return nil, socketMessage{}, err
	}
	conn.SetReadLimit(maxSnapshotSize)

	const id = "nfl-1"
	sub := rpcRequest{JSONRPC: "2.0", Method: "subscribe", ID: id, Params: subscribe{
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
		if json.Unmarshal(raw, &m) == nil && m.ID == id && m.Event == "subscribed" {
			return conn, m, nil
		}
	}
}
