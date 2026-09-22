package main

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
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
	// Every plan that is not a team plan shares the shorter personal limit.
	personalStateMaxChars = 292
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

// normalizePlanType keeps whatever plan the credential claims, lowercased.
// The threshold split is team vs. everything else, so the set of personal plan
// names does not have to be enumerated here: an unrecognised name is still a
// plan, and only a missing claim is unknown.
func normalizePlanType(plan string) string {
	return strings.ToLower(strings.TrimSpace(plan))
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
	// The id_token is authoritative; the access token carries the same auth
	// claim and covers credentials whose id_token is absent or stripped.
	for _, name := range []string{"id_token", "idToken", "IDToken", "access_token", "accessToken"} {
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

// nonDegradedTurnState applies the observed inclusive wire-text thresholds:
// a team plan may run to 332 characters, every personal (non-team) plan to 292.
// A credential that claims no plan at all cannot prove that a state is
// non-degraded, so it never enters the pool -- guessing the shorter limit would
// mark a valid team state as degraded, and guessing the longer one would pool a
// degraded personal state.
func nonDegradedTurnState(blob, plan string) (nonDegraded bool, maxChars int, knownPlan bool) {
	switch normalizePlanType(plan) {
	case "":
		return false, 0, false
	case "team":
		maxChars = teamStateMaxChars
	default:
		maxChars = personalStateMaxChars
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
// How long "fresh" lasts is an observed rule of thumb, not something the
// upstream documents, and it differs between accounts -- so it is a number the
// operator maintains per credential rather than a constant compiled in. An
// expired state is still reported and never stripped: guessing the window
// wrong would break a turn chain that would otherwise have worked. What the
// window does decide is when a still-degraded response is taken as proof that
// the injected state has stopped carrying the chain.
const (
	defaultStateTTLSeconds = 200
	minStateTTLSeconds     = 5
	maxStateTTLSeconds     = 7200
)

// turnStateReuseWindowLocked is the freshness window for one credential.
// A rule that has never been saved since the field existed carries zero, which
// reads as the default. Callers hold state.mu.
func turnStateReuseWindowLocked(authIndex string) time.Duration {
	if rule, ok := state.rules[authIndex]; ok && rule.StateTTLSeconds > 0 {
		return time.Duration(rule.StateTTLSeconds) * time.Second
	}
	return defaultStateTTLSeconds * time.Second
}

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

// headerValuesFold returns every value stored under a header name, matching the
// name case-insensitively.
//
// Not trusting the map's key casing is the whole point. http.Header.Get and
// Values canonicalize the key they look up but not the keys already in the
// map, and the ABI builds header maps straight from JSON -- whatever spelling
// the host serialised is what sits in the map, which over HTTP/2 is lowercase.
// Canonicalization is not identity either: X-OpenAI-... is stored canonically
// as X-Openai-..., so a map built with the literal spelling is invisible to
// Get.
//
// Because those keys are never normalised, one map can hold the same header
// under two spellings. Returning the first key that matched would silently
// drop the other, and map iteration is unordered, so it would not even drop
// the same one twice. Every match is collected, in sorted key order so the
// result does not depend on iteration.
func headerValuesFold(h http.Header, name string) []string {
	if h == nil {
		return nil
	}
	var matched []string
	for key := range h {
		if strings.EqualFold(key, name) {
			matched = append(matched, key)
		}
	}
	switch len(matched) {
	case 0:
		return nil
	case 1:
		return h[matched[0]]
	}
	sort.Strings(matched)
	out := make([]string, 0, len(matched)+1)
	for _, key := range matched {
		out = append(out, h[key]...)
	}
	return out
}

// headerValueFold reads the first non-empty value of a header.
func headerValueFold(h http.Header, name string) string {
	for _, value := range headerValuesFold(h, name) {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func headerTurnState(h http.Header) string {
	return headerValueFold(h, turnStateHeader)
}

// joinCookieHeader returns the request's cookies as a single header value.
// HTTP/2 may split Cookie into several crumbs, and the wire form joins them
// with "; ", so a request that arrives split is put back together rather than
// half-copied.
func joinCookieHeader(h http.Header) string {
	parts := make([]string, 0, 2)
	for _, value := range headerValuesFold(h, "Cookie") {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			parts = append(parts, trimmed)
		}
	}
	return strings.Join(parts, "; ")
}

// applySetCookies folds a response's Set-Cookie headers into the cookie the
// retry will send. The upstream can rotate or issue a session cookie on the
// very response being withheld, so sending back only what the request carried
// would present a session the upstream has already moved past.
//
// Only the name=value crumb is taken. Path, Domain, Expires and the flags say
// how a browser should store the cookie, not what goes back on the wire. A
// crumb the response clears -- empty value, or max-age at or below zero -- is
// dropped rather than echoed back empty. Order follows the request, with
// anything newly issued appended.
func applySetCookies(cookie string, responseHeaders http.Header) string {
	assignments := headerValuesFold(responseHeaders, "Set-Cookie")
	if len(assignments) == 0 {
		return cookie
	}
	order := make([]string, 0, 8)
	placed := make(map[string]bool, 8)
	values := make(map[string]string, 8)
	assign := func(name, value string) {
		if !placed[name] {
			order = append(order, name)
			placed[name] = true
		}
		values[name] = value
	}
	for _, crumb := range strings.Split(cookie, ";") {
		if name, value, ok := cutCookieCrumb(crumb); ok {
			assign(name, value)
		}
	}
	for _, assignment := range assignments {
		name, value, cleared := parseSetCookie(assignment)
		if name == "" {
			continue
		}
		if cleared {
			// Kept in the order, so re-issuing it later does not duplicate it.
			delete(values, name)
			placed[name] = true
			continue
		}
		assign(name, value)
	}
	parts := make([]string, 0, len(order))
	for _, name := range order {
		if value, ok := values[name]; ok {
			parts = append(parts, name+"="+value)
		}
	}
	return strings.Join(parts, "; ")
}

// parseSetCookie reads what a Set-Cookie header assigns, and whether it is
// clearing the cookie rather than setting one.
func parseSetCookie(assignment string) (name, value string, cleared bool) {
	segments := strings.Split(assignment, ";")
	name, value, ok := cutCookieCrumb(segments[0])
	if !ok {
		return "", "", false
	}
	if value == "" {
		return name, "", true
	}
	for _, attribute := range segments[1:] {
		key, raw, found := strings.Cut(attribute, "=")
		if !found || !strings.EqualFold(strings.TrimSpace(key), "max-age") {
			continue
		}
		if seconds, err := strconv.Atoi(strings.TrimSpace(raw)); err == nil && seconds <= 0 {
			return name, "", true
		}
	}
	return name, value, false
}

// cutCookieCrumb splits one name=value crumb. A cookie value may itself
// contain "=", so only the first one separates the pair.
func cutCookieCrumb(crumb string) (string, string, bool) {
	name, value, found := strings.Cut(strings.TrimSpace(crumb), "=")
	name = strings.TrimSpace(name)
	if !found || name == "" {
		return "", "", false
	}
	return name, strings.TrimSpace(value), true
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
		echo.expired = age > turnStateReuseWindowLocked(authIndex)
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

// turnStateForInjectionLocked returns the qualified state scoped to the exact
// selected credential and after-auth model. Callers hold state.mu.
// invalidateTurnStateLocked drops the pooled state for a credential and model
// while it is still the one identified by digest. A newer state pooled in the
// meantime is left alone: the evidence was about the injected one, not it.
// Provenance is kept, since the blob may still be echoed and must still be
// attributable.
func invalidateTurnStateLocked(authIndex, model, digest string) bool {
	key := turnStateLatestKey(authIndex, strings.TrimSpace(model))
	current, ok := state.turnStateLatest[key]
	if !ok || digest == "" || current.digest != digest {
		return false
	}
	if state.store != nil {
		if err := state.store.DeleteTurnState(authIndex, strings.TrimSpace(model)); err != nil {
			return false
		}
	}
	delete(state.turnStateLatest, key)
	return true
}

func turnStateForInjectionLocked(authIndex, model, plan string) (turnStateOrigin, bool) {
	origin, ok := state.turnStateLatest[turnStateLatestKey(authIndex, model)]
	if !ok || origin.blob == "" {
		return turnStateOrigin{}, false
	}
	qualified, _, knownPlan := nonDegradedTurnState(origin.blob, plan)
	if !knownPlan || !qualified {
		return turnStateOrigin{}, false
	}
	return origin, true
}
