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
	out := retryOnce(context.Background(), "idx-a", "old-auth", "m", "")
	if !out.stop || len(*sent) != 0 {
		t.Fatal("rotated credential was used")
	}
	out = retryOnce(context.Background(), "idx-a", "auth-a", "m", "")
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

	runDegradedRetry(retrySeries{authIndex: "idx-a", authID: "auth-a", label: "A", name: "a.json", model: "gpt-5.6-luna"}, 3, nil)

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
	// The detail reads these; without them the retry row shows no headers at all.
	if rec.BeforeHeaders == nil || rec.ResponseHeaders == nil {
		t.Fatalf("the retry row should carry its own headers: %#v", rec)
	}
	if got := rec.BeforeHeaders.Get("Authorization"); got == "" || strings.Contains(got, "secret-token") {
		t.Fatalf("Authorization must be present but redacted, got %q", got)
	}
	if rec.ResponseHeaders.Get(turnStateHeader) == "" {
		t.Fatal("the response headers should include the state the retry fetched")
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

	runDegradedRetry(retrySeries{authIndex: "idx-a", authID: "auth-a", label: "A", name: "a.json", model: "gpt-5.6-luna"}, 2, nil)

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
	rule, err := saveRule(headerRule{AuthIndex: "idx-a", RetryProxyEnabled: true, RetryProxies: []string{"socks5://one:1080", "socks5h://two:1080"}})
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
		out := retryOnce(context.Background(), "idx-a", "auth-a", "m", "")
		if out.err == "" || out.nonDegraded {
			t.Fatal("accepted state from error response")
		}
	}
	if len(seen) != 2 || !seen["socks5://one:1080"] || !seen["socks5h://two:1080"] {
		t.Fatalf("not choosing per call from current credential: %v", seen)
	}
	seen = map[string]bool{}
	_ = retryOnce(context.Background(), "idx-b", "auth-a", "m", "")
	if !seen[""] || len(seen) != 1 {
		t.Fatal("inherited another credential's proxies")
	}
	rule.RetryProxies = nil
	if _, err := saveRule(rule); err != nil {
		t.Fatal(err)
	}
	seen = map[string]bool{}
	_ = retryOnce(context.Background(), "idx-a", "auth-a", "m", "")
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

// The retry answers for a request that was withheld, so it has to look like the
// same caller to the upstream: the Cookie of that request travels with the
// series, and is recorded as sent so the two can be compared.
func TestRetryCarriesTheInterceptedCookie(t *testing.T) {
	resetState(t)
	resetTurnStates(t)
	good := fernetToken(0x80, time.Now(), 1)
	sent := retryStub(t, "team", []string{good})
	state.mu.Lock()
	state.credentials["idx-a"] = credentialSnapshot{AuthIndex: "idx-a", AuthID: "auth-a", Provider: "codex", Name: "a.json"}
	state.rules["idx-a"] = headerRule{AuthIndex: "idx-a", Enabled: true, RejectDegradedResponse: true, RetryOnDegraded: true, RetryAttempts: 1}
	state.mu.Unlock()

	const cookie = "__Secure-next-auth.session-token=session-value; oai-did=device-value"
	if _, err := interceptAfter(requestInterceptRequest{RequestID: "r", Model: "gpt-5.6-luna",
		Headers:  http.Header{"Cookie": {cookie}},
		Metadata: map[string]any{"selected_auth_index": "idx-a", "selected_auth_id": "auth-a"},
	}); err != nil {
		t.Fatal(err)
	}
	// The response is withheld, so this call returns only once the series ends.
	observeResponse(responseInterceptRequest{RequestID: "r", StatusCode: 200,
		ResponseHeaders: http.Header{turnStateHeader: {fernetToken(0x80, time.Now(), 40)}}})

	if len(*sent) != 1 {
		t.Fatalf("the series should have made one attempt, made %d", len(*sent))
	}
	if got := (*sent)[0].Headers.Get("Cookie"); got != cookie {
		t.Fatalf("the retry sent cookie %q, want the intercepted one", got)
	}
	rec := lastAttempt(t)
	if rec.Origin != originRetry {
		t.Fatalf("the last row should be the retry: %#v", rec)
	}
	if got := rec.BeforeHeaders.Get("Cookie"); got != cookie {
		t.Fatalf("the recorded cookie should be what was sent, got %q", got)
	}
	// Authorization travels on the same request and is still redacted: reading
	// cookies was the ask, not reading everything.
	if got := rec.BeforeHeaders.Get("Authorization"); got != "Bearer [REDACTED]" {
		t.Fatalf("Authorization must stay redacted, got %q", got)
	}
}

// A request without a Cookie must not grow one: the retry sends what the
// original sent, and an empty header would be a header the client never set.
func TestRetryOmitsTheCookieWhenTheRequestHadNone(t *testing.T) {
	resetState(t)
	resetTurnStates(t)
	sent := retryStub(t, "team", []string{fernetToken(0x80, time.Now(), 1)})
	state.mu.Lock()
	state.credentials["idx-a"] = credentialSnapshot{AuthIndex: "idx-a", AuthID: "auth-a", Provider: "codex", Name: "a.json"}
	state.mu.Unlock()

	runDegradedRetry(retrySeries{authIndex: "idx-a", authID: "auth-a", label: "A", name: "a.json", model: "gpt-5.6-luna"}, 1, nil)

	if len(*sent) != 1 {
		t.Fatalf("one attempt expected, made %d", len(*sent))
	}
	if _, ok := (*sent)[0].Headers["Cookie"]; ok {
		t.Fatalf("no cookie was sent, so none should be set: %v", (*sent)[0].Headers)
	}
}

// HTTP/2 may deliver Cookie split into crumbs; the wire form joins them, so a
// request that arrives split must not be half-copied.
func TestSplitCookieCrumbsAreRejoined(t *testing.T) {
	cases := []struct {
		name    string
		headers http.Header
		want    string
	}{
		{"one value", http.Header{"Cookie": {"a=1; b=2"}}, "a=1; b=2"},
		{"crumbs", http.Header{"Cookie": {"a=1", "b=2"}}, "a=1; b=2"},
		{"lowercase key", http.Header{"cookie": {"a=1"}}, "a=1"},
		{"blank crumb", http.Header{"Cookie": {"a=1", "  "}}, "a=1"},
		{"absent", http.Header{"Accept": {"*/*"}}, ""},
		{"nil", nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := joinCookieHeader(tc.headers); got != tc.want {
				t.Fatalf("got %q want %q", got, tc.want)
			}
		})
	}
}

// The withheld response is itself a chance for the upstream to rotate or issue
// the session cookie, so the retry sends the request's cookie updated by what
// that response set -- not the copy the request arrived with.
func TestSetCookieFromTheResponseUpdatesTheRetryCookie(t *testing.T) {
	cases := []struct {
		name     string
		request  string
		response []string
		want     string
	}{
		{"nothing set leaves the request cookie alone", "a=1; b=2", nil, "a=1; b=2"},
		{"a rotated value replaces it in place", "a=1; b=2", []string{"a=9; Path=/; HttpOnly"}, "a=9; b=2"},
		{"a new cookie is appended", "a=1", []string{"c=3; Secure"}, "a=1; c=3"},
		{"issued with no request cookie", "", []string{"c=3; Path=/"}, "c=3"},
		{"attributes are not sent back", "", []string{"s=v; Expires=Wed, 21 Oct 2026 07:28:00 GMT; SameSite=Lax"}, "s=v"},
		{"an emptied cookie is dropped, not echoed empty", "a=1; b=2", []string{"a=; Path=/"}, "b=2"},
		{"max-age zero is a deletion", "a=1; b=2", []string{"b=x; Max-Age=0"}, "a=1"},
		{"a later assignment wins", "a=1", []string{"a=2", "a=3"}, "a=3"},
		{"cleared then reissued appears once", "a=1", []string{"a=; Max-Age=0", "a=4"}, "a=4"},
		{"a value containing = survives", "", []string{"t=eyJhbGc=.payload=; Path=/"}, "t=eyJhbGc=.payload="},
		{"several headers all apply", "a=1", []string{"b=2", "c=3"}, "a=1; b=2; c=3"},
		{"a malformed assignment is skipped", "a=1", []string{"garbage", "b=2"}, "a=1; b=2"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			headers := http.Header{}
			for _, value := range tc.response {
				headers.Add("Set-Cookie", value)
			}
			if got := applySetCookies(tc.request, headers); got != tc.want {
				t.Fatalf("got %q want %q", got, tc.want)
			}
		})
	}
}

// End to end: the cookie the retry sends is the merged one, and the history row
// shows it, so the session a retry presented can be compared afterwards.
func TestRetrySendsTheCookieTheResponseRotated(t *testing.T) {
	resetState(t)
	resetTurnStates(t)
	sent := retryStub(t, "team", []string{fernetToken(0x80, time.Now(), 1)})
	state.mu.Lock()
	state.credentials["idx-a"] = credentialSnapshot{AuthIndex: "idx-a", AuthID: "auth-a", Provider: "codex", Name: "a.json"}
	state.rules["idx-a"] = headerRule{AuthIndex: "idx-a", Enabled: true, RejectDegradedResponse: true, RetryOnDegraded: true, RetryAttempts: 1}
	state.mu.Unlock()

	if _, err := interceptAfter(requestInterceptRequest{RequestID: "r", Model: "gpt-5.6-luna",
		Headers:  http.Header{"Cookie": {"session=old; oai-did=device"}},
		Metadata: map[string]any{"selected_auth_index": "idx-a", "selected_auth_id": "auth-a"},
	}); err != nil {
		t.Fatal(err)
	}
	observeResponse(responseInterceptRequest{RequestID: "r", StatusCode: 200, ResponseHeaders: http.Header{
		turnStateHeader: {fernetToken(0x80, time.Now(), 40)},
		"Set-Cookie":    {"session=rotated; Path=/; HttpOnly", "issued=fresh; Secure"},
	}})

	if len(*sent) != 1 {
		t.Fatalf("one attempt expected, made %d", len(*sent))
	}
	const want = "session=rotated; oai-did=device; issued=fresh"
	if got := (*sent)[0].Headers.Get("Cookie"); got != want {
		t.Fatalf("retry sent %q, want %q", got, want)
	}
	if got := lastAttempt(t).BeforeHeaders.Get("Cookie"); got != want {
		t.Fatalf("the history row shows %q, want %q", got, want)
	}
}

// The retry fills the pool, so a frozen pool stands it down whatever the
// retry's own switches say.
func TestRetryStandsDownWhileThePoolIsFrozen(t *testing.T) {
	resetState(t)
	paused := false
	state.mu.Lock()
	state.rules["idx-a"] = headerRule{AuthIndex: "idx-a", RejectDegradedResponse: true, RetryOnDegraded: true, RetryAttempts: 2, MaintainStatePool: &paused}
	got := retryEnabledForLocked("idx-a")
	state.mu.Unlock()
	if got != 0 {
		t.Fatalf("a frozen pool must not schedule retries, got %d attempts", got)
	}
}
