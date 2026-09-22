package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// CPA's host.http.do has no per-request proxy override. Background retries
// therefore own their transport; normal traffic and manual tests still use CPA.
var retryHTTPDoFunc = doRetryHTTP

func parseRetryProxy(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u == nil {
		return nil, errors.New("invalid SOCKS proxy URL")
	}
	port, err := strconv.Atoi(u.Port())
	if (u.Scheme != "socks5" && u.Scheme != "socks5h") || u.Hostname() == "" || err != nil || port < 1 || port > 65535 ||
		(u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return nil, errors.New("use socks5://host:port or socks5h://user:password@host:port, without path/query/fragment")
	}
	if u.User != nil {
		password, _ := u.User.Password()
		if len(u.User.Username()) > 255 || len(password) > 255 {
			return nil, errors.New("SOCKS username and password must each be at most 255 bytes")
		}
	}
	u.Path = ""
	return u, nil
}

func retryProxyFor(authIndex string) string {
	state.mu.Lock()
	defer state.mu.Unlock()
	proxies := state.rules[authIndex].retryProxyPool()
	if len(proxies) == 0 {
		return ""
	}
	return proxies[rand.IntN(len(proxies))]
}

// newBackgroundTransport builds a transport from scratch: a nil Proxy field
// explicitly ignores the environment's and CPA's own proxies, which is the
// point -- these requests are the only ones that choose their own egress.
//
// The caller owns it, because a probe needs two requests to leave through the
// same exit. A rotating pool changes address per connection, and a SOCKS5
// tunnel is one connection to one exit, so keeping the transport alive across
// the pair is what makes "prime a cookie, then use it" mean anything.
func newBackgroundTransport(proxyURL string) (*http.Transport, error) {
	transport := &http.Transport{
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		DisableCompression:    true,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
	}
	if proxyURL != "" {
		parsed, err := parseRetryProxy(proxyURL)
		if err != nil {
			return nil, err
		}
		transport.Proxy = http.ProxyURL(parsed)
	}
	return transport, nil
}

// maxBackgroundBodyBytes bounds what a background request reads back. The body
// is wanted for the model it declares and for the capped copy the detail
// shows; an SSE stream is unbounded, so it is read through a limit rather than
// buffered whole.
const maxBackgroundBodyBytes = maxStreamBufferBytes

func doHTTPOverTransport(ctx context.Context, transport *http.Transport, request hostHTTPRequest, proxied bool) (hostHTTPResponse, error) {
	client := &http.Client{
		Transport:     transport,
		Timeout:       30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	req, err := http.NewRequestWithContext(ctx, request.Method, request.URL, bytes.NewReader(request.Body))
	if err != nil {
		return hostHTTPResponse{}, errors.New("invalid background request")
	}
	req.Header = request.Headers.Clone()
	response, err := client.Do(req)
	if err != nil {
		// Transport errors can include the proxy URL and its credentials.
		if ctx.Err() != nil {
			return hostHTTPResponse{}, ctx.Err()
		}
		if proxied {
			return hostHTTPResponse{}, errors.New("SOCKS background request failed; check proxy connectivity and authentication")
		}
		return hostHTTPResponse{}, errors.New("direct background request failed; check connectivity")
	}
	defer response.Body.Close()
	// Drained to the limit rather than abandoned: leaving bytes unread stops
	// the connection being reused, and the pair of requests a probe makes
	// depends on reuse to keep the same exit.
	body, _ := io.ReadAll(io.LimitReader(response.Body, maxBackgroundBodyBytes))
	return hostHTTPResponse{StatusCode: response.StatusCode, Headers: response.Header.Clone(), Body: body}, nil
}

func doRetryHTTP(ctx context.Context, request hostHTTPRequest, proxyURL string) (hostHTTPResponse, error) {
	transport, err := newBackgroundTransport(proxyURL)
	if err != nil {
		return hostHTTPResponse{}, err
	}
	defer transport.CloseIdleConnections()
	return doHTTPOverTransport(ctx, transport, request, proxyURL != "")
}
