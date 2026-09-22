package main

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

func credentialDocumentWithPlan(t *testing.T, plan string) json.RawMessage {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"https://api.openai.com/auth": map[string]any{"chatgpt_plan_type": plan},
	})
	if err != nil {
		t.Fatal(err)
	}
	token := "header." + base64.RawURLEncoding.EncodeToString(payload) + ".signature"
	document, err := json.Marshal(map[string]any{"id_token": token})
	if err != nil {
		t.Fatal(err)
	}
	return document
}

func TestCredentialPlanComesFromTheIDToken(t *testing.T) {
	for _, plan := range []string{"team", "pro"} {
		if got := credentialPlanType(credentialDocumentWithPlan(t, plan)); got != plan {
			t.Fatalf("plan=%q, want %q", got, plan)
		}
	}
}

func TestCredentialPlanSupportsObjectAndNestedTokenShapes(t *testing.T) {
	tests := map[string]json.RawMessage{
		"object id token": json.RawMessage(`{"id_token":{"https://api.openai.com/auth":{"chatgpt_plan_type":"team"}}}`),
		"nested JWT":      json.RawMessage(`{"tokens":{"idToken":"header.eyJodHRwczovL2FwaS5vcGVuYWkuY29tL2F1dGgiOnsiY2hhdGdwdF9wbGFuX3R5cGUiOiJwcm8ifX0.signature"}}`),
	}
	for name, document := range tests {
		want := "team"
		if name == "nested JWT" {
			want = "pro"
		}
		if got := credentialPlanType(document); got != want {
			t.Fatalf("%s: plan=%q, want %q", name, got, want)
		}
	}
}

// A credential whose id_token is absent still carries the auth claim in its
// access token; the id_token stays authoritative when both are present.
func TestCredentialPlanFallsBackToTheAccessToken(t *testing.T) {
	accessOnly := json.RawMessage(`{"tokens":{"access_token":"header.eyJodHRwczovL2FwaS5vcGVuYWkuY29tL2F1dGgiOnsiY2hhdGdwdF9wbGFuX3R5cGUiOiJwbHVzIn19.signature"}}`)
	if got := credentialPlanType(accessOnly); got != "plus" {
		t.Fatalf("access-token plan=%q, want %q", got, "plus")
	}
	both := json.RawMessage(`{"tokens":{` +
		`"id_token":"header.eyJodHRwczovL2FwaS5vcGVuYWkuY29tL2F1dGgiOnsiY2hhdGdwdF9wbGFuX3R5cGUiOiJ0ZWFtIn19.signature",` +
		`"access_token":"header.eyJodHRwczovL2FwaS5vcGVuYWkuY29tL2F1dGgiOnsiY2hhdGdwdF9wbGFuX3R5cGUiOiJwbHVzIn19.signature"}}`)
	if got := credentialPlanType(both); got != "team" {
		t.Fatalf("id_token should stay authoritative, got %q", got)
	}
}

func TestNonDegradedStateLimitsAreInclusivePerPlan(t *testing.T) {
	tests := []struct {
		plan  string
		chars int
		want  bool
		max   int
		known bool
	}{
		{plan: "team", chars: 332, want: true, max: 332, known: true},
		{plan: "team", chars: 333, want: false, max: 332, known: true},
		{plan: "Team", chars: 333, want: false, max: 332, known: true},
		{plan: "pro", chars: 292, want: true, max: 292, known: true},
		{plan: "pro", chars: 293, want: false, max: 292, known: true},
		// Every personal plan shares the shorter limit, including names the
		// plugin has never seen; a team state of the same length is still fine.
		{plan: "plus", chars: 292, want: true, max: 292, known: true},
		{plan: "plus", chars: 293, want: false, max: 292, known: true},
		{plan: "enterprise", chars: 300, want: false, max: 292, known: true},
		{plan: "team", chars: 300, want: true, max: 332, known: true},
		// Only a missing claim is unknown, and unknown never enters the pool.
		{plan: "", chars: 1, want: false, max: 0, known: false},
		{plan: "   ", chars: 1, want: false, max: 0, known: false},
	}
	for _, tt := range tests {
		got, max, known := nonDegradedTurnState(strings.Repeat("x", tt.chars), tt.plan)
		if got != tt.want || max != tt.max || known != tt.known {
			t.Fatalf("plan=%s chars=%d: got (%v,%d,%v), want (%v,%d,%v)", tt.plan, tt.chars, got, max, known, tt.want, tt.max, tt.known)
		}
	}
}

func TestOnlyNonDegradedStatesEnterThePool(t *testing.T) {
	resetTurnStates(t)
	teamGood := strings.Repeat("a", teamStateMaxChars)
	teamDegraded := strings.Repeat("b", teamStateMaxChars+1)
	state.mu.Lock()
	goodPooled := noteTurnStateMintLocked(teamGood, "idx-a", "team-a", "m", "team")
	degradedPooled := noteTurnStateMintLocked(teamDegraded, "idx-a", "team-a", "m", "team")
	unknownPooled := noteTurnStateMintLocked("x", "idx-b", "unknown", "m", "")
	recent := recentTurnStatesLocked()
	state.mu.Unlock()

	if !goodPooled || degradedPooled || unknownPooled {
		t.Fatalf("pooled good=%v degraded=%v unknown=%v", goodPooled, degradedPooled, unknownPooled)
	}
	if len(recent) != 1 || recent[0].digest != turnStateDigest(teamGood) {
		t.Fatalf("state pool=%#v", recent)
	}
}

// fernetToken builds an envelope shaped like the real blob: version byte,
// big-endian mint timestamp, IV, ciphertext blocks, HMAC.
func fernetToken(version byte, issued time.Time, ciphertextBlocks int) string {
	raw := make([]byte, 0, fernetOverhead+ciphertextBlocks*fernetBlockSize)
	raw = append(raw, version)
	stamp := make([]byte, 8)
	binary.BigEndian.PutUint64(stamp, uint64(issued.Unix()))
	raw = append(raw, stamp...)
	raw = append(raw, make([]byte, 16)...)
	raw = append(raw, make([]byte, ciphertextBlocks*fernetBlockSize)...)
	raw = append(raw, make([]byte, 32)...)
	return base64.URLEncoding.EncodeToString(raw)
}

func resetTurnStates(t *testing.T) {
	t.Helper()
	state.mu.Lock()
	state.turnStates = map[string]turnStateOrigin{}
	state.turnStateLatest = map[string]turnStateOrigin{}
	state.turnStateWrites = 0
	state.mu.Unlock()
	t.Cleanup(func() {
		state.mu.Lock()
		state.turnStates = map[string]turnStateOrigin{}
		state.turnStateLatest = map[string]turnStateOrigin{}
		state.mu.Unlock()
	})
}

func TestTurnStateEnvelopeIsReadWithoutAnyKey(t *testing.T) {
	issued := time.Unix(1789000000, 0).UTC()
	info := decodeTurnState(fernetToken(0x80, issued, 3))
	if !info.Decodable || !info.FernetLike {
		t.Fatalf("info=%#v", info)
	}
	if info.Version != 0x80 {
		t.Fatalf("version=%#x", info.Version)
	}
	if !info.IssuedAt.Equal(issued) {
		t.Fatalf("issued=%s want %s", info.IssuedAt, issued)
	}
	if info.Bytes != fernetOverhead+3*fernetBlockSize {
		t.Fatalf("bytes=%d", info.Bytes)
	}
	// Both lengths are reported: the wire length and the decoded length are
	// different facts, and a mismatch between them is the tell for a truncated
	// or re-encoded blob.
	token := fernetToken(0x80, issued, 3)
	if info.Chars != len(token) {
		t.Fatalf("chars=%d want %d", info.Chars, len(token))
	}
	if info.Chars <= info.Bytes {
		t.Fatalf("base64 text is longer than its bytes: chars=%d bytes=%d", info.Chars, info.Bytes)
	}
	if info.Digest == "" {
		t.Fatal("a digest is what lets an echo be matched to its mint")
	}
}

func TestTurnStateAcceptsUnpaddedAndStandardAlphabets(t *testing.T) {
	issued := time.Unix(1789000123, 0).UTC()
	padded := fernetToken(0x80, issued, 2)
	raw, err := base64.URLEncoding.DecodeString(padded)
	if err != nil {
		t.Fatal(err)
	}
	for name, encoded := range map[string]string{
		"raw-url": base64.RawURLEncoding.EncodeToString(raw),
		"std":     base64.StdEncoding.EncodeToString(raw),
		"padded":  padded,
	} {
		info := decodeTurnState(encoded)
		if !info.Decodable || !info.IssuedAt.Equal(issued) {
			t.Fatalf("%s: info=%#v", name, info)
		}
	}
}

func TestUndecodableBlobStillReportsADigest(t *testing.T) {
	info := decodeTurnState("not a token at all !!!")
	if info.Decodable || info.FernetLike {
		t.Fatalf("info=%#v", info)
	}
	if info.Digest == "" {
		t.Fatal("an unreadable blob still needs an identity for correlation")
	}
	if empty := decodeTurnState("   "); empty.Digest != "" {
		t.Fatalf("an absent blob is not a blob: %#v", empty)
	}
}

func TestWrongBlockSizeIsReadableButNotFernetShaped(t *testing.T) {
	raw := make([]byte, fernetOverhead+7)
	raw[0] = 0x80
	info := decodeTurnState(base64.URLEncoding.EncodeToString(raw))
	if !info.Decodable {
		t.Fatalf("info=%#v", info)
	}
	if info.FernetLike {
		t.Fatal("ciphertext that is not a whole number of blocks is not Fernet shaped")
	}
}

func TestEchoUnderAnotherCredentialIsFlaggedAndOwnCredentialIsNot(t *testing.T) {
	resetTurnStates(t)
	blob := fernetToken(0x80, time.Now(), 2)
	state.mu.Lock()
	noteTurnStateMintLocked(blob, "idx-a", "team-a", "gpt-5.6-luna", "team")
	sameEcho, echoed := evaluateTurnStateEchoLocked(http.Header{turnStateHeader: {blob}}, "idx-a", "gpt-5.6-luna")
	crossEcho, _ := evaluateTurnStateEchoLocked(http.Header{turnStateHeader: {blob}}, "idx-b", "gpt-5.6-luna")
	state.mu.Unlock()

	if !echoed || !sameEcho.known || sameEcho.crossAccount {
		t.Fatalf("same credential echo=%#v", sameEcho)
	}
	if !crossEcho.known || !crossEcho.crossAccount {
		t.Fatalf("cross credential echo=%#v", crossEcho)
	}
	if crossEcho.originIndex != "idx-a" || crossEcho.originLabel != "team-a" {
		t.Fatalf("origin not reported: %#v", crossEcho)
	}
}

func TestUnknownOriginIsNotTreatedAsAMatch(t *testing.T) {
	resetTurnStates(t)
	state.mu.Lock()
	echo, echoed := evaluateTurnStateEchoLocked(http.Header{turnStateHeader: {fernetToken(0x80, time.Now(), 1)}}, "idx-a", "gpt-5.6-luna")
	state.mu.Unlock()
	if !echoed {
		t.Fatal("the blob was echoed")
	}
	if echo.known || echo.crossAccount {
		t.Fatalf("an unremembered blob must stay unjudged: %#v", echo)
	}
}

func TestExpiredProvenanceIsForgotten(t *testing.T) {
	resetTurnStates(t)
	blob := fernetToken(0x80, time.Now(), 1)
	state.mu.Lock()
	noteTurnStateMintLocked(blob, "idx-a", "team-a", "gpt-5.6-luna", "team")
	origin := state.turnStates[turnStateDigest(blob)]
	origin.seen = time.Now().UTC().Add(-turnStateTTL - time.Minute)
	state.turnStates[turnStateDigest(blob)] = origin
	echo, _ := evaluateTurnStateEchoLocked(http.Header{turnStateHeader: {blob}}, "idx-b", "gpt-5.6-luna")
	state.mu.Unlock()
	if echo.known {
		t.Fatal("provenance past its TTL must not accuse a later credential")
	}
}

func TestProvenanceTableStaysBounded(t *testing.T) {
	resetTurnStates(t)
	state.mu.Lock()
	for i := 0; i < turnStateMaxEntries*2; i++ {
		noteTurnStateMintLocked(fernetToken(0x80, time.Now().Add(time.Duration(i)*time.Second), 1), "idx-a", "team-a", "gpt-5.6-luna", "team")
	}
	size := len(state.turnStates)
	state.mu.Unlock()
	if size > turnStateMaxEntries {
		t.Fatalf("table grew to %d entries", size)
	}
}

func TestSessionIDIsReadInBothSpellings(t *testing.T) {
	if got := clientSessionID(http.Header{"Session-Id": {"s-1"}}); got != "s-1" {
		t.Fatalf("got=%q", got)
	}
	if got := clientSessionID(http.Header{"Session_id": {"s-2"}}); got != "s-2" {
		t.Fatalf("got=%q", got)
	}
	if got := clientSessionID(http.Header{}); got != "" {
		t.Fatalf("got=%q", got)
	}
}

// Same credential, different model: the blob belongs to the other model's turn
// chain, so it is as unusable as a blob from another account.
func TestSameCredentialDifferentModelCannotReuse(t *testing.T) {
	resetTurnStates(t)
	blob := fernetToken(0x80, time.Now(), 2)
	state.mu.Lock()
	noteTurnStateMintLocked(blob, "idx-a", "team-a", "gpt-5.6-luna", "team")
	same, _ := evaluateTurnStateEchoLocked(http.Header{turnStateHeader: {blob}}, "idx-a", "gpt-5.6-luna")
	other, _ := evaluateTurnStateEchoLocked(http.Header{turnStateHeader: {blob}}, "idx-a", "gpt-5.1-codex")
	unknownModel, _ := evaluateTurnStateEchoLocked(http.Header{turnStateHeader: {blob}}, "idx-a", "")
	state.mu.Unlock()

	if same.crossModel || same.unusable() {
		t.Fatalf("same model must stay reusable: %#v", same)
	}
	if !other.crossModel || !other.unusable() {
		t.Fatalf("different model must be reported unusable: %#v", other)
	}
	if other.originModel != "gpt-5.6-luna" {
		t.Fatalf("minting model not reported: %#v", other)
	}
	// Without a model on either side there is nothing to compare, so the echo
	// stays unjudged on that axis rather than being accused.
	if unknownModel.crossModel {
		t.Fatalf("missing model must not be treated as a mismatch: %#v", unknownModel)
	}
}

func TestCrossAccountOutranksModelComparison(t *testing.T) {
	resetTurnStates(t)
	blob := fernetToken(0x80, time.Now(), 1)
	state.mu.Lock()
	noteTurnStateMintLocked(blob, "idx-a", "team-a", "gpt-5.6-luna", "team")
	echo, _ := evaluateTurnStateEchoLocked(http.Header{turnStateHeader: {blob}}, "idx-b", "gpt-5.1-codex")
	state.mu.Unlock()
	if !echo.crossAccount || echo.crossModel {
		t.Fatalf("a cross-account echo is reported as such, not as a model mismatch: %#v", echo)
	}
	if !echo.unusable() {
		t.Fatal("cross-account echo is unusable")
	}
}

func TestStaleBlobIsReportedExpiredButStaysUsableForStripping(t *testing.T) {
	resetTurnStates(t)
	// Ages are expressed against the window rather than in fixed minutes, so
	// changing the default does not silently move what the test asserts.
	const window = defaultStateTTLSeconds * time.Second
	fresh := fernetToken(0x80, time.Now().Add(-window/2), 1)
	stale := fernetToken(0x80, time.Now().Add(-2*window), 1)
	state.mu.Lock()
	noteTurnStateMintLocked(fresh, "idx-a", "team-a", "m", "team")
	noteTurnStateMintLocked(stale, "idx-a", "team-a", "m", "team")
	freshEcho, _ := evaluateTurnStateEchoLocked(http.Header{turnStateHeader: {fresh}}, "idx-a", "m")
	staleEcho, _ := evaluateTurnStateEchoLocked(http.Header{turnStateHeader: {stale}}, "idx-a", "m")
	state.mu.Unlock()

	if freshEcho.expired {
		t.Fatalf("a blob half the window old is fresh: %#v", freshEcho)
	}
	if want := int64((window / 2).Seconds()); freshEcho.ageSeconds < want-30 || freshEcho.ageSeconds > want+30 {
		t.Fatalf("age=%d want about %d", freshEcho.ageSeconds, want)
	}
	if !staleEcho.expired {
		t.Fatalf("a blob past the reuse window is expired: %#v", staleEcho)
	}
	// The window is observed rather than documented, so expiry alone never
	// strips: guessing it wrong would break a chain that still worked.
	if staleEcho.unusable() {
		t.Fatal("expiry is reported, not enforced")
	}
}

func TestExpiryIsJudgedWithoutAnyProvenance(t *testing.T) {
	resetTurnStates(t)
	stale := fernetToken(0x80, time.Now().Add(-2*defaultStateTTLSeconds*time.Second), 1)
	state.mu.Lock()
	echo, _ := evaluateTurnStateEchoLocked(http.Header{turnStateHeader: {stale}}, "idx-a", "m")
	state.mu.Unlock()
	if echo.known {
		t.Fatal("this blob was never minted here")
	}
	if !echo.expired {
		t.Fatal("age comes from the envelope, so it is readable without provenance")
	}
}

func TestNewerMintReplacesTheRecentEntryPerCredentialAndModel(t *testing.T) {
	resetTurnStates(t)
	older := fernetToken(0x80, time.Now().Add(-20*time.Minute), 1)
	newer := fernetToken(0x80, time.Now().Add(-1*time.Minute), 1)
	state.mu.Lock()
	noteTurnStateMintLocked(older, "idx-a", "team-a", "gpt-5.6-luna", "team")
	noteTurnStateMintLocked(newer, "idx-a", "team-a", "gpt-5.6-luna", "team")
	noteTurnStateMintLocked(older, "idx-a", "team-a", "gpt-5.1-codex", "team")
	noteTurnStateMintLocked(newer, "idx-b", "team-b", "gpt-5.6-luna", "team")
	recent := recentTurnStatesLocked()
	state.mu.Unlock()

	if len(recent) != 3 {
		t.Fatalf("one row per credential and model: %#v", recent)
	}
	for _, origin := range recent {
		if origin.authIndex == "idx-a" && origin.model == "gpt-5.6-luna" && origin.digest != turnStateDigest(newer) {
			t.Fatal("a newer mint replaces the older one for the same pair")
		}
	}
	if !recent[0].mintedAt.After(recent[len(recent)-1].mintedAt) {
		t.Fatalf("recent list is newest first: %#v", recent)
	}
}

func TestOlderMintDoesNotReplaceANewerOne(t *testing.T) {
	resetTurnStates(t)
	older := fernetToken(0x80, time.Now().Add(-30*time.Minute), 1)
	newer := fernetToken(0x80, time.Now(), 1)
	state.mu.Lock()
	newerPooled := noteTurnStateMintLocked(newer, "idx-a", "team-a", "m", "team")
	olderPooled := noteTurnStateMintLocked(older, "idx-a", "team-a", "m", "team")
	recent := recentTurnStatesLocked()
	state.mu.Unlock()
	if !newerPooled || olderPooled {
		t.Fatalf("newer pooled=%v, out-of-order older pooled=%v", newerPooled, olderPooled)
	}
	if len(recent) != 1 || recent[0].digest != turnStateDigest(newer) {
		t.Fatalf("out of order mints must not rewind the latest entry: %#v", recent)
	}
}

// The freshness window is the operator's number now, kept per credential. A
// rule saved before the field existed carries zero, which has to read as the
// default rather than as "everything is expired".
func TestReuseWindowComesFromTheRule(t *testing.T) {
	resetRules(t)
	resetTurnStates(t)
	state.mu.Lock()
	defer state.mu.Unlock()
	if got := turnStateReuseWindowLocked("idx-none"); got != defaultStateTTLSeconds*time.Second {
		t.Fatalf("a credential with no rule uses the default, got %s", got)
	}
	state.rules["idx-zero"] = headerRule{AuthIndex: "idx-zero", StateTTLSeconds: 0}
	if got := turnStateReuseWindowLocked("idx-zero"); got != defaultStateTTLSeconds*time.Second {
		t.Fatalf("zero means the default, got %s", got)
	}
	state.rules["idx-short"] = headerRule{AuthIndex: "idx-short", StateTTLSeconds: 30}
	state.rules["idx-long"] = headerRule{AuthIndex: "idx-long", StateTTLSeconds: 1800}
	if got := turnStateReuseWindowLocked("idx-short"); got != 30*time.Second {
		t.Fatalf("short window=%s", got)
	}
	if got := turnStateReuseWindowLocked("idx-long"); got != 1800*time.Second {
		t.Fatalf("long window=%s", got)
	}
}

// Two credentials can disagree about how long a state stays fresh, and the same
// blob age must be judged by whichever rule is serving the request.
func TestTheSameAgeIsJudgedPerCredential(t *testing.T) {
	resetRules(t)
	resetTurnStates(t)
	blob := fernetToken(0x80, time.Now().Add(-120*time.Second), 1)
	state.mu.Lock()
	defer state.mu.Unlock()
	state.rules["idx-short"] = headerRule{AuthIndex: "idx-short", StateTTLSeconds: 60}
	state.rules["idx-long"] = headerRule{AuthIndex: "idx-long", StateTTLSeconds: 600}
	short, _ := evaluateTurnStateEchoLocked(http.Header{turnStateHeader: {blob}}, "idx-short", "m")
	long, _ := evaluateTurnStateEchoLocked(http.Header{turnStateHeader: {blob}}, "idx-long", "m")
	if !short.expired {
		t.Fatalf("a two minute old blob is past a one minute window: %#v", short)
	}
	if long.expired {
		t.Fatalf("a two minute old blob is inside a ten minute window: %#v", long)
	}
}

// resetRules gives a test its own rule table without needing the request-path
// fixtures, which only exist in the localtest build.
func resetRules(t *testing.T) {
	t.Helper()
	state.mu.Lock()
	previous := state.rules
	state.rules = map[string]headerRule{}
	state.mu.Unlock()
	t.Cleanup(func() {
		state.mu.Lock()
		state.rules = previous
		state.mu.Unlock()
	})
}

// Header maps come across the ABI straight from JSON, so their keys keep
// whatever spelling the host serialised -- lowercase over HTTP/2 -- and one map
// can hold the same header under two spellings. Reading one of them is not
// enough, and picking whichever the map iterated first is not even stable.
func TestHeaderReadsDoNotTrustKeyCasing(t *testing.T) {
	t.Run("any single spelling is found", func(t *testing.T) {
		for _, key := range []string{"Cookie", "cookie", "COOKIE", "CooKie"} {
			if got := joinCookieHeader(http.Header{key: {"a=1"}}); got != "a=1" {
				t.Fatalf("%s: got %q", key, got)
			}
		}
		// Canonicalization is not identity for this one: Get would look up
		// X-Openai-Internal-Codex-Responses-Lite and miss the literal spelling.
		literal := "X-OpenAI-Internal-Codex-Responses-Lite"
		if got := headerValueFold(http.Header{literal: {"true"}}, literal); got != "true" {
			t.Fatalf("literal spelling not found: %q", got)
		}
	})

	t.Run("every spelling contributes", func(t *testing.T) {
		got := joinCookieHeader(http.Header{"Cookie": {"a=1"}, "cookie": {"b=2"}})
		if got != "a=1; b=2" {
			t.Fatalf("both spellings should be sent: %q", got)
		}
		merged := applySetCookies("a=1", http.Header{"Set-Cookie": {"a=9"}, "set-cookie": {"b=8"}})
		if merged != "a=9; b=8" {
			t.Fatalf("both assignments should apply: %q", merged)
		}
	})

	t.Run("the order does not depend on map iteration", func(t *testing.T) {
		// Same map contents, built repeatedly: a first-match-wins read would
		// return different answers across runs.
		for i := 0; i < 64; i++ {
			h := http.Header{"cookie": {"a=1"}, "COOKIE": {"b=2"}, "Cookie": {"c=3"}}
			if got := joinCookieHeader(h); got != "b=2; c=3; a=1" {
				t.Fatalf("run %d returned %q", i, got)
			}
		}
	})

	t.Run("a turn state under any spelling is read", func(t *testing.T) {
		blob := fernetToken(0x80, time.Now(), 1)
		for _, key := range []string{turnStateHeader, "x-codex-turn-state", "X-CODEX-TURN-STATE"} {
			if got := headerTurnState(http.Header{key: {blob}}); got != blob {
				t.Fatalf("%s: got %q", key, got)
			}
		}
	})

	// Header names fold; cookie names do not. RFC 6265 makes cookie names
	// case-sensitive, so these are two cookies and neither replaces the other.
	t.Run("cookie names stay case sensitive", func(t *testing.T) {
		got := applySetCookies("session=old", http.Header{"Set-Cookie": {"Session=new"}})
		if got != "session=old; Session=new" {
			t.Fatalf("got %q", got)
		}
	})
}
