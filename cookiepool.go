package main

import (
	"encoding/base64"
	"encoding/json"
	"math/rand/v2"
	"sort"
	"strings"
	"time"
)

// The non-degraded cookie pool keeps, per credential, the newest session
// cookie that went with a turn judged non-degraded, keyed by the backend its
// __oailb load-balancer cookie pins the session to. A newer cookie for the
// same backend replaces the older one. A cookie without __oailb names no
// backend and is not kept.
//
// It is fed only by positive verdicts: a probe whose multi-check came back
// with no new state, and a live turn that carried an injected state and drew
// none back. The probe's cookie_pool mode draws from it.

type cookiePoolEntry struct {
	AuthIndex string    `json:"auth_index"`
	Host      string    `json:"host"`
	Cookie    string    `json:"cookie"`
	IssuedAt  time.Time `json:"issued_at,omitempty"`
	ExpiresAt time.Time `json:"expires_at,omitempty"`
	// Source is "probe" or "live": which verdict vouched for it.
	Source string `json:"source"`
	// InvalidatedAt is set when a turn that carried this cookie was judged
	// degraded; the entry stays listed but is never drawn again. A new
	// non-degraded cookie for the same backend replaces it outright.
	InvalidatedAt time.Time `json:"invalidated_at,omitempty"`
	// InvalidatedBy is "live" or "probe": which verdict took it out.
	InvalidatedBy string    `json:"invalidated_by,omitempty"`
	Model         string    `json:"model,omitempty"`
	Digest        string    `json:"digest,omitempty"`
	SavedAt       time.Time `json:"saved_at"`
}

func cookiePoolKey(authIndex, host string) string { return authIndex + "\x00" + host }

// usable is whether the entry may still be sent: its __oailb has not expired.
// An entry whose token names no expiry is taken at its word.
func (e cookiePoolEntry) usable(now time.Time) bool {
	return e.InvalidatedAt.IsZero() && (e.ExpiresAt.IsZero() || now.Before(e.ExpiresAt))
}

// oailbRoute reads the backend and validity out of the __oailb crumb. The
// token is only decoded, never verified: it is read to label the cookie, not
// to trust it.
func oailbRoute(cookie string) (host string, issued, expires time.Time, ok bool) {
	for _, crumb := range strings.Split(cookie, ";") {
		name, value, found := cutCookieCrumb(crumb)
		if !found || name != "__oailb" {
			continue
		}
		parts := strings.Split(value, ".")
		if len(parts) != 3 {
			return "", time.Time{}, time.Time{}, false
		}
		raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
		if err != nil {
			return "", time.Time{}, time.Time{}, false
		}
		var claims struct {
			Host string  `json:"host"`
			Iat  float64 `json:"iat"`
			Exp  float64 `json:"exp"`
		}
		if json.Unmarshal(raw, &claims) != nil || strings.TrimSpace(claims.Host) == "" {
			return "", time.Time{}, time.Time{}, false
		}
		if claims.Iat > 0 {
			issued = time.Unix(int64(claims.Iat), 0).UTC()
		}
		if claims.Exp > 0 {
			expires = time.Unix(int64(claims.Exp), 0).UTC()
		}
		return strings.TrimSpace(claims.Host), issued, expires, true
	}
	return "", time.Time{}, time.Time{}, false
}

// noteCookiePoolLocked keeps a cookie a non-degraded verdict vouched for.
// Callers hold state.mu.
func noteCookiePoolLocked(authIndex, cookie, source, model, digest string) bool {
	if state.store == nil || authIndex == "" || strings.TrimSpace(cookie) == "" {
		return false
	}
	host, issued, expires, ok := oailbRoute(cookie)
	if !ok {
		return false
	}
	entry := cookiePoolEntry{AuthIndex: authIndex, Host: host, Cookie: strings.TrimSpace(cookie),
		IssuedAt: issued, ExpiresAt: expires, Source: source, Model: model, Digest: digest, SavedAt: time.Now().UTC()}
	return state.store.SaveCookiePoolEntry(entry) == nil
}

// cookiePoolFor lists a credential's entries, soonest-expiring last.
func cookiePoolFor(authIndex string) ([]cookiePoolEntry, error) {
	state.mu.Lock()
	store := state.store
	state.mu.Unlock()
	if store == nil {
		return nil, nil
	}
	all, err := store.ListCookiePool()
	if err != nil {
		return nil, err
	}
	out := make([]cookiePoolEntry, 0, len(all))
	for _, entry := range all {
		if entry.AuthIndex == authIndex {
			out = append(out, entry)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SavedAt.After(out[j].SavedAt) })
	return out, nil
}

// pickPoolCookie draws one unexpired entry at random. None usable means none.
func pickPoolCookie(authIndex string, now time.Time) (cookiePoolEntry, bool) {
	entries, err := cookiePoolFor(authIndex)
	if err != nil {
		return cookiePoolEntry{}, false
	}
	usable := entries[:0]
	for _, entry := range entries {
		if entry.usable(now) {
			usable = append(usable, entry)
		}
	}
	if len(usable) == 0 {
		return cookiePoolEntry{}, false
	}
	return usable[rand.IntN(len(usable))], true
}

// invalidateCookiePoolLocked marks the entry for the backend this cookie is
// pinned to as invalid: a turn that carried it was judged degraded. It reports
// the backend, or "" when the cookie names none or the pool has no entry for
// it. Callers hold state.mu.
func invalidateCookiePoolLocked(authIndex, cookie, by string) string {
	if state.store == nil || authIndex == "" {
		return ""
	}
	host, _, _, ok := oailbRoute(cookie)
	if !ok {
		return ""
	}
	all, err := state.store.ListCookiePool()
	if err != nil {
		return ""
	}
	for _, entry := range all {
		if entry.AuthIndex != authIndex || entry.Host != host || !entry.InvalidatedAt.IsZero() {
			continue
		}
		entry.InvalidatedAt, entry.InvalidatedBy = time.Now().UTC(), by
		if state.store.SaveCookiePoolEntry(entry) != nil {
			return ""
		}
		return host
	}
	return ""
}
