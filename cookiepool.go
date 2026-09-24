package main

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/rand/v2"
	"net/http"
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

// poolCookieByHand adds a cookie to the pool on an operator's word: pasted,
// or the session of a recorded request -- what went out, updated by what the
// response set. It needs an __oailb to key on, like every other entry.
func poolCookieByHand(authIndex, id, pasted string) (cookiePoolEntry, error) {
	authIndex = strings.TrimSpace(authIndex)
	if authIndex == "" {
		return cookiePoolEntry{}, manualPoolFailure(http.StatusBadRequest, "auth_index is required")
	}
	state.mu.Lock()
	store := state.store
	state.mu.Unlock()
	if store == nil {
		return cookiePoolEntry{}, manualPoolFailure(http.StatusServiceUnavailable, "persistence is not initialized")
	}
	cookie, model, digest := strings.TrimSpace(pasted), "", ""
	if id = strings.TrimSpace(id); id != "" {
		record, found := findRecord(store, authIndex, id)
		if !found {
			return cookiePoolEntry{}, manualPoolFailure(http.StatusNotFound, "history record not found")
		}
		sent := joinCookieHeader(record.AfterHeaders)
		if sent == "" {
			sent = joinCookieHeader(record.BeforeHeaders)
		}
		cookie = applySetCookies(sent, record.ResponseHeaders)
		model = sentModel(record.Model, record.RequestedModel)
		if record.TurnStateMinted != nil {
			digest = record.TurnStateMinted.Digest
		}
	}
	if cookie == "" {
		return cookiePoolEntry{}, manualPoolFailure(http.StatusBadRequest, "no cookie to pool")
	}
	if _, _, _, ok := oailbRoute(cookie); !ok {
		return cookiePoolEntry{}, manualPoolFailure(http.StatusUnprocessableEntity, "the cookie carries no __oailb routing target")
	}
	state.mu.Lock()
	saved := noteCookiePoolLocked(authIndex, cookie, "manual", model, digest)
	state.mu.Unlock()
	if !saved {
		return cookiePoolEntry{}, errors.New("the cookie could not be saved to the pool")
	}
	host, _, _, _ := oailbRoute(cookie)
	entries, _ := cookiePoolFor(authIndex)
	for _, entry := range entries {
		if entry.Host == host {
			return entry, nil
		}
	}
	return cookiePoolEntry{}, errors.New("the cookie could not be read back")
}

// findRecord looks a record up in the request history, then the probe
// history; both are bounded, so a scan is the lookup.
func findRecord(store persistence, authIndex, id string) (historyRecord, bool) {
	if page, err := store.History(authIndex, 1, historyLimit); err == nil {
		for _, record := range page.Items {
			if record.ID == id {
				return record, true
			}
		}
	}
	if page, err := store.ProbeHistory(authIndex, 1, probeHistoryLimit); err == nil {
		for _, record := range page.Items {
			if record.ID == id {
				return record, true
			}
		}
	}
	return historyRecord{}, false
}

// useCookieForCredential writes a pool entry's cookie into the credential's
// own jar, the one the probe's credential mode presents. Live traffic keeps
// writing that jar, so the next live response replaces it again; the liveness
// clock is left alone.
func useCookieForCredential(authIndex, host string) (credentialSession, error) {
	authIndex, host = strings.TrimSpace(authIndex), strings.TrimSpace(host)
	if authIndex == "" || host == "" {
		return credentialSession{}, manualPoolFailure(http.StatusBadRequest, "auth_index and host are required")
	}
	entries, err := cookiePoolFor(authIndex)
	if err != nil {
		return credentialSession{}, err
	}
	for _, entry := range entries {
		if entry.Host != host {
			continue
		}
		state.mu.Lock()
		store := state.store
		state.mu.Unlock()
		if store == nil {
			return credentialSession{}, manualPoolFailure(http.StatusServiceUnavailable, "persistence is not initialized")
		}
		session, _, _ := store.Session(authIndex, "")
		session.AuthIndex, session.Egress = authIndex, ""
		session.Cookie, session.RefreshAt = entry.Cookie, time.Now().UTC()
		if err := store.SaveSession(session); err != nil {
			return credentialSession{}, err
		}
		return session, nil
	}
	return credentialSession{}, manualPoolFailure(http.StatusNotFound, "no cookie for that routing target")
}
