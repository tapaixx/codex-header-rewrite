package main

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"net/http"
	"strings"
	"time"
)

// X-Codex-Turn-State is an opaque blob the upstream mints in a response and the
// client echoes on the next request of the same turn chain. The blob is minted
// under one account's outbound identity, so echoing a blob minted by account A
// on a request that now goes out under account B is a contradiction the real
// Codex client never produces: it only happens behind a proxy that switched
// credentials between turns.
//
// This file does two separate things with that header:
//
//   - Provenance: remember which credential minted each blob, then flag a later
//     request that echoes it under a different credential. The rule can also
//     strip the echo, which is the only way to stop the contradiction reaching
//     the upstream.
//   - Envelope decoding: the blob is a Fernet token, so its version byte and
//     mint timestamp are readable without any key. Nothing inside the ciphertext
//     is read, and no key is held.
const (
	turnStateHeader = "X-Codex-Turn-State"
	// Fernet envelope: 1 version + 8 timestamp + 16 IV + ciphertext + 32 HMAC.
	fernetOverhead  = 57
	fernetBlockSize = 16
	// Provenance is a short-lived correlation aid, not a store. Entries expire
	// and the table is capped so a long-running host cannot grow it without end.
	turnStateTTL         = 2 * time.Hour
	turnStateMaxEntries  = 512
	turnStateSweepPeriod = 128
)

// turnStateInfo is the non-secret envelope of one observed blob. Digest is a
// short hash used to correlate an echo with the response that minted it without
// comparing blobs by value.
type turnStateInfo struct {
	Digest     string    `json:"digest"`
	Bytes      int       `json:"bytes,omitempty"`
	Version    int       `json:"version,omitempty"`
	IssuedAt   time.Time `json:"issued_at,omitempty"`
	FernetLike bool      `json:"fernet_like"`
	Decodable  bool      `json:"decodable"`
}

// turnStateOrigin records which credential minted a blob.
type turnStateOrigin struct {
	authIndex string
	label     string
	mintedAt  time.Time
	seen      time.Time
}

func headerTurnState(h http.Header) string {
	if h == nil {
		return ""
	}
	return strings.TrimSpace(h.Get(turnStateHeader))
}

// clientSessionID matches the header the Codex client uses to identify a
// conversation, in both spellings seen in the wild.
func clientSessionID(h http.Header) string {
	if h == nil {
		return ""
	}
	for _, name := range []string{"Session-Id", "Session_id"} {
		if value := strings.TrimSpace(h.Get(name)); value != "" {
			return value
		}
	}
	return ""
}

func turnStateDigest(blob string) string {
	sum := sha256.Sum256([]byte(blob))
	return hex.EncodeToString(sum[:])[:16]
}

// decodeTurnState reads the Fernet envelope: version byte, big-endian mint
// timestamp, and whether the remaining length is a whole number of cipher
// blocks. A blob that does not decode is still digested, because an
// undecodable value is itself worth showing rather than hiding.
func decodeTurnState(blob string) turnStateInfo {
	blob = strings.TrimSpace(blob)
	info := turnStateInfo{Digest: turnStateDigest(blob)}
	if blob == "" {
		return turnStateInfo{}
	}
	raw, err := decodeBase64Flexible(blob)
	if err != nil || len(raw) < 9 {
		return info
	}
	info.Decodable = true
	info.Bytes = len(raw)
	info.Version = int(raw[0])
	if seconds := binary.BigEndian.Uint64(raw[1:9]); seconds <= 1<<62 {
		info.IssuedAt = time.Unix(int64(seconds), 0).UTC()
	}
	info.FernetLike = len(raw) >= fernetOverhead && (len(raw)-fernetOverhead)%fernetBlockSize == 0
	return info
}

// decodeBase64Flexible accepts the URL-safe alphabet Fernet uses, with or
// without padding, and falls back to the standard alphabet.
func decodeBase64Flexible(value string) ([]byte, error) {
	for _, encoding := range []*base64.Encoding{
		base64.RawURLEncoding, base64.URLEncoding,
		base64.RawStdEncoding, base64.StdEncoding,
	} {
		if raw, err := encoding.DecodeString(strings.TrimRight(value, "=")); err == nil {
			return raw, nil
		}
	}
	return base64.URLEncoding.DecodeString(value)
}

// noteTurnStateMintLocked records that authIndex minted this blob. Callers hold
// state.mu.
func noteTurnStateMintLocked(blob, authIndex, label string) {
	digest := turnStateDigest(blob)
	if blob == "" || digest == "" || authIndex == "" {
		return
	}
	now := time.Now().UTC()
	info := decodeTurnState(blob)
	minted := info.IssuedAt
	if minted.IsZero() {
		minted = now
	}
	state.turnStates[digest] = turnStateOrigin{authIndex: authIndex, label: label, mintedAt: minted, seen: now}
	state.turnStateWrites++
	if state.turnStateWrites%turnStateSweepPeriod == 0 || len(state.turnStates) > turnStateMaxEntries {
		sweepTurnStatesLocked(now)
	}
}

// sweepTurnStatesLocked drops expired entries, then oldest-first until the table
// is back under its cap.
func sweepTurnStatesLocked(now time.Time) {
	oldestKey, oldestSeen := "", time.Time{}
	for digest, origin := range state.turnStates {
		if now.Sub(origin.seen) > turnStateTTL {
			delete(state.turnStates, digest)
			continue
		}
		if oldestSeen.IsZero() || origin.seen.Before(oldestSeen) {
			oldestKey, oldestSeen = digest, origin.seen
		}
	}
	for len(state.turnStates) > turnStateMaxEntries && oldestKey != "" {
		delete(state.turnStates, oldestKey)
		oldestKey, oldestSeen = "", time.Time{}
		for digest, origin := range state.turnStates {
			if oldestSeen.IsZero() || origin.seen.Before(oldestSeen) {
				oldestKey, oldestSeen = digest, origin.seen
			}
		}
	}
}

// lookupTurnStateOriginLocked returns the credential that minted this blob, if
// it is still remembered. Callers hold state.mu.
func lookupTurnStateOriginLocked(blob string) (turnStateOrigin, bool) {
	if blob == "" {
		return turnStateOrigin{}, false
	}
	origin, ok := state.turnStates[turnStateDigest(blob)]
	if !ok {
		return turnStateOrigin{}, false
	}
	if time.Since(origin.seen) > turnStateTTL {
		delete(state.turnStates, turnStateDigest(blob))
		return turnStateOrigin{}, false
	}
	return origin, true
}

// turnStateEcho describes the blob a request echoed, and whether it was minted
// under a different credential than the one now serving the request.
type turnStateEcho struct {
	info         turnStateInfo
	known        bool
	originIndex  string
	originLabel  string
	crossAccount bool
}

func evaluateTurnStateEchoLocked(headers http.Header, authIndex string) (turnStateEcho, bool) {
	blob := headerTurnState(headers)
	if blob == "" {
		return turnStateEcho{}, false
	}
	echo := turnStateEcho{info: decodeTurnState(blob)}
	origin, known := lookupTurnStateOriginLocked(blob)
	if !known {
		return echo, true
	}
	echo.known = true
	echo.originIndex = origin.authIndex
	echo.originLabel = origin.label
	echo.crossAccount = origin.authIndex != authIndex
	return echo, true
}
