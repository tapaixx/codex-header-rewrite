//go:build localtest

package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestRetryRequiresRejectionSwitch(t *testing.T) {
	resetState(t)
	state.mu.Lock()
	defer state.mu.Unlock()
	for _, reject := range []bool{false, true} {
		state.rules["idx-a"] = headerRule{RejectDegradedResponse: reject, RetryOnDegraded: true, RetryAttempts: 3}
		got := retryEnabledForLocked("idx-a")
		want := 0
		if reject {
			want = 3
		}
		if got != want {
			t.Fatalf("reject=%v: retries=%d want=%d", reject, got, want)
		}
	}
}

func TestRetryRejectsRotatedIdentityAndUsesCurrentPlan(t *testing.T) {
	resetState(t)
	resetTurnStates(t)
	sent := retryStub(t, "pro", []string{strings.Repeat("x", 300)})
	out := retryOnce(context.Background(), "idx-a", "old-auth", "m")
	if !out.stop || len(*sent) != 0 {
		t.Fatal("rotated credential was used")
	}
	out = retryOnce(context.Background(), "idx-a", "auth-a", "m")
	if out.plan != "pro" || out.nonDegraded {
		t.Fatalf("wrong plan classification: %#v", out)
	}
}

func TestQuiesceWaitsForRetryAndDiscardsItsResult(t *testing.T) {
	resetState(t)
	resetTurnStates(t)
	retryStub(t, "team", nil)
	entered, release := make(chan struct{}), make(chan struct{})
	retryHTTPDoFunc = func(context.Context, hostHTTPRequest, string) (hostHTTPResponse, error) {
		close(entered)
		<-release
		return hostHTTPResponse{StatusCode: 200, Headers: http.Header{turnStateHeader: {fernetToken(0x80, time.Now(), 1)}}}, nil
	}
	state.mu.Lock()
	scheduleDegradedRetry(&pendingAttempt{historyRecord: historyRecord{AuthIndex: "idx-a", AuthID: "auth-a", Model: "m", CredentialPlan: "team"}}, 2)
	state.mu.Unlock()
	<-entered
	finished := make(chan error, 1)
	go func() { finished <- quiescePlugin() }()
	// Wait for the cancellation flag, not an arbitrary scheduling delay.
	deadline := time.Now().Add(time.Second)
	for {
		state.mu.Lock()
		stopping := state.quiescing
		state.mu.Unlock()
		if stopping {
			break
		}
		if time.Now().After(deadline) {
			close(release)
			<-finished
			t.Fatal("quiesce did not begin")
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case <-finished:
		close(release)
		t.Fatal("quiesce returned with host call active")
	default:
	}
	close(release)
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if len(state.turnStateLatest) != 0 || state.store != nil {
		t.Fatal("old retry populated pool during shutdown")
	}
}

// retryStub answers each call with the blob the test supplies, and records
// what went out so the request shape can be asserted.
func retryStub(t *testing.T, plan string, blobs []string) *[]hostHTTPRequest {
	t.Helper()
	oldGet, oldDo, oldRuntime := hostAuthGetFunc, retryHTTPDoFunc, hostAuthGetRuntimeFunc
	hostAuthGetRuntimeFunc = func(index string) (hostAuthGetRuntimeResponse, error) {
		return hostAuthGetRuntimeResponse{Auth: hostAuthFileEntry{ID: "auth-a", AuthIndex: index, Provider: "codex"}}, nil
	}
	var doc map[string]any
	_ = json.Unmarshal(credentialDocumentWithPlan(t, plan), &doc)
	doc["access_token"] = "secret-token"
	doc["chatgpt_account_id"] = "acct-1"
	raw, _ := json.Marshal(doc)
	hostAuthGetFunc = func(string) (json.RawMessage, error) { return raw, nil }
	sent := []hostHTTPRequest{}
	i := 0
	retryHTTPDoFunc = func(_ context.Context, req hostHTTPRequest, _ string) (hostHTTPResponse, error) {
		sent = append(sent, req)
		blob := ""
		if i < len(blobs) {
			blob = blobs[i]
		}
		i++
		return hostHTTPResponse{StatusCode: 200, Headers: http.Header{turnStateHeader: {blob}}, Body: []byte("ignored")}, nil
	}
	t.Cleanup(func() { hostAuthGetFunc, retryHTTPDoFunc, hostAuthGetRuntimeFunc = oldGet, oldDo, oldRuntime })
	return &sent
}

// The retry asks the same credential and model for a fresh state, sends only
// "hi", stops at the first non-degraded answer, pools it, and reports the
// whole series as one history row.
func TestRetryPoolsTheFirstNonDegradedStateAndRecordsOneRow(t *testing.T) {
	resetState(t)
	resetTurnStates(t)
	degraded := fernetToken(0x80, time.Now(), 40)
	good := fernetToken(0x80, time.Now(), 1)
	sent := retryStub(t, "team", []string{degraded, good})
	state.mu.Lock()
	state.credentials["idx-a"] = credentialSnapshot{AuthIndex: "idx-a", AuthID: "auth-a", Provider: "codex", Name: "a.json"}
	state.mu.Unlock()

	runDegradedRetry("idx-a", "auth-a", "A", "a.json", "gpt-5.6-luna", "team", 3, nil)

	if len(*sent) != 2 {
		t.Fatalf("should have stopped at the first good state, sent %d", len(*sent))
	}
	first := (*sent)[0]
	if first.URL != defaultTestURL || first.Method != http.MethodPost {
		t.Fatalf("unexpected target: %s %s", first.Method, first.URL)
	}
	if !strings.Contains(string(first.Body), `"hi"`) || !strings.Contains(string(first.Body), `"gpt-5.6-luna"`) {
		t.Fatalf("payload should carry the prompt and the current model: %s", first.Body)
	}
	if first.Headers.Get("Authorization") == "" || first.Headers.Get("Chatgpt-Account-Id") != "acct-1" {
		t.Fatalf("credential not attached: %v", first.Headers)
	}
	state.mu.Lock()
	origin, ok := state.turnStateLatest[turnStateLatestKey("idx-a", "gpt-5.6-luna")]
	state.mu.Unlock()
	if !ok || origin.blob != good {
		t.Fatal("the non-degraded state should have been pooled under this credential")
	}
	rec := lastAttempt(t)
	if rec.Origin != originRetry || rec.RetryAttempts != 2 || rec.Outcome != "succeeded" {
		t.Fatalf("one row summarising the series expected: %#v", rec)
	}
}

// Every attempt degraded: nothing is pooled, the row says how many were made
// and that it failed, and the attempt cap is respected.
func TestRetryStopsAtTheCapAndReportsFailure(t *testing.T) {
	resetState(t)
	resetTurnStates(t)
	degraded := fernetToken(0x80, time.Now(), 40)
	sent := retryStub(t, "team", []string{degraded, degraded, degraded, degraded})
	state.mu.Lock()
	state.credentials["idx-a"] = credentialSnapshot{AuthIndex: "idx-a", AuthID: "auth-a", Provider: "codex", Name: "a.json"}
	state.mu.Unlock()

	runDegradedRetry("idx-a", "auth-a", "A", "a.json", "gpt-5.6-luna", "team", 2, nil)

	if len(*sent) != 2 {
		t.Fatalf("the cap should bound the series, sent %d", len(*sent))
	}
	state.mu.Lock()
	_, pooled := state.turnStateLatest[turnStateLatestKey("idx-a", "gpt-5.6-luna")]
	state.mu.Unlock()
	if pooled {
		t.Fatal("a degraded state must not enter the pool")
	}
	if rec := lastAttempt(t); rec.RetryAttempts != 2 || rec.Outcome != "failed" {
		t.Fatalf("failed series expected: %#v", rec)
	}
}

// The count comes from the rule, defaults when unset, and is capped.
func TestRetryAttemptCountHonoursTheRuleAndTheCap(t *testing.T) {
	if got := retryAttemptCount(headerRule{}); got != defaultRetryAttempts {
		t.Fatalf("default=%d", got)
	}
	if got := retryAttemptCount(headerRule{RetryAttempts: 3}); got != 3 {
		t.Fatalf("explicit=%d", got)
	}
	if got := retryAttemptCount(headerRule{RetryAttempts: 99}); got != maxRetryAttempts {
		t.Fatalf("capped=%d", got)
	}
	if _, err := validateRule(headerRule{AuthIndex: "idx-a", RetryAttempts: 99}); err == nil {
		t.Fatal("validation should reject a count above the cap")
	}
}

func TestRetrySelectsProxyPerCallAndKeepsCredentialsIsolated(t *testing.T) {
	resetState(t)
	retryStub(t, "team", nil)
	rule, err := saveRule(headerRule{AuthIndex: "idx-a", RetryProxies: []string{"socks5://one:1080", "socks5h://two:1080"}})
	if err != nil {
		t.Fatal(err)
	}
	persisted, ok, err := state.store.GetRule("idx-a")
	if err != nil || !ok || len(persisted.RetryProxies) != 2 {
		t.Fatalf("proxy settings not saved: %#v %v", persisted, err)
	}
	seen := map[string]bool{}
	retryHTTPDoFunc = func(_ context.Context, _ hostHTTPRequest, proxy string) (hostHTTPResponse, error) {
		seen[proxy] = true
		return hostHTTPResponse{StatusCode: 503, Headers: http.Header{turnStateHeader: {"short-but-error"}}}, nil
	}
	for i := 0; i < 64; i++ {
		out := retryOnce(context.Background(), "idx-a", "auth-a", "m")
		if out.err == "" || out.nonDegraded {
			t.Fatal("accepted state from error response")
		}
	}
	if len(seen) != 2 || !seen["socks5://one:1080"] || !seen["socks5h://two:1080"] {
		t.Fatalf("not choosing per call from current credential: %v", seen)
	}
	seen = map[string]bool{}
	_ = retryOnce(context.Background(), "idx-b", "auth-a", "m")
	if !seen[""] || len(seen) != 1 {
		t.Fatal("inherited another credential's proxies")
	}
	rule.RetryProxies = nil
	if _, err := saveRule(rule); err != nil {
		t.Fatal(err)
	}
	seen = map[string]bool{}
	_ = retryOnce(context.Background(), "idx-a", "auth-a", "m")
	if !seen[""] || len(seen) != 1 {
		t.Fatal("cleared proxies did not select direct")
	}
}

// A withheld response must not reach the client before the retry series has
// finished. Returning first sends the client straight back at an upstream
// whose pool has not been refilled, which is the loop this exists to break.
func TestWithheldResponseWaitsForTheRetrySeries(t *testing.T) {
	stubCredentialPlan(t, "team")
	resetState(t)
	resetTurnStates(t)
	release := make(chan struct{})
	oldGet, oldDo, oldRuntime := hostAuthGetFunc, retryHTTPDoFunc, hostAuthGetRuntimeFunc
	hostAuthGetRuntimeFunc = func(index string) (hostAuthGetRuntimeResponse, error) {
		return hostAuthGetRuntimeResponse{Auth: hostAuthFileEntry{ID: "auth-a", AuthIndex: index, Provider: "codex"}}, nil
	}
	var doc map[string]any
	_ = json.Unmarshal(credentialDocumentWithPlan(t, "team"), &doc)
	doc["access_token"] = "secret-token"
	raw, _ := json.Marshal(doc)
	hostAuthGetFunc = func(string) (json.RawMessage, error) { return raw, nil }
	retryHTTPDoFunc = func(context.Context, hostHTTPRequest, string) (hostHTTPResponse, error) {
		<-release // the series cannot finish until the test lets it
		return hostHTTPResponse{StatusCode: 200, Headers: http.Header{turnStateHeader: {fernetToken(0x80, time.Now(), 1)}}}, nil
	}
	t.Cleanup(func() { hostAuthGetFunc, retryHTTPDoFunc, hostAuthGetRuntimeFunc = oldGet, oldDo, oldRuntime })

	state.mu.Lock()
	state.credentials["idx-a"] = credentialSnapshot{AuthIndex: "idx-a", AuthID: "auth-a", Provider: "codex", Name: "a.json"}
	state.rules["idx-a"] = headerRule{AuthIndex: "idx-a", Enabled: true, RejectDegradedResponse: true, RetryOnDegraded: true, RetryAttempts: 1}
	state.mu.Unlock()
	if _, err := interceptAfter(requestInterceptRequest{RequestID: "r", Model: "gpt-5.6-luna", Metadata: map[string]any{"selected_auth_index": "idx-a", "selected_auth_id": "auth-a"}}); err != nil {
		t.Fatal(err)
	}

	returned := make(chan responseInterceptResponse, 1)
	go func() {
		returned <- observeResponse(responseInterceptRequest{RequestID: "r", StatusCode: 200,
			ResponseHeaders: http.Header{turnStateHeader: {fernetToken(0x80, time.Now(), 40)}}})
	}()
	select {
	case out := <-returned:
		t.Fatalf("the response returned before the retry finished: %s", out.Body)
	case <-time.After(200 * time.Millisecond):
	}

	// Other requests must still be served while one is held.
	free := make(chan struct{})
	go func() { defer close(free); state.mu.Lock(); state.mu.Unlock() }()
	select {
	case <-free:
	case <-time.After(time.Second):
		t.Fatal("the held response is holding the state lock")
	}

	close(release)
	select {
	case out := <-returned:
		if !strings.Contains(string(out.Body), `"turn_state_degraded"`) {
			t.Fatalf("expected the withheld error body, got %s", out.Body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the response never returned after the retry finished")
	}
}
