package main

// Throwaway: runs once at startup, before the feed, to find which snapshot request Akamai accepts on Render.

import (
	"bufio"
	"compress/gzip"
	"compress/zlib"
	"context"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptrace"
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

// Runs in init so no production snapshot request overlaps the probes.
func init() {
	if testing.Testing() {
		return
	}
	probes := []struct {
		name string
		run  func(context.Context, *probeResult) error
	}{
		{"A current client", probeCurrent},
		{"D Node ClientHello + raw Node bytes", func(ctx context.Context, r *probeResult) error { return probeRaw(ctx, r, true) }},
	}
	for i, p := range probes {
		if i > 0 {
			time.Sleep(3 * time.Second)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		var r probeResult
		err := p.run(ctx, &r)
		cancel()
		slog.Info("PROBE", "variant", p.name, "status", r.status, "bytes", r.bytes, "json", r.json,
			"remote", r.remote, "alpn", r.alpn, "err", err)
	}
	time.Sleep(3 * time.Second)
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

func probeNodeHeaders(ctx context.Context, r *probeResult) error {
	req, _ := http.NewRequestWithContext(traceConn(ctx, r), http.MethodGet, snapshotURL, nil)
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/128.0.0.0 Safari/537.36")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Accept-Language", "en-CA,en;q=0.9")
	req.Header.Set("Origin", dkOrigin)
	req.Header.Set("Referer", dkOrigin+"/")
	req.Header.Set("Connection", "keep-alive")
	req.Header.Set("Sec-Fetch-Mode", "cors")
	req.Header.Set("Accept-Encoding", "gzip, deflate")
	resp, err := dkClient.Do(req)
	if err != nil {
		return err
	}
	return readProbe(resp, r)
}

func probeRaw(ctx context.Context, r *probeResult, nodeHello bool) error {
	var d net.Dialer
	raw, err := d.DialContext(ctx, "tcp", probeHost+":443")
	if err != nil {
		return err
	}
	defer raw.Close()
	raw.SetDeadline(time.Now().Add(10 * time.Second))
	r.remote = raw.RemoteAddr().String()
	var conn net.Conn
	if nodeHello {
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
		conn = uc
	} else {
		tc := tls.Client(raw, &tls.Config{ServerName: probeHost, NextProtos: []string{"http/1.1"}})
		if err := tc.HandshakeContext(ctx); err != nil {
			return err
		}
		r.alpn = tc.ConnectionState().NegotiatedProtocol
		conn = tc
	}
	if _, err := io.WriteString(conn, nodeRequest); err != nil {
		return err
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
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
