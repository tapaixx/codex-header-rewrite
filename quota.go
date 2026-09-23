package main

import (
	"encoding/json"
	"errors"
	"fmt"
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
	quota, ok := quotaFromHeaders(headers, time.Now())
	if !ok {
		return
	}
	storeQuotaLocked(authIndex, quota)
}

func storeQuotaLocked(authIndex string, quota credentialQuota) {
	if state.store == nil || authIndex == "" {
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

// usageURL is the endpoint Codex clients read their limits from. It is a read:
// it returns the allowance without spending any of it, which is what lets the
// panel's refresh ask the upstream rather than wait for the next response.
const usageURL = "https://chatgpt.com/backend-api/wham/usage"

type usageWindow struct {
	UsedPercent        float64 `json:"used_percent"`
	LimitWindowSeconds int     `json:"limit_window_seconds"`
	ResetAfterSeconds  int64   `json:"reset_after_seconds"`
	ResetAt            int64   `json:"reset_at"`
}

// quotaFromUsage reads the usage payload into the same reading the response
// headers give. Only the rate-limit windows are read; identity fields in the
// payload are not kept.
func quotaFromUsage(body []byte, now time.Time) (credentialQuota, bool) {
	var payload struct {
		RateLimit *struct {
			Primary   *usageWindow `json:"primary_window"`
			Secondary *usageWindow `json:"secondary_window"`
		} `json:"rate_limit"`
	}
	if json.Unmarshal(body, &payload) != nil || payload.RateLimit == nil || payload.RateLimit.Primary == nil {
		return credentialQuota{}, false
	}
	reset := func(w *usageWindow) time.Time {
		if w.ResetAt > 0 {
			return time.Unix(w.ResetAt, 0).UTC()
		}
		if w.ResetAfterSeconds > 0 {
			return now.UTC().Add(time.Duration(w.ResetAfterSeconds) * time.Second)
		}
		return time.Time{}
	}
	primary := payload.RateLimit.Primary
	quota := credentialQuota{
		PrimaryUsedPercent: primary.UsedPercent, PrimaryResetAt: reset(primary),
		PrimaryWindowMinutes: primary.LimitWindowSeconds / 60, ObservedAt: now.UTC(),
	}
	if secondary := payload.RateLimit.Secondary; secondary != nil {
		quota.SecondaryUsedPercent, quota.SecondaryResetAt = secondary.UsedPercent, reset(secondary)
		quota.SecondaryWindowMinutes = secondary.LimitWindowSeconds / 60
	}
	return quota, true
}

// refreshQuota asks the upstream for the credential's allowance through the
// host's proxy-aware client, keeps the answer as the newest reading, and
// returns it. The credential is read for this one call and nothing of it is
// kept, logged, or returned.
func refreshQuota(authIndex string) (credentialQuota, error) {
	document, err := hostAuthGetFunc(authIndex)
	if err != nil {
		return credentialQuota{}, errors.New("credential is not readable through the host")
	}
	material := parseTestAuthMaterial(document)
	if material.accessToken == "" {
		return credentialQuota{}, errors.New("credential has no usable access token")
	}
	headers := retryHeaders(material, "")
	headers.Del("Content-Type")
	headers.Set("Accept", "application/json")
	response, err := hostHTTPDoFunc(hostHTTPRequest{Method: http.MethodGet, URL: usageURL, Headers: headers})
	if err != nil {
		return credentialQuota{}, err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return credentialQuota{}, fmt.Errorf("usage endpoint returned HTTP %d", response.StatusCode)
	}
	quota, ok := quotaFromUsage(response.Body, time.Now())
	if !ok {
		return credentialQuota{}, errors.New("usage endpoint returned no rate limits")
	}
	state.mu.Lock()
	storeQuotaLocked(authIndex, quota)
	state.mu.Unlock()
	return quota, nil
}
