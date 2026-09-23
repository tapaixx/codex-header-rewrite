package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

// The allowance is read as the upstream reports it: percentages as given,
// resets as absolute times, with Reset-After-Seconds as the fallback.
func TestQuotaFromHeaders(t *testing.T) {
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	h := http.Header{
		"X-Codex-Primary-Used-Percent":          {"47"},
		"X-Codex-Primary-Reset-At":              {"1790079259"},
		"X-Codex-Primary-Window-Minutes":        {"300"},
		"X-Codex-Secondary-Used-Percent":        {"15.5"},
		"X-Codex-Secondary-Reset-After-Seconds": {"3600"},
		"X-Codex-Secondary-Window-Minutes":      {"10080"},
	}
	q, ok := quotaFromHeaders(h, now)
	if !ok {
		t.Fatal("a response with a primary percentage carries a quota")
	}
	if q.PrimaryUsedPercent != 47 || q.SecondaryUsedPercent != 15.5 || q.PrimaryWindowMinutes != 300 || q.SecondaryWindowMinutes != 10080 {
		t.Fatalf("misread: %+v", q)
	}
	if !q.PrimaryResetAt.Equal(time.Unix(1790079259, 0)) {
		t.Fatalf("primary reset should come from Reset-At: %v", q.PrimaryResetAt)
	}
	if !q.SecondaryResetAt.Equal(now.Add(time.Hour)) {
		t.Fatalf("secondary reset should fall back to Reset-After-Seconds: %v", q.SecondaryResetAt)
	}
	if _, ok := quotaFromHeaders(http.Header{"X-Codex-Secondary-Used-Percent": {"3"}}, now); ok {
		t.Fatal("no primary percentage, no quota")
	}
}

// The usage endpoint gives the same two windows the headers do.
func TestQuotaFromUsage(t *testing.T) {
	now := time.Unix(1790180000, 0)
	body := []byte(`{"user_id":"u","email":"x@example.com","plan_type":"team","rate_limit":{"allowed":true,
		"primary_window":{"used_percent":2,"limit_window_seconds":18000,"reset_after_seconds":2808,"reset_at":1790186084},
		"secondary_window":{"used_percent":23,"limit_window_seconds":604800,"reset_after_seconds":247491}}}`)
	quota, ok := quotaFromUsage(body, now)
	if !ok {
		t.Fatal("rate limits not read")
	}
	if quota.PrimaryUsedPercent != 2 || quota.PrimaryWindowMinutes != 300 || !quota.PrimaryResetAt.Equal(time.Unix(1790186084, 0)) {
		t.Fatalf("primary: %+v", quota)
	}
	if quota.SecondaryUsedPercent != 23 || quota.SecondaryWindowMinutes != 10080 || !quota.SecondaryResetAt.Equal(now.Add(247491*time.Second)) {
		t.Fatalf("secondary, reset from reset_after_seconds: %+v", quota)
	}
	if _, ok := quotaFromUsage([]byte(`{"rate_limit":null}`), now); ok {
		t.Fatal("no rate limits, no reading")
	}
}

// A refresh reads the credential, asks the usage endpoint with it, and never
// sends a request body or a cookie.
func TestRefreshQuotaAsksTheUsageEndpoint(t *testing.T) {
	oldGet, oldDo := hostAuthGetFunc, hostHTTPDoFunc
	t.Cleanup(func() { hostAuthGetFunc, hostHTTPDoFunc = oldGet, oldDo })
	hostAuthGetFunc = func(string) (json.RawMessage, error) {
		return json.RawMessage(`{"access_token":"tok","account_id":"acct"}`), nil
	}
	var sent hostHTTPRequest
	hostHTTPDoFunc = func(req hostHTTPRequest) (hostHTTPResponse, error) {
		sent = req
		return hostHTTPResponse{StatusCode: 200, Body: []byte(`{"rate_limit":{"primary_window":{"used_percent":40,"limit_window_seconds":18000,"reset_at":1790186084}}}`)}, nil
	}
	quota, err := refreshQuota("idx")
	if err != nil || quota.PrimaryUsedPercent != 40 {
		t.Fatalf("quota=%+v err=%v", quota, err)
	}
	if sent.Method != http.MethodGet || sent.URL != usageURL || len(sent.Body) != 0 || sent.Headers.Get("Cookie") != "" {
		t.Fatalf("request: %s %s body=%d cookie=%q", sent.Method, sent.URL, len(sent.Body), sent.Headers.Get("Cookie"))
	}
	if sent.Headers.Get("Authorization") != "Bearer tok" || sent.Headers.Get("Chatgpt-Account-Id") != "acct" {
		t.Fatalf("credential headers: %v", sent.Headers)
	}
	hostHTTPDoFunc = func(hostHTTPRequest) (hostHTTPResponse, error) { return hostHTTPResponse{StatusCode: 401}, nil }
	if _, err := refreshQuota("idx"); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("a refused refresh says so: %v", err)
	}
}
