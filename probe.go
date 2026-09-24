package main

// Throwaway: runs once at startup, before the feed, to find why Akamai on Render accepts Node but not Go.

import (
	"bufio"
	"compress/gzip"
	"compress/zlib"
	"context"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptrace"
	"os/exec"
	"strings"
	"testing"
	"time"

	utls "github.com/refraction-networking/utls"
)

const probeHost = "sportsbook-nash.draftkings.com"

// Byte for byte what Node's fetch sends for dk-scraper's headers.
const nodeRequest = "GET /api/sportscontent/dkcaon/v1/leagues/88808 HTTP/1.1\r\n" +
	"host: " + probeHost + "\r\n" +
	"connection: keep-alive\r\n" +
	"User-Agent: Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/128.0.0.0 Safari/537.36\r\n" +
	"Accept: application/json, text/plain, */*\r\n" +
	"Accept-Language: en-CA,en;q=0.9\r\n" +
	"Origin: https://sportsbook.draftkings.com\r\n" +
	"Referer: https://sportsbook.draftkings.com/\r\n" +
	"sec-fetch-mode: cors\r\n" +
	"accept-encoding: gzip, deflate\r\n\r\n"

type probeResult struct {
	status, bytes int
	json          bool
	remote, alpn  string
}

// Runs in init so the feed's snapshot requests can't overlap the probes. Go and Node alternate,
// and D repeats at Node's edge IP, to separate client, edge and ordering effects.
func init() {
	if testing.Testing() {
		return
	}
	var nodeEdge string
	probes := []struct {
		name string
		run  func(context.Context, *probeResult) error
	}{
		{"D uTLS Node hello + Node bytes", func(ctx context.Context, r *probeResult) error { return probeRaw(ctx, r, probeHost+":443") }},
		{"N Node 22.20 fetch", func(ctx context.Context, r *probeResult) error {
			err := probeNode(ctx, r)
			nodeEdge = r.remote
			return err
		}},
		{"D at Node's edge", func(ctx context.Context, r *probeResult) error { return probeRaw(ctx, r, nodeEdge) }},
		{"N Node 22.20 fetch", probeNode},
		{"A current client", probeCurrent},
	}
	for i, p := range probes {
		if i > 0 {
			time.Sleep(3 * time.Second)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
		var r probeResult
		err := p.run(ctx, &r)
		cancel()
		slog.Info("PROBE", "variant", p.name, "status", r.status, "bytes", r.bytes, "json", r.json,
			"remote", r.remote, "alpn", r.alpn, "err", err)
	}
	time.Sleep(3 * time.Second)
}

func probeNode(ctx context.Context, r *probeResult) error {
	out, err := exec.CommandContext(ctx, "node", "/probe.mjs").CombinedOutput()
	line := strings.TrimSpace(string(out))
	if _, err := fmt.Sscanf(line, "status=%d bytes=%d json=%t remote=%s", &r.status, &r.bytes, &r.json, &r.remote); err != nil {
		return fmt.Errorf("%q: %w", line, err)
	}
	return err
}

func traceConn(ctx context.Context, r *probeResult) context.Context {
	return httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{GotConn: func(ci httptrace.GotConnInfo) {
		r.remote = ci.Conn.RemoteAddr().String()
		if tc, ok := ci.Conn.(*tls.Conn); ok {
			r.alpn = tc.ConnectionState().NegotiatedProtocol
		}
	}})
}

func probeCurrent(ctx context.Context, r *probeResult) error {
	b, err := fetchSnapshot(traceConn(ctx, r), dkClient, snapshotURL)
	if err != nil {
		return err
	}
	r.status, r.bytes, r.json = 200, len(b), json.Valid(b)
	return nil
}

func probeRaw(ctx context.Context, r *probeResult, addr string) error {
	if addr == "" {
		return errors.New("no address")
	}
	var d net.Dialer
	raw, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	defer raw.Close()
	raw.SetDeadline(time.Now().Add(10 * time.Second))
	r.remote = raw.RemoteAddr().String()
	uc := utls.UClient(raw, &utls.Config{ServerName: probeHost}, utls.HelloCustom)
	b, err := hex.DecodeString(strings.TrimSpace(nodeHelloHex))
	if err != nil {
		return err
	}
	spec, err := (&utls.Fingerprinter{AllowBluntMimicry: true}).FingerprintClientHello(b)
	if err != nil {
		return err
	}
	if err := uc.ApplyPreset(spec); err != nil {
		return err
	}
	if err := uc.HandshakeContext(ctx); err != nil {
		return err
	}
	r.alpn = uc.ConnectionState().NegotiatedProtocol
	if _, err := io.WriteString(uc, nodeRequest); err != nil {
		return err
	}
	resp, err := http.ReadResponse(bufio.NewReader(uc), nil)
	if err != nil {
		return err
	}
	return readProbe(resp, r)
}

func readProbe(resp *http.Response, r *probeResult) error {
	defer resp.Body.Close()
	r.status = resp.StatusCode
	body := io.Reader(resp.Body)
	switch resp.Header.Get("Content-Encoding") {
	case "gzip":
		zr, err := gzip.NewReader(body)
		if err != nil {
			return err
		}
		body = zr
	case "deflate":
		zr, err := zlib.NewReader(body)
		if err != nil {
			return err
		}
		body = zr
	}
	b, err := io.ReadAll(io.LimitReader(body, maxSnapshotSize+1))
	r.bytes, r.json = len(b), json.Valid(b)
	return err
}
