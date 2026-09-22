package main

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

// quotaFromHeaders reads the allowance the upstream reports on every response.
// Primary is the five-hour window, secondary the weekly one; a reset is taken
// from the absolute Reset-At when present and from Reset-After-Seconds
// otherwise. Without a primary percentage there is no quota to read.
func quotaFromHeaders(h http.Header, now time.Time) (credentialQuota, bool) {
	primary, ok := headerFloat(h, "X-Codex-Primary-Used-Percent")
	if !ok {
		return credentialQuota{}, false
	}
	quota := credentialQuota{PrimaryUsedPercent: primary, ObservedAt: now.UTC()}
	quota.SecondaryUsedPercent, _ = headerFloat(h, "X-Codex-Secondary-Used-Percent")
	quota.PrimaryWindowMinutes = headerInt(h, "X-Codex-Primary-Window-Minutes")
	quota.SecondaryWindowMinutes = headerInt(h, "X-Codex-Secondary-Window-Minutes")
	quota.PrimaryResetAt = headerReset(h, "X-Codex-Primary-Reset-At", "X-Codex-Primary-Reset-After-Seconds", now)
	quota.SecondaryResetAt = headerReset(h, "X-Codex-Secondary-Reset-At", "X-Codex-Secondary-Reset-After-Seconds", now)
	return quota, true
}

func headerFloat(h http.Header, name string) (float64, bool) {
	value, err := strconv.ParseFloat(strings.TrimSpace(headerValueFold(h, name)), 64)
	if err != nil {
		return 0, false
	}
	return value, true
}

func headerInt(h http.Header, name string) int {
	n, _ := strconv.Atoi(strings.TrimSpace(headerValueFold(h, name)))
	return n
}

func headerReset(h http.Header, atName, afterName string, now time.Time) time.Time {
	if unix, err := strconv.ParseInt(strings.TrimSpace(headerValueFold(h, atName)), 10, 64); err == nil && unix > 0 {
		return time.Unix(unix, 0).UTC()
	}
	if after, err := strconv.ParseInt(strings.TrimSpace(headerValueFold(h, afterName)), 10, 64); err == nil && after > 0 {
		return now.UTC().Add(time.Duration(after) * time.Second)
	}
	return time.Time{}
}

// noteQuotaLocked keeps the newest reading on the credential's direct jar.
// The caller holds state.mu.
func noteQuotaLocked(authIndex string, headers http.Header) {
	if state.store == nil || authIndex == "" {
		return
	}
	quota, ok := quotaFromHeaders(headers, time.Now())
	if !ok {
		return
	}
	record, _, _ := state.store.Session(authIndex, "")
	record.AuthIndex, record.Egress = authIndex, ""
	record.Quota = &quota
	_ = state.store.SaveSession(record)
}

func noteQuota(authIndex string, headers http.Header) {
	state.mu.Lock()
	defer state.mu.Unlock()
	noteQuotaLocked(authIndex, headers)
}

// credentialQuotaFor reads the newest reading back for the panel.
func credentialQuotaFor(authIndex string) (credentialQuota, bool) {
	state.mu.Lock()
	store := state.store
	state.mu.Unlock()
	if store == nil {
		return credentialQuota{}, false
	}
	record, found, err := store.Session(authIndex, "")
	if err != nil || !found || record.Quota == nil {
		return credentialQuota{}, false
	}
	return *record.Quota, true
}
