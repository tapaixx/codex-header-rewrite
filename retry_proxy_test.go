package main

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Dropping the new field or failing to validate it loses operator intent.
func TestRetryProxyRuleValidation(t *testing.T) {
	for _, tc := range []struct {
		name, value string
		invalid     bool
	}{
		{"valid", `[" socks5://user:secret@127.0.0.1:1080 ","","socks5://user:secret@127.0.0.1:1080","socks5h://[::1]:1081"]`, false},
		{"wrong scheme", `["http://user:secret@localhost:1080"]`, true},
		{"missing port", `["socks5://user:secret@localhost"]`, true},
		{"bad port", `["socks5://user:secret@localhost:65536"]`, true},
		{"path", `["socks5://user:secret@localhost:1080/path"]`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var rule headerRule
			if err := json.Unmarshal([]byte(`{"auth_index":"a","retry_proxies":`+tc.value+`}`), &rule); err != nil {
				t.Fatal(err)
			}
			got, err := validateRule(rule)
			if tc.invalid {
				if err == nil {
					t.Fatal("invalid proxy accepted")
				}
				if strings.Contains(err.Error(), "secret") {
					t.Fatal("proxy password leaked")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(got)
			var fields map[string]json.RawMessage
			_ = json.Unmarshal(raw, &fields)
			if string(fields["retry_proxies"]) != `["socks5://user:secret@127.0.0.1:1080","socks5h://[::1]:1081"]` {
				t.Fatalf("proxy list lost or not normalized: %s", fields["retry_proxies"])
			}
		})
	}
}

func TestRetryTransportDirectAndSOCKS(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer fixture" || r.Header.Get("Proxy-Authorization") != "" {
			t.Error("wrong upstream credentials")
		}
		w.Header().Set(turnStateHeader, "fixture-state")
		w.WriteHeader(200)
	}))
	defer upstream.Close()
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	req := hostHTTPRequest{Method: "POST", URL: upstream.URL, Headers: http.Header{"Authorization": {"Bearer fixture"}}}
	resp, err := doRetryHTTP(context.Background(), req, "")
	if err != nil || headerTurnState(resp.Headers) != "fixture-state" {
		t.Fatalf("direct: %v, %v", resp, err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	connected := make(chan string, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		var head [2]byte
		if _, err := io.ReadFull(conn, head[:]); err != nil {
			return
		}
		if _, err := io.CopyN(io.Discard, conn, int64(head[1])); err != nil {
			return
		}
		_, _ = conn.Write([]byte{5, 0})
		var connect [5]byte
		if _, err := io.ReadFull(conn, connect[:]); err != nil {
			return
		}
		if connect[3] != 3 {
			return
		} // Domain resolution must be left to the proxy.
		hostPort := make([]byte, int(connect[4])+2)
		if _, err := io.ReadFull(conn, hostPort); err != nil {
			return
		}
		connected <- string(hostPort[:len(hostPort)-2])
		target, err := net.Dial("tcp", strings.TrimPrefix(upstream.URL, "http://"))
		if err != nil {
			return
		}
		defer target.Close()
		_, _ = conn.Write([]byte{5, 0, 0, 1, 127, 0, 0, 1, 0, 80})
		go io.Copy(target, conn)
		_, _ = io.Copy(conn, target)
	}()
	req.URL = "http://proxy-only.invalid/"
	resp, err = doRetryHTTP(context.Background(), req, "socks5h://"+listener.Addr().String())
	if err != nil || headerTurnState(resp.Headers) != "fixture-state" {
		t.Fatalf("SOCKS: %v, %v", resp, err)
	}
	if got := <-connected; got != "proxy-only.invalid" {
		t.Fatalf("wrong destination %q", got)
	}
	// A refused SOCKS connection must not fall back to a reachable direct target.
	req.URL = upstream.URL
	resp, err = doRetryHTTP(context.Background(), req, "socks5://user:secret@127.0.0.1:1")
	if err == nil || resp.StatusCode != 0 || strings.Contains(err.Error(), "secret") {
		t.Fatalf("fallback or credential leak: %v %v", resp, err)
	}
}

func TestRetryTransportDoesNotFollowRedirectAndHonoursCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			t.Error("retry followed redirect")
		}
		http.Redirect(w, r, "/other", http.StatusFound)
	}))
	defer server.Close()
	req := hostHTTPRequest{Method: "POST", URL: server.URL}
	resp, err := doRetryHTTP(context.Background(), req, "")
	if err != nil || resp.StatusCode != 302 {
		t.Fatalf("redirect: %v %v", resp, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := doRetryHTTP(ctx, req, ""); err == nil {
		t.Fatal("ignored cancellation")
	}
}
