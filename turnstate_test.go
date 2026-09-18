package main

import (
	"encoding/base64"
	"encoding/binary"
	"net/http"
	"testing"
	"time"
)

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
	noteTurnStateMintLocked(blob, "idx-a", "team-a", "gpt-5.6-luna")
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
	noteTurnStateMintLocked(blob, "idx-a", "team-a", "gpt-5.6-luna")
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
		noteTurnStateMintLocked(fernetToken(0x80, time.Now().Add(time.Duration(i)*time.Second), 1), "idx-a", "team-a", "gpt-5.6-luna")
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
	noteTurnStateMintLocked(blob, "idx-a", "team-a", "gpt-5.6-luna")
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
	noteTurnStateMintLocked(blob, "idx-a", "team-a", "gpt-5.6-luna")
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
	fresh := fernetToken(0x80, time.Now().Add(-5*time.Minute), 1)
	stale := fernetToken(0x80, time.Now().Add(-turnStateReuseWindow-10*time.Minute), 1)
	state.mu.Lock()
	noteTurnStateMintLocked(fresh, "idx-a", "team-a", "m")
	noteTurnStateMintLocked(stale, "idx-a", "team-a", "m")
	freshEcho, _ := evaluateTurnStateEchoLocked(http.Header{turnStateHeader: {fresh}}, "idx-a", "m")
	staleEcho, _ := evaluateTurnStateEchoLocked(http.Header{turnStateHeader: {stale}}, "idx-a", "m")
	state.mu.Unlock()

	if freshEcho.expired {
		t.Fatalf("a five minute old blob is fresh: %#v", freshEcho)
	}
	if freshEcho.ageSeconds < 240 || freshEcho.ageSeconds > 360 {
		t.Fatalf("age=%d", freshEcho.ageSeconds)
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
	stale := fernetToken(0x80, time.Now().Add(-2*turnStateReuseWindow), 1)
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
	noteTurnStateMintLocked(older, "idx-a", "team-a", "gpt-5.6-luna")
	noteTurnStateMintLocked(newer, "idx-a", "team-a", "gpt-5.6-luna")
	noteTurnStateMintLocked(older, "idx-a", "team-a", "gpt-5.1-codex")
	noteTurnStateMintLocked(newer, "idx-b", "team-b", "gpt-5.6-luna")
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
	noteTurnStateMintLocked(newer, "idx-a", "team-a", "m")
	noteTurnStateMintLocked(older, "idx-a", "team-a", "m")
	recent := recentTurnStatesLocked()
	state.mu.Unlock()
	if len(recent) != 1 || recent[0].digest != turnStateDigest(newer) {
		t.Fatalf("out of order mints must not rewind the latest entry: %#v", recent)
	}
}
