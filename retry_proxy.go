package main

import (
	"bytes"
	"context"
	"errors"
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
	proxies := state.rules[authIndex].RetryProxies
	if len(proxies) == 0 {
		return ""
	}
	return proxies[rand.IntN(len(proxies))]
}

func doRetryHTTP(ctx context.Context, request hostHTTPRequest, proxyURL string) (hostHTTPResponse, error) {
	// Build from scratch: nil Proxy explicitly ignores environment/CPA proxies.
	transport := &http.Transport{
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		DisableCompression:    true,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
	}
	defer transport.CloseIdleConnections()
	if proxyURL != "" {
		parsed, err := parseRetryProxy(proxyURL)
		if err != nil {
			return hostHTTPResponse{}, err
		}
		transport.Proxy = http.ProxyURL(parsed)
	}
	client := &http.Client{
		Transport:     transport,
		Timeout:       30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	req, err := http.NewRequestWithContext(ctx, request.Method, request.URL, bytes.NewReader(request.Body))
	if err != nil {
		return hostHTTPResponse{}, errors.New("invalid retry request")
	}
	req.Header = request.Headers.Clone()
	response, err := client.Do(req)
	if err != nil {
		// Transport errors can include the proxy URL and its credentials.
		if ctx.Err() != nil {
			return hostHTTPResponse{}, ctx.Err()
		}
		if proxyURL != "" {
			return hostHTTPResponse{}, errors.New("SOCKS retry request failed; check proxy connectivity and authentication")
		}
		return hostHTTPResponse{}, errors.New("direct retry request failed; check connectivity")
	}
	defer response.Body.Close() // Only the state header is needed; never buffer SSE.
	return hostHTTPResponse{StatusCode: response.StatusCode, Headers: response.Header.Clone()}, nil
}
