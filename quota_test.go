package main

import (
	"net/http"
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
