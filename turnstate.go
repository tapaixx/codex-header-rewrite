package main

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"time"
)

// X-Codex-Turn-State is an opaque blob the upstream returns in a response and
// the client echoes on the next request of the same turn chain. The blob is
// tied to one account's outbound identity, so echoing a blob from account A
// on a request that now goes out under account B is a contradiction the real
// Codex client never produces: it only happens behind a proxy that switched
// credentials between turns.
//
// This file does two separate things with that header:
//
//   - Provenance: remember which credential returned each blob, then flag a later
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
	teamStateMaxChars    = 332
	proStateMaxChars     = 292
)

// turnStateInfo is the non-secret envelope of one observed blob. Digest is a
// short hash used to correlate an echo with the response that returned it without
// comparing blobs by value.
// Chars is the length of the blob as it travelled on the wire and Bytes the
// length after decoding. Both are kept: a blob whose character count does not
// match its byte count the way base64 requires is truncated or re-encoded, and
// that is visible only when the two numbers are shown side by side.
type turnStateInfo struct {
	Digest      string    `json:"digest"`
	Chars       int       `json:"chars,omitempty"`
	Bytes       int       `json:"bytes,omitempty"`
	Version     int       `json:"version,omitempty"`
	IssuedAt    time.Time `json:"issued_at,omitempty"`
	FernetLike  bool      `json:"fernet_like"`
	Decodable   bool      `json:"decodable"`
	PlanType    string    `json:"plan_type,omitempty"`
	MaxChars    int       `json:"max_chars,omitempty"`
	NonDegraded *bool     `json:"non_degraded,omitempty"`
	Pooled      bool      `json:"pooled,omitempty"`
}

func normalizePlanType(plan string) string {
	switch strings.ToLower(strings.TrimSpace(plan)) {
	case "team":
		return "team"
	case "pro":
		return "pro"
	default:
		return ""
	}
}

// credentialPlanType reads only the plan claim from the credential's ID token.
// No token or identity value is retained.
func credentialPlanType(document json.RawMessage) string {
	return credentialPlanFromJSON(document, 0)
}

func credentialPlanFromJSON(raw json.RawMessage, depth int) string {
	if depth > 6 || len(raw) == 0 {
		return ""
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil {
		return ""
	}
	for _, name := range []string{"chatgpt_plan_type", "plan_type", "plan"} {
		if field := rawFieldFold(object, name); len(field) > 0 {
			var plan string
			if json.Unmarshal(field, &plan) == nil {
				if normalized := normalizePlanType(plan); normalized != "" {
					return normalized
				}
			}
		}
	}
	for _, name := range []string{"id_token", "idToken", "IDToken"} {
		field := rawFieldFold(object, name)
		if len(field) == 0 {
			continue
		}
		var token string
		if json.Unmarshal(field, &token) == nil {
			if plan := credentialPlanFromJWT(token, depth+1); plan != "" {
				return plan
			}
		} else if plan := credentialPlanFromJSON(field, depth+1); plan != "" {
			return plan
		}
	}
	for _, name := range []string{"https://api.openai.com/auth", "tokens", "oauth", "credential", "auth", "data"} {
		if plan := credentialPlanFromJSON(rawFieldFold(object, name), depth+1); plan != "" {
			return plan
		}
	}
	return ""
}

func credentialPlanFromJWT(token string, depth int) string {
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) < 2 {
		return ""
	}
	payload, err := decodeBase64Flexible(parts[1])
	if err != nil {
		return ""
	}
	return credentialPlanFromJSON(payload, depth)
}

func rawFieldFold(object map[string]json.RawMessage, name string) json.RawMessage {
	for key, value := range object {
		if strings.EqualFold(key, name) {
			return value
		}
	}
	return nil
}

// nonDegradedTurnState applies the observed inclusive wire-text thresholds.
// An unknown plan cannot prove that a state is non-degraded, so it never enters
// the pool.
func nonDegradedTurnState(blob, plan string) (nonDegraded bool, maxChars int, knownPlan bool) {
	switch normalizePlanType(plan) {
	case "team":
		maxChars = teamStateMaxChars
	case "pro":
		maxChars = proStateMaxChars
	default:
		return false, 0, false
	}
	return len(blob) <= maxChars, maxChars, true
}

func classifyTurnState(info turnStateInfo, blob, plan string) turnStateInfo {
	info.PlanType = normalizePlanType(plan)
	nonDegraded, maxChars, knownPlan := nonDegradedTurnState(blob, plan)
	info.MaxChars = maxChars
	if knownPlan {
		info.NonDegraded = &nonDegraded
	}
	return info
}

// A pooled blob is only reusable by the same credential, on the same model,
// and while it is still fresh. Those three conditions are what make an echo
// legitimate; failing any of them makes the echo a contradiction the upstream
// can see.
//
// The freshness window is an observed rule of thumb rather than a documented
// guarantee, so an expired echo is reported and never stripped: guessing the
// window wrong would break a turn chain that would otherwise have worked.
const turnStateReuseWindow = time.Hour

// turnStateOrigin records the credential and model that returned a blob.
type turnStateOrigin struct {
	blob      string
	digest    string
	authIndex string
	label     string
	model     string
	planType  string
	chars     int
	maxChars  int
	mintedAt  time.Time
	seen      time.Time
}

// persistedTurnState is the durable form of the newest qualified state for a
// credential and model. The original blob is intentionally stored because the
// pool must be usable after a host restart, not merely identify an old value.
type persistedTurnState struct {
	State     string    `json:"state"`
	Digest    string    `json:"digest"`
	AuthIndex string    `json:"auth_index"`
	Label     string    `json:"label,omitempty"`
	Model     string    `json:"model,omitempty"`
	PlanType  string    `json:"plan_type"`
	Chars     int       `json:"chars"`
	MaxChars  int       `json:"max_chars"`
	MintedAt  time.Time `json:"minted_at"`
	SeenAt    time.Time `json:"seen_at"`
}

func (origin turnStateOrigin) persisted() persistedTurnState {
	return persistedTurnState{
		State: origin.blob, Digest: origin.digest, AuthIndex: origin.authIndex,
		Label: origin.label, Model: origin.model, PlanType: origin.planType,
		Chars: origin.chars, MaxChars: origin.maxChars,
		MintedAt: origin.mintedAt, SeenAt: origin.seen,
	}
}

func restoreTurnState(record persistedTurnState) (turnStateOrigin, bool) {
	blob := strings.TrimSpace(record.State)
	nonDegraded, maxChars, knownPlan := nonDegradedTurnState(blob, record.PlanType)
	if blob == "" || record.AuthIndex == "" || !knownPlan || !nonDegraded {
		return turnStateOrigin{}, false
	}
	seen := record.SeenAt
	if seen.IsZero() {
		seen = time.Now().UTC()
	}
	minted := record.MintedAt
	if minted.IsZero() {
		minted = decodeTurnState(blob).IssuedAt
	}
	if minted.IsZero() {
		minted = seen
	}
	return turnStateOrigin{
		blob: blob, digest: turnStateDigest(blob), authIndex: record.AuthIndex,
		label: record.Label, model: strings.TrimSpace(record.Model),
		planType: normalizePlanType(record.PlanType), chars: len(blob),
		maxChars: maxChars, mintedAt: minted, seen: seen,
	}, true
}

// latestKey indexes the most recent blob per credential and model. A newer
// state for the same pair replaces the older one, because the older one is what
// the upstream has already moved past.
func turnStateLatestKey(authIndex, model string) string {
	return authIndex + "\x00" + strings.TrimSpace(model)
}

// headerValueFold reads a header without trusting the map's key casing.
// http.Header.Get canonicalizes the key it looks up but not the keys already in
// the map, and canonicalization is not identity for every name: X-OpenAI-... is
// stored canonically as X-Openai-..., so a map built with the literal spelling
// is invisible to Get. Any header map that did not come from Set is suspect.
func headerValueFold(h http.Header, name string) string {
	if h == nil {
		return ""
	}
	if value := strings.TrimSpace(h.Get(name)); value != "" {
		return value
	}
	for key, values := range h {
		if strings.EqualFold(key, name) && len(values) > 0 {
			return strings.TrimSpace(values[0])
		}
	}
	return ""
}

func headerTurnState(h http.Header) string {
	return headerValueFold(h, turnStateHeader)
}

// clientSessionID matches the header the Codex client uses to identify a
// conversation, in both spellings seen in the wild.
func clientSessionID(h http.Header) string {
	if h == nil {
		return ""
	}
	for _, name := range []string{"Session-Id", "Session_id"} {
		if value := headerValueFold(h, name); value != "" {
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
	info := turnStateInfo{Digest: turnStateDigest(blob), Chars: len(blob)}
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

// noteTurnStateMintLocked mints a qualified observed blob into the state pool.
// Callers hold state.mu.
func noteTurnStateMintLocked(blob, authIndex, label, model, plan string) bool {
	digest := turnStateDigest(blob)
	if blob == "" || digest == "" || authIndex == "" {
		return false
	}
	nonDegraded, maxChars, knownPlan := nonDegradedTurnState(blob, plan)
	if !knownPlan || !nonDegraded {
		return false
	}
	now := time.Now().UTC()
	info := decodeTurnState(blob)
	minted := info.IssuedAt
	if minted.IsZero() {
		minted = now
	}
	origin := turnStateOrigin{blob: blob, digest: digest, authIndex: authIndex, label: label, model: strings.TrimSpace(model), planType: normalizePlanType(plan), chars: len(blob), maxChars: maxChars, mintedAt: minted, seen: now}
	state.turnStates[digest] = origin
	if state.turnStateLatest == nil {
		state.turnStateLatest = map[string]turnStateOrigin{}
	}
	key := turnStateLatestKey(origin.authIndex, origin.model)
	pooled := false
	if previous, ok := state.turnStateLatest[key]; !ok || !previous.mintedAt.After(origin.mintedAt) {
		if state.store != nil {
			if err := state.store.SaveTurnState(origin.persisted()); err != nil {
				delete(state.turnStates, digest)
				return false
			}
		}
		state.turnStateLatest[key] = origin
		pooled = true
	}
	state.turnStateWrites++
	if state.turnStateWrites%turnStateSweepPeriod == 0 || len(state.turnStates) > turnStateMaxEntries {
		sweepTurnStatesLocked(now)
	}
	return pooled
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

// lookupTurnStateOriginLocked returns the credential that supplied this blob, if
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

// turnStateEcho describes the blob a request echoed, and whether it originated
// under a different credential than the one now serving the request.
type turnStateEcho struct {
	info         turnStateInfo
	known        bool
	originIndex  string
	originLabel  string
	originModel  string
	crossAccount bool
	crossModel   bool
	expired      bool
	ageSeconds   int64
}

// reusable reports whether the echo is one the upstream could legitimately
// accept. Unknown provenance is not reusable-or-not, it is unjudged.
func (e turnStateEcho) unusable() bool { return e.known && (e.crossAccount || e.crossModel) }

func evaluateTurnStateEchoLocked(headers http.Header, authIndex, model string) (turnStateEcho, bool) {
	blob := headerTurnState(headers)
	if blob == "" {
		return turnStateEcho{}, false
	}
	echo := turnStateEcho{info: decodeTurnState(blob)}
	// Age comes from the envelope itself, so it can be judged even when this
	// process never saw the blob being returned.
	if !echo.info.IssuedAt.IsZero() {
		age := time.Since(echo.info.IssuedAt)
		echo.ageSeconds = int64(age.Seconds())
		echo.expired = age > turnStateReuseWindow
	}
	origin, known := lookupTurnStateOriginLocked(blob)
	if !known {
		return echo, true
	}
	echo.known = true
	echo.originIndex = origin.authIndex
	echo.originLabel = origin.label
	echo.originModel = origin.model
	echo.crossAccount = origin.authIndex != authIndex
	// Same credential, different model is still an echo the upstream cannot
	// use: the blob belongs to the other model's turn chain.
	echo.crossModel = !echo.crossAccount && origin.model != "" && strings.TrimSpace(model) != "" &&
		!strings.EqualFold(origin.model, strings.TrimSpace(model))
	return echo, true
}

// recentTurnStatesLocked lists the newest blob per credential and model, newest
// first. Callers hold state.mu.
func recentTurnStatesLocked() []turnStateOrigin {
	out := make([]turnStateOrigin, 0, len(state.turnStateLatest))
	for _, origin := range state.turnStateLatest {
		out = append(out, origin)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].mintedAt.After(out[j].mintedAt) })
	return out
}
