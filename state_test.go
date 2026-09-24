//go:build localtest

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func resetState(t *testing.T) persistence {
	t.Helper()
	shutdownPlugin()
	p, err := openPersistence(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	state.mu.Lock()
	state.store = p
	state.quiescing = false
	state.retryStop = make(chan struct{})
	state.writer = newQueuedPersistence(p)
	state.rules = map[string]headerRule{}
	state.pending = map[string]*pendingRequest{}
	state.credentials = map[string]credentialSnapshot{}
	state.mu.Unlock()
	t.Cleanup(shutdownPlugin)
	return p
}

func stubCredentialPlan(t *testing.T, plan string) {
	t.Helper()
	oldGet := hostAuthGetFunc
	hostAuthGetFunc = func(string) (json.RawMessage, error) {
		return credentialDocumentWithPlan(t, plan), nil
	}
	t.Cleanup(func() { hostAuthGetFunc = oldGet })
}

func TestTurnStatePoolSurvivesPluginReconfigure(t *testing.T) {
	shutdownPlugin()
	resetTurnStates(t)
	dataPath := filepath.Join(t.TempDir(), "state.json")
	request, err := json.Marshal(lifecycleRequest{ConfigYAML: []byte("data_path: " + dataPath)})
	if err != nil {
		t.Fatal(err)
	}
	if err := configurePlugin(request); err != nil {
		t.Fatal(err)
	}
	blob := fernetToken(0x80, time.Now(), 1)
	state.mu.Lock()
	pooled := noteTurnStateMintLocked(blob, "idx-pro-a", "Pro A", "gpt-5.6-luna", "pro", "")
	state.mu.Unlock()
	if !pooled {
		t.Fatal("fixture state did not enter the pool")
	}
	if err := quiescePlugin(); err != nil {
		t.Fatal(err)
	}
	state.mu.Lock()
	state.turnStates = map[string]turnStateOrigin{}
	state.turnStateLatest = map[string]turnStateOrigin{}
	state.mu.Unlock()

	if err := configurePlugin(request); err != nil {
		t.Fatal(err)
	}
	resp, _ := handleManagementAPI(managementRequest{Method: "GET", Path: "/v0/management" + apiTurnStatesPath})
	var payload struct {
		TurnStates []struct {
			AuthIndex string `json:"auth_index"`
			Label     string `json:"label"`
			Model     string `json:"model"`
			State     string `json:"state"`
		} `json:"turn_states"`
	}
	if err := json.Unmarshal(resp.Body, &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.TurnStates) != 1 {
		t.Fatalf("restored states=%#v", payload.TurnStates)
	}
	got := payload.TurnStates[0]
	if got.AuthIndex != "idx-pro-a" || got.Label != "Pro A" || got.Model != "gpt-5.6-luna" || got.State != blob {
		t.Fatalf("restored state=%#v", got)
	}
}

func TestCredentialPlanLookupRetriesAndRefreshes(t *testing.T) {
	resetState(t)
	state.mu.Lock()
	state.credentials["idx-a"] = credentialSnapshot{AuthIndex: "idx-a", Provider: "codex", Name: "a.json"}
	state.mu.Unlock()
	oldGet := hostAuthGetFunc
	calls := 0
	hostAuthGetFunc = func(string) (json.RawMessage, error) {
		calls++
		switch calls {
		case 1:
			return nil, errors.New("temporary host failure")
		case 2:
			return credentialDocumentWithPlan(t, "team"), nil
		case 3:
			return credentialDocumentWithPlan(t, "pro"), nil
		default:
			return nil, errors.New("credential unreadable after rotation")
		}
	}
	t.Cleanup(func() { hostAuthGetFunc = oldGet })

	first, ok := resolveCodexCredential("idx-a", "")
	if !ok || first.PlanResolved {
		t.Fatalf("failed lookup was cached: %#v", first)
	}
	second, _ := resolveCodexCredential("idx-a", "")
	third, _ := resolveCodexCredential("idx-a", "")
	fourth, _ := resolveCodexCredential("idx-a", "")
	if second.PlanType != "team" || third.PlanType != "pro" || fourth.PlanResolved || fourth.PlanType != "" || calls != 4 {
		t.Fatalf("second=%#v third=%#v fourth=%#v calls=%d", second, third, fourth, calls)
	}
}

func TestRetryAttemptsBelongToEachCredential(t *testing.T) {
	p := resetState(t)
	state.mu.Lock()
	state.credentials["idx-a"] = credentialSnapshot{AuthIndex: "idx-a", AuthID: "a", Provider: "codex", Name: "a.json"}
	state.credentials["idx-b"] = credentialSnapshot{AuthIndex: "idx-b", AuthID: "b", Provider: "codex", Name: "b.json"}
	state.rules["idx-a"] = headerRule{AuthIndex: "idx-a", Enabled: true, Set: map[string]string{"X-Team": "A"}}
	state.rules["idx-b"] = headerRule{AuthIndex: "idx-b", Enabled: true, Set: map[string]string{"X-Team": "B"}}
	state.mu.Unlock()
	r1, err := interceptAfter(requestInterceptRequest{RequestID: "req", Model: "gpt", Headers: http.Header{"Authorization": {"Bearer real"}}, Metadata: map[string]any{"selected_auth_index": "idx-a", "selected_auth_id": "a"}})
	if err != nil || r1.Headers.Get("X-Team") != "A" {
		t.Fatalf("r1=%#v err=%v", r1, err)
	}
	r2, err := interceptAfter(requestInterceptRequest{RequestID: "req", Model: "gpt", Metadata: map[string]any{"selected_auth_index": "idx-b", "selected_auth_id": "b"}})
	if err != nil || r2.Headers.Get("X-Team") != "B" {
		t.Fatalf("r2=%#v err=%v", r2, err)
	}
	observeResponse(responseInterceptRequest{RequestID: "req", StatusCode: 200, ResponseHeaders: http.Header{"X-Api-Key": {"secret"}, "Set-Cookie": {"session=kept"}, "X-Upstream": {"ok"}}})
	completeRequest(requestCompletion{RequestID: "req", Outcome: "succeeded", StatusCode: 200, StartedAt: time.Now().Add(-time.Second), CompletedAt: time.Now()})
	state.mu.Lock()
	writer := state.writer
	state.writer = nil
	state.store = nil
	state.mu.Unlock()
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	pa, _ := p.History("idx-a", 1, pageSize)
	pb, _ := p.History("idx-b", 1, pageSize)
	if len(pa.Items) != 1 || pa.Items[0].Outcome != "switched" || pa.Items[0].StatusCode != 0 {
		t.Fatalf("A=%#v", pa.Items)
	}
	if len(pb.Items) != 1 || pb.Items[0].Outcome != "succeeded" || pb.Items[0].StatusCode != 200 {
		t.Fatalf("B=%#v", pb.Items)
	}
	if pa.Items[0].BeforeHeaders.Get("Authorization") != "Bearer [REDACTED]" {
		t.Fatalf("request secret not redacted %#v", pa.Items[0].BeforeHeaders)
	}
	if pb.Items[0].ResponseHeaders.Get("X-Api-Key") != "[REDACTED]" {
		t.Fatalf("response secret not redacted %#v", pb.Items[0].ResponseHeaders)
	}
	// Cookies are recorded as they were, so the session can be compared.
	if pb.Items[0].ResponseHeaders.Get("Set-Cookie") != "session=kept" {
		t.Fatalf("Set-Cookie should be recorded verbatim %#v", pb.Items[0].ResponseHeaders)
	}
}

func TestLiveStreamRecordsTheModelTheUpstreamServed(t *testing.T) {
	p := resetState(t)
	state.mu.Lock()
	state.credentials["idx-a"] = credentialSnapshot{AuthIndex: "idx-a", AuthID: "a", Provider: "codex", Name: "a.json"}
	state.mu.Unlock()
	if _, err := interceptAfter(requestInterceptRequest{RequestID: "req", Model: "gpt-5.6-luna", RequestedModel: "gpt-5", Stream: true, Metadata: map[string]any{"selected_auth_index": "idx-a"}}); err != nil {
		t.Fatal(err)
	}
	observeStreamHeaders(streamChunkInterceptRequest{RequestID: "req", ChunkIndex: streamChunkHeaderInitIndex, ResponseHeaders: http.Header{"X-Upstream": {"ok"}}})
	observeStreamHeaders(streamChunkInterceptRequest{RequestID: "req", ChunkIndex: 0, Body: []byte("event: response.created\ndata: {\"response\":{\"mod")})
	observeStreamHeaders(streamChunkInterceptRequest{RequestID: "req", ChunkIndex: 1, Body: []byte("el\":\"gpt-5.6-luna-mini\"}}\n\n")})
	completeRequest(requestCompletion{RequestID: "req", Outcome: "succeeded", StatusCode: 200, CompletedAt: time.Now()})
	state.mu.Lock()
	writer := state.writer
	state.writer = nil
	state.store = nil
	state.mu.Unlock()
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	page, _ := p.History("idx-a", 1, pageSize)
	if len(page.Items) != 1 {
		t.Fatalf("history=%#v", page.Items)
	}
	rec := page.Items[0]
	if rec.UpstreamModel != "gpt-5.6-luna-mini" {
		t.Fatalf("upstream model=%q", rec.UpstreamModel)
	}
	if rec.ModelMismatch == nil || !*rec.ModelMismatch {
		t.Fatalf("mismatch=%v", rec.ModelMismatch)
	}
	if rec.Origin != originLive {
		t.Fatalf("origin=%q", rec.Origin)
	}
	raw, _ := json.Marshal(rec)
	if strings.Contains(string(raw), "response.created") {
		t.Fatalf("stream body reached persistence: %s", raw)
	}
}

func TestLiveCallbackModelReachesHistory(t *testing.T) {
	p := resetState(t)
	state.mu.Lock()
	state.credentials["idx-callback"] = credentialSnapshot{AuthIndex: "idx-callback", Provider: "codex"}
	state.mu.Unlock()
	_, err := interceptAfter(requestInterceptRequest{RequestID: "callback-model", Model: "asked", Metadata: map[string]any{"selected_auth_index": "idx-callback"}})
	if err != nil {
		t.Fatal(err)
	}
	observeStreamHeaders(streamChunkInterceptRequest{RequestID: "callback-model", ChunkIndex: 0, Body: []byte(`data: {"type":"response.created","response":{"model":"initial"}}`)})
	observeStreamHeaders(streamChunkInterceptRequest{RequestID: "callback-model", ChunkIndex: 1, Body: []byte(`data: {"type":"response.completed","response":{"model":"actual"}}`)})
	completeRequest(requestCompletion{RequestID: "callback-model", Outcome: "succeeded", StatusCode: 200})
	shutdownPlugin()
	page, err := p.History("idx-callback", 1, pageSize)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || page.Items[0].UpstreamModel != "actual" || page.Items[0].ModelMismatch == nil || !*page.Items[0].ModelMismatch {
		t.Fatalf("history=%#v", page.Items)
	}
}

func TestNonStreamingResponseBodyIsReadForTheModelOnly(t *testing.T) {
	p := resetState(t)
	state.mu.Lock()
	state.credentials["idx-a"] = credentialSnapshot{AuthIndex: "idx-a", Provider: "codex", Name: "a.json"}
	state.mu.Unlock()
	if _, err := interceptAfter(requestInterceptRequest{RequestID: "req2", Model: "gpt-5.6-luna", Metadata: map[string]any{"selected_auth_index": "idx-a"}}); err != nil {
		t.Fatal(err)
	}
	observeResponse(responseInterceptRequest{RequestID: "req2", StatusCode: 200, Body: []byte(`{"model":"gpt-5.6-luna","output":[{"text":"private answer"}]}`)})
	completeRequest(requestCompletion{RequestID: "req2", Outcome: "succeeded", StatusCode: 200, CompletedAt: time.Now()})
	state.mu.Lock()
	writer := state.writer
	state.writer = nil
	state.store = nil
	state.mu.Unlock()
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	page, _ := p.History("idx-a", 1, pageSize)
	if len(page.Items) != 1 {
		t.Fatalf("history=%#v", page.Items)
	}
	rec := page.Items[0]
	if rec.UpstreamModel != "gpt-5.6-luna" || rec.ModelMismatch == nil || *rec.ModelMismatch {
		t.Fatalf("rec=%#v mismatch=%v", rec, rec.ModelMismatch)
	}
	raw, _ := json.Marshal(rec)
	if strings.Contains(string(raw), "private answer") {
		t.Fatalf("response body reached persistence: %s", raw)
	}
}

// A turn chain across a credential switch: the upstream mints the blob under
// idx-a, then the next request goes out under idx-b still echoing it.
func TestForeignTurnStateEchoIsFlaggedAndOptionallyStripped(t *testing.T) {
	stubCredentialPlan(t, "team")
	for _, strip := range []bool{false, true} {
		p := resetState(t)
		resetTurnStates(t)
		state.mu.Lock()
		state.credentials["idx-a"] = credentialSnapshot{AuthIndex: "idx-a", Provider: "codex", Name: "a.json", Label: "team-a", PlanType: "team", PlanResolved: true}
		state.credentials["idx-b"] = credentialSnapshot{AuthIndex: "idx-b", Provider: "codex", Name: "b.json", Label: "team-b", PlanType: "team", PlanResolved: true}
		state.rules["idx-b"] = headerRule{AuthIndex: "idx-b", Enabled: true, InjectTurnState: strip}
		state.mu.Unlock()

		blob := fernetToken(0x80, time.Now(), 2)
		if _, err := interceptAfter(requestInterceptRequest{RequestID: "turn-1", Model: "gpt-5.6-luna", Headers: http.Header{"Session-Id": {"sess-9"}}, Metadata: map[string]any{"selected_auth_index": "idx-a"}}); err != nil {
			t.Fatal(err)
		}
		observeResponse(responseInterceptRequest{RequestID: "turn-1", StatusCode: 200, ResponseHeaders: http.Header{turnStateHeader: {blob}}})
		completeRequest(requestCompletion{RequestID: "turn-1", Outcome: "succeeded", StatusCode: 200, CompletedAt: time.Now()})

		resp, err := interceptAfter(requestInterceptRequest{RequestID: "turn-2", Model: "gpt-5.6-luna",
			Headers:  http.Header{"Session-Id": {"sess-9"}, turnStateHeader: {blob}},
			Metadata: map[string]any{"selected_auth_index": "idx-b"}})
		if err != nil {
			t.Fatal(err)
		}
		completeRequest(requestCompletion{RequestID: "turn-2", Outcome: "succeeded", StatusCode: 200, CompletedAt: time.Now()})

		cleared := false
		for _, name := range resp.ClearHeaders {
			if strings.EqualFold(name, turnStateHeader) {
				cleared = true
			}
		}
		if cleared != strip {
			t.Fatalf("strip=%v cleared=%v", strip, cleared)
		}

		state.mu.Lock()
		writer := state.writer
		state.writer = nil
		state.store = nil
		state.mu.Unlock()
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}

		minted, _ := p.History("idx-a", 1, pageSize)
		if len(minted.Items) != 1 || minted.Items[0].TurnStateMinted == nil {
			t.Fatalf("mint not recorded: %#v", minted.Items)
		}
		if !minted.Items[0].TurnStateMinted.FernetLike {
			t.Fatalf("minted envelope not decoded: %#v", minted.Items[0].TurnStateMinted)
		}
		quality := minted.Items[0].TurnStateMinted
		// Nothing was injected on that request, so the live rule has nothing to
		// read: the state is recorded and waits for a manual pool.
		if quality.Pooled || !quality.ManualPool || quality.NonDegraded != nil || quality.PlanType != "team" {
			t.Fatalf("an unjudged live state should wait for a hand: %#v", quality)
		}

		echoed, _ := p.History("idx-b", 1, pageSize)
		if len(echoed.Items) != 1 {
			t.Fatalf("echo not recorded: %#v", echoed.Items)
		}
		record := echoed.Items[0]
		if record.TurnStateEcho == nil || record.TurnStateCrossAccount == nil || !*record.TurnStateCrossAccount {
			t.Fatalf("cross-account echo not flagged: %#v", record)
		}
		if record.TurnStateOriginIndex != "idx-a" || record.TurnStateOriginLabel != "team-a" {
			t.Fatalf("minting credential not reported: %#v", record)
		}
		if record.TurnStateSessionID != "sess-9" {
			t.Fatalf("session=%q", record.TurnStateSessionID)
		}
		if record.TurnStateStripped != strip {
			t.Fatalf("strip=%v recorded=%v", strip, record.TurnStateStripped)
		}
		if got := record.AfterHeaders.Get(turnStateHeader); (got == "") != strip {
			t.Fatalf("strip=%v after-header=%q", strip, got)
		}
	}
}

func TestOwnTurnStateEchoIsNotFlagged(t *testing.T) {
	stubCredentialPlan(t, "team")
	p := resetState(t)
	resetTurnStates(t)
	state.mu.Lock()
	state.credentials["idx-a"] = credentialSnapshot{AuthIndex: "idx-a", Provider: "codex", Name: "a.json", PlanType: "team", PlanResolved: true}
	state.rules["idx-a"] = headerRule{AuthIndex: "idx-a", Enabled: true, InjectTurnState: true}
	state.mu.Unlock()
	blob := fernetToken(0x80, time.Now(), 1)
	if _, err := interceptAfter(requestInterceptRequest{RequestID: "t1", Metadata: map[string]any{"selected_auth_index": "idx-a"}}); err != nil {
		t.Fatal(err)
	}
	observeStreamHeaders(streamChunkInterceptRequest{RequestID: "t1", ChunkIndex: streamChunkHeaderInitIndex, ResponseHeaders: http.Header{turnStateHeader: {blob}}})
	completeRequest(requestCompletion{RequestID: "t1", Outcome: "succeeded", StatusCode: 200, CompletedAt: time.Now()})

	resp, err := interceptAfter(requestInterceptRequest{RequestID: "t2", Headers: http.Header{turnStateHeader: {blob}}, Metadata: map[string]any{"selected_auth_index": "idx-a"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range resp.ClearHeaders {
		if strings.EqualFold(name, turnStateHeader) {
			t.Fatal("a credential echoing its own blob must not have it stripped")
		}
	}
	completeRequest(requestCompletion{RequestID: "t2", Outcome: "succeeded", StatusCode: 200, CompletedAt: time.Now()})
	state.mu.Lock()
	writer := state.writer
	state.writer = nil
	state.store = nil
	state.mu.Unlock()
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	page, _ := p.History("idx-a", 1, pageSize)
	if len(page.Items) != 2 {
		t.Fatalf("history=%#v", page.Items)
	}
	latest := page.Items[0]
	if latest.TurnStateCrossAccount == nil || *latest.TurnStateCrossAccount {
		t.Fatalf("own echo should be an explicit match: %#v", latest.TurnStateCrossAccount)
	}
	if latest.TurnStateStripped {
		t.Fatal("nothing should have been stripped")
	}
}

func TestStatePoolInjectsOnlyForMatchingCredentialAndModel(t *testing.T) {
	stubCredentialPlan(t, "team")
	resetState(t)
	resetTurnStates(t)
	state.mu.Lock()
	state.credentials["idx-a"] = credentialSnapshot{AuthIndex: "idx-a", AuthID: "auth-a", Provider: "codex", Name: "a.json"}
	state.credentials["idx-b"] = credentialSnapshot{AuthIndex: "idx-b", AuthID: "auth-b", Provider: "codex", Name: "b.json"}
	state.rules["idx-a"] = headerRule{AuthIndex: "idx-a", Enabled: true, InjectTurnState: true}
	state.rules["idx-b"] = headerRule{AuthIndex: "idx-b", Enabled: true, InjectTurnState: true}
	state.mu.Unlock()
	blob := fernetToken(0x80, time.Now(), 1)
	state.mu.Lock()
	if !noteTurnStateMintLocked(blob, "idx-a", "A", "gpt-5.6-luna", "team", "") {
		state.mu.Unlock()
		t.Fatal("fixture state did not enter the pool")
	}
	state.mu.Unlock()

	hit, err := interceptAfter(requestInterceptRequest{RequestID: "hit", Model: "gpt-5.6-luna", Metadata: map[string]any{"selected_auth_index": "idx-a", "selected_auth_id": "auth-a"}})
	if err != nil || hit.Headers.Get(turnStateHeader) != blob {
		t.Fatalf("matching request headers=%#v err=%v", hit.Headers, err)
	}
	wrongCredential, err := interceptAfter(requestInterceptRequest{RequestID: "wrong-credential", Model: "gpt-5.6-luna", Metadata: map[string]any{"selected_auth_index": "idx-b", "selected_auth_id": "auth-b"}})
	if err != nil || wrongCredential.Headers.Get(turnStateHeader) != "" {
		t.Fatalf("other credential received state: headers=%#v err=%v", wrongCredential.Headers, err)
	}
	wrongModel, err := interceptAfter(requestInterceptRequest{RequestID: "wrong-model", Model: "gpt-5.6-sol", Metadata: map[string]any{"selected_auth_index": "idx-a", "selected_auth_id": "auth-a"}})
	if err != nil || wrongModel.Headers.Get(turnStateHeader) != "" {
		t.Fatalf("other model received state: headers=%#v err=%v", wrongModel.Headers, err)
	}
}

func TestStatePoolInjectionReplacesAConflictingClientState(t *testing.T) {
	stubCredentialPlan(t, "team")
	resetState(t)
	resetTurnStates(t)
	state.mu.Lock()
	state.credentials["idx-a"] = credentialSnapshot{AuthIndex: "idx-a", AuthID: "auth-a", Provider: "codex", Name: "a.json"}
	state.rules["idx-a"] = headerRule{AuthIndex: "idx-a", Enabled: true, InjectTurnState: true}
	state.mu.Unlock()
	pooled := fernetToken(0x80, time.Now(), 1)
	client := fernetToken(0x80, time.Now().Add(-time.Minute), 1)
	state.mu.Lock()
	noteTurnStateMintLocked(pooled, "idx-a", "A", "gpt-5.6-luna", "team", "")
	state.mu.Unlock()

	response, err := interceptAfter(requestInterceptRequest{
		RequestID: "replace", Model: "gpt-5.6-luna",
		Headers:  http.Header{turnStateHeader: {client}},
		Metadata: map[string]any{"selected_auth_index": "idx-a", "selected_auth_id": "auth-a"},
	})
	if err != nil || response.Headers.Get(turnStateHeader) != pooled {
		t.Fatalf("pooled state did not replace client state: response=%#v err=%v", response, err)
	}
	state.mu.Lock()
	attempt := state.pending["replace"].current
	state.mu.Unlock()
	if attempt.BeforeHeaders.Get(turnStateHeader) != client || attempt.AfterHeaders.Get(turnStateHeader) != pooled {
		t.Fatalf("history did not preserve replacement: before=%q after=%q", attempt.BeforeHeaders.Get(turnStateHeader), attempt.AfterHeaders.Get(turnStateHeader))
	}
}

func TestNonCodexIgnored(t *testing.T) {
	resetState(t)
	state.mu.Lock()
	state.credentials["idx-x"] = credentialSnapshot{AuthIndex: "idx-x", Provider: "xai", Type: "codex"}
	state.mu.Unlock()
	resp, err := interceptAfter(requestInterceptRequest{RequestID: "x", Metadata: map[string]any{"selected_auth_index": "idx-x"}})
	if err != nil || len(resp.Headers) != 0 {
		t.Fatalf("resp=%#v err=%v", resp, err)
	}
	state.mu.Lock()
	pr := state.pending["x"]
	state.mu.Unlock()
	if pr == nil || pr.current != nil {
		t.Fatalf("non-codex current=%#v", pr)
	}
}

// poolFixture arms the pool for one credential and model, and returns the blob.
func poolFixture(t *testing.T, rule headerRule) string {
	t.Helper()
	blob := fernetToken(0x80, time.Now(), 1)
	state.mu.Lock()
	state.credentials["idx-a"] = credentialSnapshot{AuthIndex: "idx-a", AuthID: "auth-a", Provider: "codex", Name: "a.json"}
	if rule.AuthIndex != "" {
		state.rules["idx-a"] = rule
	}
	ok := noteTurnStateMintLocked(blob, "idx-a", "A", "gpt-5.6-luna", "team", "")
	state.mu.Unlock()
	if !ok {
		t.Fatal("fixture state did not enter the pool")
	}
	return blob
}

func injectTestRequest(t *testing.T, id string, headers http.Header) requestInterceptResponse {
	t.Helper()
	response, err := interceptAfter(requestInterceptRequest{RequestID: id, Model: "gpt-5.6-luna", Headers: headers,
		Metadata: map[string]any{"selected_auth_index": "idx-a", "selected_auth_id": "auth-a"}})
	if err != nil {
		t.Fatal(err)
	}
	return response
}

// Injection is the pool's own feature, not part of header rewriting: the rule
// switch being off leaves Set and Remove unapplied and nothing else.
func TestPoolInjectsEvenWhileTheRuleIsOff(t *testing.T) {
	stubCredentialPlan(t, "team")
	resetState(t)
	resetTurnStates(t)
	blob := poolFixture(t, headerRule{AuthIndex: "idx-a", Enabled: false, InjectTurnState: true, Set: map[string]string{"X-Team": "A"}})
	response := injectTestRequest(t, "rule-off", nil)
	if got := response.Headers.Get(turnStateHeader); got != blob {
		t.Fatalf("a disabled rule must not hold injection back: %q", got)
	}
	if response.Headers.Get("X-Team") != "" {
		t.Fatal("the disabled rule's own headers must stay unapplied")
	}
	state.mu.Lock()
	attempt := state.pending["rule-off"].current
	state.mu.Unlock()
	if !attempt.TurnStateInjected {
		t.Fatal("history must show the injection")
	}
}

// A pinned value only pins while the rule that pins it is on; a disabled rule
// is inert all the way down, so it does not hold the pool back either.
func TestDisabledRulePinsNothing(t *testing.T) {
	stubCredentialPlan(t, "team")
	resetState(t)
	resetTurnStates(t)
	blob := poolFixture(t, headerRule{AuthIndex: "idx-a", Enabled: false, InjectTurnState: true, Set: map[string]string{"x-codex-turn-state": "operator-pinned"}})
	if got := injectTestRequest(t, "inert-pin", nil).Headers.Get(turnStateHeader); got != blob {
		t.Fatalf("a pin in a disabled rule is inert; want the pooled state, got %q", got)
	}
}

// The pool's master switch outranks the injection switch: frozen, the pool
// neither hands anything out nor takes anything in, though history still
// records what the upstream sent.
func TestFrozenPoolNeitherInjectsNorMints(t *testing.T) {
	stubCredentialPlan(t, "team")
	resetState(t)
	resetTurnStates(t)
	paused := false
	poolFixture(t, headerRule{AuthIndex: "idx-a", Enabled: true, InjectTurnState: true, MaintainStatePool: &paused})
	if got := injectTestRequest(t, "frozen", nil).Headers.Get(turnStateHeader); got != "" {
		t.Fatalf("a frozen pool must not inject: %q", got)
	}
	fresh := fernetToken(0x80, time.Now(), 5)
	observeResponse(responseInterceptRequest{RequestID: "frozen", StatusCode: 200, ResponseHeaders: http.Header{turnStateHeader: {fresh}}})
	state.mu.Lock()
	attempt := state.pending["frozen"].current
	_, pooled := lookupTurnStateOriginLocked(fresh)
	state.mu.Unlock()
	if attempt.TurnStateMinted == nil || !attempt.TurnStateMinted.FernetLike || attempt.TurnStateMinted.PlanType != "team" {
		t.Fatalf("the response state must still be classified for history: %+v", attempt.TurnStateMinted)
	}
	if attempt.TurnStateMinted.Pooled || pooled {
		t.Fatal("a frozen pool must not take the state in")
	}
	if attempt.TurnStateInjected {
		t.Fatal("history claimed an injection that did not happen")
	}
}

func TestPoolDoesNotInjectWithoutAnyRule(t *testing.T) {
	stubCredentialPlan(t, "team")
	resetState(t)
	resetTurnStates(t)
	poolFixture(t, headerRule{})
	if got := injectTestRequest(t, "no-rule", nil).Headers.Get(turnStateHeader); got != "" {
		t.Fatalf("a credential with no rule must not inject: %q", got)
	}
}

func TestEnabledRuleInjectsAndRecordsIt(t *testing.T) {
	stubCredentialPlan(t, "team")
	resetState(t)
	resetTurnStates(t)
	blob := poolFixture(t, headerRule{AuthIndex: "idx-a", Enabled: true, InjectTurnState: true})
	if got := injectTestRequest(t, "rule-on", nil).Headers.Get(turnStateHeader); got != blob {
		t.Fatalf("headers=%q want the pooled state", got)
	}
	state.mu.Lock()
	attempt := state.pending["rule-on"].current
	state.mu.Unlock()
	if !attempt.TurnStateInjected {
		t.Fatal("an injection has to be visible in history")
	}
	if attempt.AfterHeaders.Get(turnStateHeader) != blob {
		t.Fatalf("after view=%q", attempt.AfterHeaders.Get(turnStateHeader))
	}
}

// An operator who pinned this header by hand outranks the pool.
func TestRuleSetOfTheStateHeaderWinsOverInjection(t *testing.T) {
	stubCredentialPlan(t, "team")
	resetState(t)
	resetTurnStates(t)
	poolFixture(t, headerRule{AuthIndex: "idx-a", Enabled: true, Set: map[string]string{"x-codex-turn-state": "operator-pinned"}})
	response := injectTestRequest(t, "pinned", nil)
	if got := response.Headers.Get(turnStateHeader); got != "operator-pinned" {
		t.Fatalf("the operator's own value must survive: %q", got)
	}
	state.mu.Lock()
	attempt := state.pending["pinned"].current
	state.mu.Unlock()
	if attempt.TurnStateInjected {
		t.Fatal("a pinned value is not an injection")
	}
}

// And an operator who removed it wants it gone, not refilled from the pool.
func TestRuleRemovalOfTheStateHeaderIsNotRefilled(t *testing.T) {
	stubCredentialPlan(t, "team")
	resetState(t)
	resetTurnStates(t)
	client := fernetToken(0x80, time.Now().Add(-time.Minute), 1)
	poolFixture(t, headerRule{AuthIndex: "idx-a", Enabled: true, Remove: []string{"X-Codex-Turn-State"}})
	response := injectTestRequest(t, "removed", http.Header{turnStateHeader: {client}})
	if got := response.Headers.Get(turnStateHeader); got != "" {
		t.Fatalf("a removed header must not be refilled: %q", got)
	}
	cleared := false
	for _, name := range response.ClearHeaders {
		if strings.EqualFold(name, turnStateHeader) {
			cleared = true
		}
	}
	if !cleared {
		t.Fatalf("the removal must still reach the host: %#v", response.ClearHeaders)
	}
	state.mu.Lock()
	attempt := state.pending["removed"].current
	state.mu.Unlock()
	if attempt.AfterHeaders.Get(turnStateHeader) != "" || attempt.TurnStateInjected {
		t.Fatalf("after view=%q injected=%v", attempt.AfterHeaders.Get(turnStateHeader), attempt.TurnStateInjected)
	}
}

// The guard and the pool work together: an unusable echo is replaced rather
// pooledAt is poolFixture with the mint time chosen by the test, so a state can
// be placed inside or past the reuse window.
func pooledAt(t *testing.T, issued time.Time) string {
	t.Helper()
	blob := fernetToken(0x80, issued, 1)
	state.mu.Lock()
	state.credentials["idx-a"] = credentialSnapshot{AuthIndex: "idx-a", AuthID: "auth-a", Provider: "codex", Name: "a.json"}
	state.rules["idx-a"] = headerRule{AuthIndex: "idx-a", Enabled: true, InjectTurnState: true}
	ok := noteTurnStateMintLocked(blob, "idx-a", "A", "gpt-5.6-luna", "team", "")
	state.mu.Unlock()
	if !ok {
		t.Fatal("fixture state did not enter the pool")
	}
	return blob
}

func pooledFor(t *testing.T) (turnStateOrigin, bool) {
	t.Helper()
	state.mu.Lock()
	defer state.mu.Unlock()
	origin, ok := state.turnStateLatest[turnStateLatestKey("idx-a", "gpt-5.6-luna")]
	return origin, ok
}

// lastAttempt drains the history writer, the way the persistence tests do,
// then reads the newest record back from the store.
func lastAttempt(t *testing.T) historyRecord {
	t.Helper()
	state.mu.Lock()
	writer := state.writer
	state.writer = nil
	state.mu.Unlock()
	if writer != nil {
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
	}
	page, err := state.store.History("idx-a", 1, pageSize)
	if err != nil || len(page.Items) == 0 {
		t.Fatalf("history: %v items=%d", err, len(page.Items))
	}
	return page.Items[0]
}

// An expired pooled state that went out and came back degraded has stopped
// carrying the chain. It leaves the pool -- memory and store -- and the
// attempt says so, so the next request is not handed the same dead state.
func TestExpiredInjectedStateIsInvalidatedWhenTheResponseIsDegraded(t *testing.T) {
	stubCredentialPlan(t, "team")
	resetState(t)
	resetTurnStates(t)
	stale := pooledAt(t, time.Now().Add(-2*time.Hour))
	response := injectTestRequest(t, "stale", nil)
	if response.Headers.Get(turnStateHeader) != stale {
		t.Fatal("the expired pooled state should still have been injected")
	}
	degraded := fernetToken(0x80, time.Now(), 40) // far past the team limit
	observeResponse(responseInterceptRequest{RequestID: "stale", StatusCode: 200, ResponseHeaders: http.Header{turnStateHeader: {degraded}}})
	completeRequest(requestCompletion{RequestID: "stale", Outcome: "succeeded", StatusCode: 200, CompletedAt: time.Now()})

	if _, ok := pooledFor(t); ok {
		t.Fatal("the invalidated state is still in the pool")
	}
	persisted, err := state.store.ListTurnStates()
	if err != nil || len(persisted) != 0 {
		t.Fatalf("the invalidated state is still persisted: %v %#v", err, persisted)
	}
	if got := lastAttempt(t); !got.TurnStateInvalidated || !got.TurnStateInjected {
		t.Fatalf("attempt should record injection and invalidation: %#v", got)
	}
	if next := injectTestRequest(t, "after", nil); next.Headers.Get(turnStateHeader) != "" {
		t.Fatal("a dead state was injected again")
	}
}

// Age is not part of it: a state inside the window that went out and came
// back degraded is just as dead as an expired one, and leaves the pool.
func TestFreshInjectedStateIsEvictedByADegradedResponse(t *testing.T) {
	stubCredentialPlan(t, "team")
	resetState(t)
	resetTurnStates(t)
	pooledAt(t, time.Now().Add(-defaultStateTTLSeconds*time.Second/2))
	injectTestRequest(t, "fresh", nil)
	observeResponse(responseInterceptRequest{RequestID: "fresh", StatusCode: 200, ResponseHeaders: http.Header{turnStateHeader: {fernetToken(0x80, time.Now(), 40)}}})
	completeRequest(requestCompletion{RequestID: "fresh", Outcome: "succeeded", StatusCode: 200, CompletedAt: time.Now()})
	if _, ok := pooledFor(t); ok {
		t.Fatal("a state that produced a degraded turn must leave the pool, however fresh")
	}
	if got := lastAttempt(t); !got.TurnStateInvalidated {
		t.Fatalf("attempt should record the eviction: %#v", got)
	}
}

// The live rule reads the exchange, not the state: a turn that went out with
// an injected state and came back with one of its own is degraded however
// ordinary that state looks, and the injected one leaves the pool.
func TestInjectedTurnThatComesBackWithAStateIsDegraded(t *testing.T) {
	stubCredentialPlan(t, "team")
	resetState(t)
	resetTurnStates(t)
	injected := poolFixture(t, headerRule{AuthIndex: "idx-a", InjectTurnState: true})
	if got := injectTestRequest(t, "renew", nil).Headers.Get(turnStateHeader); got != injected {
		t.Fatalf("the pooled state should have gone out, got %q", got)
	}
	fresh := fernetToken(0x80, time.Now(), 1)
	observeResponse(responseInterceptRequest{RequestID: "renew", StatusCode: 200, ResponseHeaders: http.Header{turnStateHeader: {fresh}}})
	completeRequest(requestCompletion{RequestID: "renew", Outcome: "succeeded", StatusCode: 200, CompletedAt: time.Now()})
	if _, pooled := pooledFor(t); pooled {
		t.Fatal("the injected state is evicted and the returned one is not pooled")
	}
	got := lastAttempt(t)
	info := got.TurnStateMinted
	if info == nil || info.Judgement != degradedRuleInjected || info.NonDegraded == nil || *info.NonDegraded || info.Pooled || info.ManualPool {
		t.Fatalf("the returned state should read as degraded: %+v", info)
	}
	if !got.TurnStateInvalidated || got.TurnStateHeld {
		t.Fatalf("the injected state should be recorded as invalidated: %#v", got)
	}
}

// The healthy live turn: a state went out, the upstream wrote none back. There
// is nothing to pool and nothing to evict, and the row says the turn held.
func TestInjectedTurnThatComesBackSilentIsHealthy(t *testing.T) {
	stubCredentialPlan(t, "team")
	resetState(t)
	resetTurnStates(t)
	injected := poolFixture(t, headerRule{AuthIndex: "idx-a", InjectTurnState: true})
	injectTestRequest(t, "quiet", nil)
	observeResponse(responseInterceptRequest{RequestID: "quiet", StatusCode: 200, ResponseHeaders: http.Header{"X-Request-Id": {"req_1"}}})
	completeRequest(requestCompletion{RequestID: "quiet", Outcome: "succeeded", StatusCode: 200, CompletedAt: time.Now()})
	if origin, pooled := pooledFor(t); !pooled || origin.blob != injected {
		t.Fatal("the injected state stays in the pool")
	}
	got := lastAttempt(t)
	if !got.TurnStateHeld || got.TurnStateMinted != nil || got.TurnStateInvalidated || got.TurnStateRejected {
		t.Fatalf("a silent response is the healthy case: %#v", got)
	}
}

func rejectFixture(t *testing.T, reject bool) {
	t.Helper()
	stubCredentialPlan(t, "team")
	resetState(t)
	resetTurnStates(t)
	state.mu.Lock()
	state.credentials["idx-a"] = credentialSnapshot{AuthIndex: "idx-a", AuthID: "auth-a", Provider: "codex", Name: "a.json"}
	state.rules["idx-a"] = headerRule{AuthIndex: "idx-a", Enabled: true, InjectTurnState: true, RejectDegradedResponse: reject}
	// Under the live rule a response is degraded when it writes a state back
	// over one the plugin injected, so the request goes out carrying one.
	noteTurnStateMintLocked(fernetToken(0x80, time.Now(), 1), "idx-a", "A", "gpt-5.6-luna", "team", "")
	state.mu.Unlock()
	if _, err := interceptAfter(requestInterceptRequest{RequestID: "r", Model: "gpt-5.6-luna", Metadata: map[string]any{"selected_auth_index": "idx-a", "selected_auth_id": "auth-a"}}); err != nil {
		t.Fatal(err)
	}
}

// With the flag on, a non-stream response whose minted state is degraded is
// replaced by an error object and marked, and the attempt records it.
func TestDegradedNonStreamResponseIsWithheldWhenTheRuleAsks(t *testing.T) {
	rejectFixture(t, true)
	degraded := fernetToken(0x80, time.Now(), 40)
	out := observeResponse(responseInterceptRequest{RequestID: "r", StatusCode: 200, ResponseHeaders: http.Header{turnStateHeader: {degraded}}, Body: []byte(`{"id":"resp"}`)})
	if !strings.Contains(string(out.Body), `"turn_state_degraded"`) || out.Headers.Get("X-Codex-Header-Rewrite") != "rejected-degraded-turn-state" {
		t.Fatalf("response not withheld: %s %v", out.Body, out.Headers)
	}
	completeRequest(requestCompletion{RequestID: "r", Outcome: "succeeded", StatusCode: 200, CompletedAt: time.Now()})
	if got := lastAttempt(t); !got.TurnStateRejected {
		t.Fatalf("attempt should record the rejection: %#v", got)
	}
}

// On a stream the decision is taken on the header chunk: the first payload
// chunk becomes a terminal error event and every later chunk is dropped.
func TestDegradedStreamIsCutAtTheFirstChunk(t *testing.T) {
	rejectFixture(t, true)
	degraded := fernetToken(0x80, time.Now(), 40)
	head := observeStreamHeaders(streamChunkInterceptRequest{RequestID: "r", ChunkIndex: streamChunkHeaderInitIndex, ResponseHeaders: http.Header{turnStateHeader: {degraded}}})
	if head.Headers.Get("X-Codex-Header-Rewrite") == "" || head.DropChunk {
		t.Fatalf("header chunk should only be marked: %#v", head)
	}
	first := observeStreamHeaders(streamChunkInterceptRequest{RequestID: "r", ChunkIndex: 0, Body: []byte("data: {\"type\":\"response.created\"}\n\n")})
	if !strings.HasPrefix(string(first.Body), "event: error\n") || !strings.Contains(string(first.Body), `"turn_state_degraded"`) || first.DropChunk {
		t.Fatalf("first chunk should carry the error event: %#v", first)
	}
	second := observeStreamHeaders(streamChunkInterceptRequest{RequestID: "r", ChunkIndex: 1, Body: []byte("data: {\"type\":\"response.output_text.delta\"}\n\n")})
	if !second.DropChunk || len(second.Body) != 0 {
		t.Fatalf("later chunks should be dropped: %#v", second)
	}
}

func TestDegradedModelScopeForBothResponsePaths(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, tc := range []struct {
			name   string
			models []string
			want   bool
		}{
			{"all", nil, true},
			{"match", []string{"gpt-5.6-luna"}, true},
			{"other", []string{"gpt-6-astra"}, false},
			{"no-prefix-match", []string{"gpt-5.6"}, false},
		} {
			t.Run(fmt.Sprintf("%s/stream=%v", tc.name, stream), func(t *testing.T) {
				rejectFixture(t, true)
				state.mu.Lock()
				rule := state.rules["idx-a"]
				rule.Enabled = false
				rule.RejectDegradedModels = tc.models
				rule.RetryOnDegraded = !tc.want
				state.rules["idx-a"] = rule
				state.mu.Unlock()
				headers := http.Header{turnStateHeader: {fernetToken(0x80, time.Now(), 40)}}
				var blocked bool
				if stream {
					observeStreamHeaders(streamChunkInterceptRequest{RequestID: "r", ChunkIndex: streamChunkHeaderInitIndex, ResponseHeaders: headers})
					out := observeStreamHeaders(streamChunkInterceptRequest{RequestID: "r", ChunkIndex: 0, Body: []byte("data: {}\n\n")})
					blocked = len(out.Body) > 0
				} else {
					blocked = len(observeResponse(responseInterceptRequest{RequestID: "r", ResponseHeaders: headers, StatusCode: 200}).Body) > 0
				}
				if blocked != tc.want {
					t.Fatalf("blocked=%v want=%v", blocked, tc.want)
				}
				state.mu.Lock()
				scheduled := state.pending["r"].current.retryScheduled
				state.mu.Unlock()
				if scheduled {
					t.Fatal("unmatched model triggered retries")
				}
			})
		}
	}
}

func TestModelScopePersistsAndEmptyMeansAll(t *testing.T) {
	p := resetState(t)
	rule, err := saveRule(headerRule{AuthIndex: "a", RejectDegradedResponse: true, RejectDegradedModels: []string{" gpt-5.6-luna ", "", "gpt-5.6-luna"}})
	if err != nil {
		t.Fatal(err)
	}
	got, ok, err := p.GetRule("a")
	if err != nil || !ok || len(got.RejectDegradedModels) != 1 || !rejectsDegradedModel(got, "gpt-5.6-luna") || rejectsDegradedModel(got, "gpt-6-astra") {
		t.Fatalf("stored rule=%#v err=%v", got, err)
	}
	rule.RejectDegradedModels = []string{"  "}
	got, err = saveRule(rule)
	if err != nil || !rejectsDegradedModel(got, "gpt-6-astra") {
		t.Fatalf("empty scope=%#v err=%v", got, err)
	}
}

// The flag off, or a turn the rule does not call degraded, leaves the
// response alone.
func TestResponsesPassThroughWithoutTheFlagOrWithoutDegradation(t *testing.T) {
	rejectFixture(t, false)
	returned := fernetToken(0x80, time.Now(), 40)
	if out := observeResponse(responseInterceptRequest{RequestID: "r", StatusCode: 200, ResponseHeaders: http.Header{turnStateHeader: {returned}}}); len(out.Body) != 0 || len(out.Headers) != 0 {
		t.Fatalf("flag off must pass through: %#v", out)
	}
	// Flag on, but the upstream wrote no state back over the injected one,
	// which is the healthy turn.
	rejectFixture(t, true)
	if out := observeResponse(responseInterceptRequest{RequestID: "r", StatusCode: 200, ResponseHeaders: http.Header{"X-Request-Id": {"req_1"}}}); len(out.Body) != 0 || len(out.Headers) != 0 {
		t.Fatalf("a healthy turn must pass through: %#v", out)
	}
	if chunk := observeStreamHeaders(streamChunkInterceptRequest{RequestID: "r", ChunkIndex: 0, Body: []byte("data: x\n\n")}); chunk.DropChunk || len(chunk.Body) != 0 {
		t.Fatalf("chunks of an accepted stream must pass through: %#v", chunk)
	}
}

// The switch decides whether the plugin supplies this header at all. With it
// off the client's request goes upstream exactly as it arrived, pool or no
// pool -- that is the whole of the rule.
func TestNothingIsInjectedWhenTheSwitchIsOff(t *testing.T) {
	stubCredentialPlan(t, "team")
	resetState(t)
	resetTurnStates(t)
	foreign := fernetToken(0x80, time.Now(), 3)
	state.mu.Lock()
	state.credentials["idx-b"] = credentialSnapshot{AuthIndex: "idx-b", AuthID: "auth-b", Provider: "codex", Name: "b.json"}
	noteTurnStateMintLocked(foreign, "idx-b", "B", "gpt-5.6-luna", "team", "")
	state.mu.Unlock()
	poolFixture(t, headerRule{AuthIndex: "idx-a", Enabled: true, InjectTurnState: false})

	response := injectTestRequest(t, "switch-off", http.Header{turnStateHeader: {foreign}})
	if got := response.Headers.Get(turnStateHeader); got != "" {
		t.Fatalf("the switch is off, nothing should be injected: %q", got)
	}
	for _, name := range response.ClearHeaders {
		if strings.EqualFold(name, turnStateHeader) {
			t.Fatal("the switch is off, nothing should be cleared either")
		}
	}
}

// than merely dropped, and the response never both sets and clears one header.
func TestGuardStripAndInjectionDoNotContradictEachOther(t *testing.T) {
	stubCredentialPlan(t, "team")
	resetState(t)
	resetTurnStates(t)
	foreign := fernetToken(0x80, time.Now(), 2)
	state.mu.Lock()
	state.credentials["idx-b"] = credentialSnapshot{AuthIndex: "idx-b", AuthID: "auth-b", Provider: "codex", Name: "b.json"}
	noteTurnStateMintLocked(foreign, "idx-b", "B", "gpt-5.6-luna", "team", "")
	state.mu.Unlock()
	pooled := poolFixture(t, headerRule{AuthIndex: "idx-a", Enabled: true, InjectTurnState: true})

	response := injectTestRequest(t, "replace-foreign", http.Header{turnStateHeader: {foreign}})
	if got := response.Headers.Get(turnStateHeader); got != pooled {
		t.Fatalf("the foreign echo should be replaced by the pooled state: %q", got)
	}
	for _, name := range response.ClearHeaders {
		if strings.EqualFold(name, turnStateHeader) {
			t.Fatal("one response must not both set and clear the same header")
		}
	}
}

// A rule stored before the switch became an injection control carries its
// value under the old key; loading it must not silently turn injection off.
func TestLegacyGuardKeyBecomesTheInjectionSwitch(t *testing.T) {
	migrated, err := validateRule(headerRule{AuthIndex: "idx-a", LegacyGuard: true})
	if err != nil {
		t.Fatal(err)
	}
	if !migrated.InjectTurnState || migrated.LegacyGuard {
		t.Fatalf("the old key should fold into the new switch: %#v", migrated)
	}
}

// End to end: the session pooled with a state is the request's cookie updated
// by the minting response's Set-Cookie, so a response that rotates the session
// pools the state under the new one.
func TestPooledSessionFollowsTheRotatingResponse(t *testing.T) {
	stubCredentialPlan(t, "team")
	resetState(t)
	resetTurnStates(t)
	blob := fernetToken(0x80, time.Now(), 1)
	state.mu.Lock()
	state.credentials["idx-a"] = credentialSnapshot{AuthIndex: "idx-a", AuthID: "auth-a", Provider: "codex", Name: "a.json"}
	state.mu.Unlock()

	injectTestRequest(t, "r", http.Header{"Cookie": {"session=old; oai-did=device"}})
	observeResponse(responseInterceptRequest{RequestID: "r", StatusCode: 200, ResponseHeaders: http.Header{
		turnStateHeader: {blob},
		"Set-Cookie":    {"session=rotated; Path=/; HttpOnly", "issued=fresh; Secure"},
	}})
	completeRequest(requestCompletion{RequestID: "r", Outcome: "succeeded", StatusCode: 200, CompletedAt: time.Now()})

	// Nothing was injected, so the state waits for a manual pool; the session
	// is recorded with it either way and is what a manual pool would take.
	state.mu.Lock()
	origin, ok := lookupTurnStateOriginLocked(blob)
	state.mu.Unlock()
	if !ok {
		t.Fatal("the state should have been recorded")
	}
	const want = "session=rotated; oai-did=device; issued=fresh"
	if origin.cookie != want {
		t.Fatalf("recorded session=%q want %q", origin.cookie, want)
	}
}

// Paused, the judgement vouches for nothing a live request brings back: the
// state is recorded -- its provenance still feeds cross-account detection --
// but it does not pool itself, and the history row says it waits for a hand.
// Nothing is intercepted or evicted either, since there is no verdict.
func TestLiveStateWithoutInjectionWaitsForAManualPool(t *testing.T) {
	stubCredentialPlan(t, "team")
	resetState(t)
	resetTurnStates(t)
	state.mu.Lock()
	state.credentials["idx-a"] = credentialSnapshot{AuthIndex: "idx-a", AuthID: "auth-a", Provider: "codex", Name: "a.json"}
	state.rules["idx-a"] = headerRule{AuthIndex: "idx-a", Enabled: true, InjectTurnState: true, RejectDegradedResponse: true}
	state.mu.Unlock()
	long := fernetToken(0x80, time.Now(), 40) // far past the old team limit
	response := injectTestRequest(t, "paused", http.Header{"Cookie": {"sid=one"}})
	if response.Headers.Get(turnStateHeader) != "" {
		t.Fatal("nothing pooled yet, nothing to inject")
	}
	observeResponse(responseInterceptRequest{RequestID: "paused", StatusCode: 200, ResponseHeaders: http.Header{
		turnStateHeader: {long}, "Set-Cookie": {"cf=two; Path=/; HttpOnly"}}})
	state.mu.Lock()
	attempt := *state.pending["paused"].current
	state.mu.Unlock()
	completeRequest(requestCompletion{RequestID: "paused", Outcome: "succeeded", StatusCode: 200, CompletedAt: time.Now()})
	if _, pooled := pooledFor(t); pooled {
		t.Fatal("a live state must not pool itself while the judgement is paused")
	}
	if attempt.TurnStateRejected || attempt.TurnStateInvalidated {
		t.Fatalf("nothing is withheld or evicted without a verdict: %+v", attempt.historyRecord)
	}
	if info := attempt.TurnStateMinted; info == nil || info.Pooled || !info.ManualPool || info.NonDegraded != nil || info.Judgement != degradedRuleInjected {
		t.Fatalf("history should say the state waits for a manual pool, with no verdict: %+v", info)
	}
	state.mu.Lock()
	origin, known := lookupTurnStateOriginLocked(long)
	state.mu.Unlock()
	if !known || origin.authIndex != "idx-a" {
		t.Fatal("the state's provenance is still recorded, for cross-account detection")
	}
	if got := injectTestRequest(t, "next", nil).Headers.Get(turnStateHeader); got != "" {
		t.Fatalf("an unpooled live state must not be injected, got %q", got)
	}

	// The operator pools it from history. The session is rebuilt from the
	// recorded Cookie and Set-Cookie, the same merge the live path makes.
	record := lastAttempt(t)
	result, err := poolFromHistory("idx-a", record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Info.Pooled || !result.Info.ManualPool || result.ReplacedNewer {
		t.Fatalf("manual pool result: %+v", result)
	}
	pooled, ok := pooledFor(t)
	if !ok || pooled.blob != long || pooled.cookie != "sid=one; cf=two" {
		t.Fatalf("pooled %v: blob match=%v cookie=%q", ok, pooled.blob == long, pooled.cookie)
	}
	page, _ := state.store.History("idx-a", 1, pageSize)
	if minted := page.Items[0].TurnStateMinted; minted == nil || !minted.Pooled || !minted.ManualPool {
		t.Fatalf("the history row should now read pooled by hand: %+v", minted)
	}
	if got := injectTestRequest(t, "after-manual", nil).Headers.Get(turnStateHeader); got != long {
		t.Fatalf("the manually pooled state should go out on the next request, got %q", got)
	}
}

// A manual pool is the operator's pick, so it takes the slot even from a
// newer state; the result says so, so the panel can too.
func TestManualPoolTakesTheSlotFromANewerState(t *testing.T) {
	stubCredentialPlan(t, "team")
	resetState(t)
	resetTurnStates(t)
	older := fernetToken(0x80, time.Now().Add(-10*time.Minute), 2)
	state.mu.Lock()
	state.credentials["idx-a"] = credentialSnapshot{AuthIndex: "idx-a", AuthID: "auth-a", Provider: "codex", Name: "a.json"}
	state.rules["idx-a"] = headerRule{AuthIndex: "idx-a", Enabled: true}
	state.mu.Unlock()
	injectTestRequest(t, "old", nil)
	observeResponse(responseInterceptRequest{RequestID: "old", StatusCode: 200, ResponseHeaders: http.Header{turnStateHeader: {older}}})
	completeRequest(requestCompletion{RequestID: "old", Outcome: "succeeded", StatusCode: 200, CompletedAt: time.Now()})
	newer := poolFixture(t, headerRule{})
	record := lastAttempt(t)
	result, err := poolFromHistory("idx-a", record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !result.ReplacedNewer {
		t.Fatal("the result should say a newer state was replaced")
	}
	if origin, _ := pooledFor(t); origin.blob != older || origin.blob == newer {
		t.Fatal("the manual pick should hold the slot")
	}
}

// Frozen, the pool takes nothing in by hand either; unknown rows are refused.
func TestManualPoolRefusals(t *testing.T) {
	stubCredentialPlan(t, "team")
	resetState(t)
	resetTurnStates(t)
	state.mu.Lock()
	state.credentials["idx-a"] = credentialSnapshot{AuthIndex: "idx-a", AuthID: "auth-a", Provider: "codex", Name: "a.json"}
	state.rules["idx-a"] = headerRule{AuthIndex: "idx-a", Enabled: true}
	state.mu.Unlock()
	injectTestRequest(t, "live", nil)
	observeResponse(responseInterceptRequest{RequestID: "live", StatusCode: 200, ResponseHeaders: http.Header{turnStateHeader: {fernetToken(0x80, time.Now(), 2)}}})
	completeRequest(requestCompletion{RequestID: "live", Outcome: "succeeded", StatusCode: 200, CompletedAt: time.Now()})
	record := lastAttempt(t)

	off := false
	state.mu.Lock()
	state.rules["idx-a"] = headerRule{AuthIndex: "idx-a", Enabled: true, MaintainStatePool: &off}
	state.mu.Unlock()
	statusOf := func(err error) int {
		var failure *manualPoolError
		if errors.As(err, &failure) {
			return failure.status
		}
		return 0
	}
	if _, err := poolFromHistory("idx-a", record.ID); statusOf(err) != http.StatusConflict {
		t.Fatalf("a frozen pool must refuse a manual pool, got %v", err)
	}
	if _, pooled := pooledFor(t); pooled {
		t.Fatal("nothing should have entered the frozen pool")
	}
	state.mu.Lock()
	state.rules["idx-a"] = headerRule{AuthIndex: "idx-a", Enabled: true}
	state.mu.Unlock()
	if _, err := poolFromHistory("idx-a", "no-such-row"); statusOf(err) != http.StatusNotFound {
		t.Fatalf("an unknown row is not found, got %v", err)
	}
	resp, _ := handleManagementAPI(managementRequest{Method: http.MethodPost, Path: "/v0/management" + apiTurnStatePoolPath, Body: []byte(`{"auth_index":"idx-a"}`)})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("the route needs a row id, got %d", resp.StatusCode)
	}
}

// poolWithCookie pools one state with a session for idx-a on gpt-5.6-luna.
func poolWithCookie(t *testing.T, rule headerRule, cookie string) string {
	t.Helper()
	blob := fernetToken(0x80, time.Now(), 1)
	state.mu.Lock()
	state.credentials["idx-a"] = credentialSnapshot{AuthIndex: "idx-a", AuthID: "auth-a", Provider: "codex", Name: "a.json"}
	state.rules["idx-a"] = rule
	ok := noteTurnStateMintLocked(blob, "idx-a", "A", "gpt-5.6-luna", "team", cookie)
	state.mu.Unlock()
	if !ok {
		t.Fatal("fixture state did not enter the pool")
	}
	return blob
}

// Cookie injection is independent of state injection: on its own it merges
// the pooled session into the request's Cookie, pooled values winning, and
// leaves X-Codex-Turn-State alone. The session a response mints under is the
// one that went out.
func TestCookieInjectionMergesThePooledSession(t *testing.T) {
	stubCredentialPlan(t, "team")
	resetState(t)
	resetTurnStates(t)
	poolWithCookie(t, headerRule{AuthIndex: "idx-a", InjectCookie: true}, "sid=pool; cf=x")
	response := injectTestRequest(t, "cookie-only", http.Header{"Cookie": {"sid=client; keep=1"}})
	if got := response.Headers.Get("Cookie"); got != "sid=pool; keep=1; cf=x" {
		t.Fatalf("outbound cookie = %q", got)
	}
	if response.Headers.Get(turnStateHeader) != "" {
		t.Fatal("the state switch is off; no state goes out")
	}
	state.mu.Lock()
	attempt := state.pending["cookie-only"].current
	injected, stateInjected, session, after := attempt.CookieInjected, attempt.TurnStateInjected, attempt.clientCookie, attempt.AfterHeaders.Get("Cookie")
	state.mu.Unlock()
	if !injected || stateInjected {
		t.Fatalf("cookie injected=%v state injected=%v", injected, stateInjected)
	}
	if session != "sid=pool; keep=1; cf=x" || after != session {
		t.Fatalf("the recorded session and after view should be what went out: %q / %q", session, after)
	}

	// Both switches on: both go out.
	state.mu.Lock()
	state.rules["idx-a"] = headerRule{AuthIndex: "idx-a", InjectCookie: true, InjectTurnState: true}
	state.mu.Unlock()
	both := injectTestRequest(t, "both", nil)
	if both.Headers.Get(turnStateHeader) == "" || both.Headers.Get("Cookie") != "sid=pool; cf=x" {
		t.Fatalf("both switches: state=%q cookie=%q", both.Headers.Get(turnStateHeader), both.Headers.Get("Cookie"))
	}
}

// Nothing goes out when the pool is frozen, when an enabled rule names Cookie
// by hand, when the switch is off, or when the pooled state has no session.
func TestCookieInjectionStandsDown(t *testing.T) {
	off := false
	cases := []struct {
		name   string
		rule   headerRule
		cookie string
	}{
		{"switch off", headerRule{AuthIndex: "idx-a", InjectTurnState: true}, "sid=pool"},
		{"frozen pool", headerRule{AuthIndex: "idx-a", InjectCookie: true, MaintainStatePool: &off}, "sid=pool"},
		{"pinned by the rule", headerRule{AuthIndex: "idx-a", Enabled: true, InjectCookie: true, Set: map[string]string{"cookie": "sid=operator"}}, "sid=pool"},
		{"no session pooled", headerRule{AuthIndex: "idx-a", InjectCookie: true}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stubCredentialPlan(t, "team")
			resetState(t)
			resetTurnStates(t)
			rule := tc.rule
			rule.MaintainStatePool = nil
			poolWithCookie(t, rule, tc.cookie)
			state.mu.Lock()
			state.rules["idx-a"] = tc.rule
			state.mu.Unlock()
			response := injectTestRequest(t, "req", http.Header{"Cookie": {"sid=client"}})
			if got := response.Headers.Get("Cookie"); got == "sid=pool" || strings.Contains(got, "pool") {
				t.Fatalf("no pooled cookie should go out, got %q", got)
			}
			state.mu.Lock()
			injected := state.pending["req"].current.CookieInjected
			state.mu.Unlock()
			if injected {
				t.Fatal("the record must not claim an injection")
			}
		})
	}
}

// Cookie injection edits the credential's auth file so CPA forwards the
// Cookie: on adds the entry, off removes it, freezing the pool counts as off,
// and a failed edit refuses the whole change.
func TestCookieInjectionManagesTheAuthFileEntry(t *testing.T) {
	resetState(t)
	state.mu.Lock()
	state.credentials["idx-a"] = credentialSnapshot{AuthIndex: "idx-a", AuthID: "auth-a", Provider: "codex", Name: "a.json"}
	state.mu.Unlock()
	oldGet, oldSave := hostAuthGetFileFunc, hostAuthSaveFunc
	t.Cleanup(func() { hostAuthGetFileFunc, hostAuthSaveFunc = oldGet, oldSave })
	file := json.RawMessage(`{"type":"codex","refresh_token":"r"}`)
	hostAuthGetFileFunc = func(string) (hostAuthFile, error) { return hostAuthFile{Name: "a.json", JSON: file}, nil }
	saves := 0
	hostAuthSaveFunc = func(f hostAuthFile) error { saves++; file = f.JSON; return nil }
	forwarding := func() bool { return strings.Contains(string(file), `"$Cookie"`) }

	if _, err := saveRule(headerRule{AuthIndex: "idx-a", InjectCookie: true}); err != nil {
		t.Fatal(err)
	}
	if !forwarding() || saves != 1 {
		t.Fatalf("on adds the entry: %s", file)
	}
	if _, err := saveRule(headerRule{AuthIndex: "idx-a", InjectCookie: true, Enabled: true}); err != nil || saves != 1 {
		t.Fatalf("an unrelated change leaves the file alone: saves=%d err=%v", saves, err)
	}
	off := false
	if _, err := saveRule(headerRule{AuthIndex: "idx-a", InjectCookie: true, MaintainStatePool: &off}); err != nil || forwarding() {
		t.Fatalf("a frozen pool injects nothing, so nothing is forwarded: %s err=%v", file, err)
	}
	if _, err := saveRule(headerRule{AuthIndex: "idx-a", InjectCookie: true}); err != nil || !forwarding() {
		t.Fatalf("thawed, it is back: %s", file)
	}

	// A failed edit refuses the change: the rule keeps what it had.
	hostAuthSaveFunc = func(hostAuthFile) error { return errors.New("disk full") }
	if _, err := saveRule(headerRule{AuthIndex: "idx-a"}); err == nil || !strings.Contains(err.Error(), "cookie forwarding") {
		t.Fatalf("the failure must surface: %v", err)
	}
	state.mu.Lock()
	kept := state.rules["idx-a"].InjectCookie
	state.mu.Unlock()
	if !kept {
		t.Fatal("the switch must not claim a change the file did not take")
	}
	hostAuthSaveFunc = func(f hostAuthFile) error { saves++; file = f.JSON; return nil }

	if err := deleteRule("idx-a"); err != nil || forwarding() {
		t.Fatalf("deleting the rule removes the entry: %s err=%v", file, err)
	}
}

// The cookie pool keeps one cookie per backend, a newer one replacing the
// older; a cookie without __oailb is not kept; only unexpired ones are drawn.
func TestCookiePoolKeysByBackend(t *testing.T) {
	resetState(t)
	now := time.Now()
	fresh := "__oailb=" + oailbToken("gw-a", now, now.Add(time.Hour)) + "; __cf_bm=one"
	newer := "__oailb=" + oailbToken("gw-a", now, now.Add(2*time.Hour)) + "; __cf_bm=two"
	stale := "__oailb=" + oailbToken("gw-b", now.Add(-3*time.Hour), now.Add(-time.Hour))
	state.mu.Lock()
	okFresh := noteCookiePoolLocked("idx-a", fresh, "probe", "m", "d1")
	okNewer := noteCookiePoolLocked("idx-a", newer, "live", "m", "d2")
	okStale := noteCookiePoolLocked("idx-a", stale, "probe", "m", "d3")
	okBare := noteCookiePoolLocked("idx-a", "__cf_bm=no-route", "probe", "m", "d4")
	state.mu.Unlock()
	if !okFresh || !okNewer || !okStale || okBare {
		t.Fatalf("kept: %v %v %v, bare kept: %v", okFresh, okNewer, okStale, okBare)
	}
	entries, _ := cookiePoolFor("idx-a")
	if len(entries) != 2 {
		t.Fatalf("one entry per backend: %+v", entries)
	}
	for _, e := range entries {
		if e.Host == "gw-a" && (e.Cookie != newer || e.Source != "live") {
			t.Fatalf("the newer cookie replaces the older: %+v", e)
		}
	}
	for i := 0; i < 20; i++ {
		entry, ok := pickPoolCookie("idx-a", now)
		if !ok || entry.Host != "gw-a" {
			t.Fatalf("only the unexpired entry is drawn: %+v %v", entry, ok)
		}
	}
	if _, ok := pickPoolCookie("idx-other", now); ok {
		t.Fatal("another credential's pool is its own")
	}
}

// A live turn that carried an injected state and drew none back vouches for
// the session it went out with.
func TestHeldLiveTurnFeedsTheCookiePool(t *testing.T) {
	stubCredentialPlan(t, "team")
	resetState(t)
	resetTurnStates(t)
	poolFixture(t, headerRule{AuthIndex: "idx-a", InjectTurnState: true})
	now := time.Now()
	lb := oailbToken("gw-live", now, now.Add(time.Hour))
	injectTestRequest(t, "held", http.Header{"Cookie": {"__oailb=" + lb}})
	observeResponse(responseInterceptRequest{RequestID: "held", StatusCode: 200, ResponseHeaders: http.Header{"Set-Cookie": {"__cf_bm=fresh; Path=/"}}})
	entries, _ := cookiePoolFor("idx-a")
	if len(entries) != 1 || entries[0].Host != "gw-live" || entries[0].Source != "live" || entries[0].Cookie != "__oailb="+lb+"; __cf_bm=fresh" {
		t.Fatalf("the held turn's session should be pooled: %+v", entries)
	}
}

// A degraded live turn that carried an injected cookie takes that cookie's
// backend out of the Cookie 池: still listed, never drawn, until a new
// non-degraded cookie for the same backend replaces it.
func TestDegradedInjectedCookieInvalidatesItsBackend(t *testing.T) {
	stubCredentialPlan(t, "team")
	resetState(t)
	resetTurnStates(t)
	now := time.Now()
	session := "__oailb=" + oailbToken("gw-bad", now, now.Add(time.Hour))
	poolWithCookie(t, headerRule{AuthIndex: "idx-a", InjectTurnState: true, InjectCookie: true}, session)
	state.mu.Lock()
	noteCookiePoolLocked("idx-a", session, "probe", "m", "")
	state.mu.Unlock()
	if _, ok := pickPoolCookie("idx-a", now); !ok {
		t.Fatal("the fixture entry should be usable")
	}

	injectTestRequest(t, "bad", nil)
	observeResponse(responseInterceptRequest{RequestID: "bad", StatusCode: 200, ResponseHeaders: http.Header{turnStateHeader: {fernetToken(0x80, now, 2)}}})
	state.mu.Lock()
	attempt := *state.pending["bad"].current
	state.mu.Unlock()
	if attempt.CookieInvalidated != "gw-bad" {
		t.Fatalf("the record should name the backend taken out: %q", attempt.CookieInvalidated)
	}
	entries, _ := cookiePoolFor("idx-a")
	if len(entries) != 1 || entries[0].InvalidatedAt.IsZero() || entries[0].InvalidatedBy != "live" || entries[0].usable(now) {
		t.Fatalf("the entry stays listed but unusable: %+v", entries)
	}
	if _, ok := pickPoolCookie("idx-a", now); ok {
		t.Fatal("an invalidated entry is never drawn")
	}
	// A new non-degraded cookie for the same backend brings it back.
	state.mu.Lock()
	noteCookiePoolLocked("idx-a", session+"; __cf_bm=new", "probe", "m", "")
	state.mu.Unlock()
	if entry, ok := pickPoolCookie("idx-a", now); !ok || !entry.InvalidatedAt.IsZero() {
		t.Fatalf("the replacement is usable: %+v %v", entry, ok)
	}
}

// A cookie can be pooled by hand, pasted or from a recorded request's session,
// and a pool entry can replace the credential's own cookie.
func TestManualCookiePoolAndCredentialReplace(t *testing.T) {
	stubCredentialPlan(t, "team")
	store := resetState(t)
	resetTurnStates(t)
	now := time.Now()
	statusOf := func(err error) int {
		var failure *manualPoolError
		if errors.As(err, &failure) {
			return failure.status
		}
		return 0
	}

	pasted := "__oailb=" + oailbToken("gw-pasted", now, now.Add(time.Hour)) + "; oai-did=x"
	entry, err := poolCookieByHand("idx-a", "", pasted)
	if err != nil || entry.Host != "gw-pasted" || entry.Source != "manual" {
		t.Fatalf("pasted: %+v %v", entry, err)
	}
	if _, err := poolCookieByHand("idx-a", "", "__cf_bm=no-route"); statusOf(err) != http.StatusUnprocessableEntity {
		t.Fatalf("a cookie without __oailb is refused: %v", err)
	}

	// From a record: what went out, updated by what the response set.
	lb := oailbToken("gw-record", now, now.Add(time.Hour))
	state.mu.Lock()
	state.credentials["idx-a"] = credentialSnapshot{AuthIndex: "idx-a", AuthID: "auth-a", Provider: "codex", Name: "a.json"}
	state.mu.Unlock()
	injectTestRequest(t, "rec", http.Header{"Cookie": {"sid=1"}})
	observeResponse(responseInterceptRequest{RequestID: "rec", StatusCode: 200, ResponseHeaders: http.Header{"Set-Cookie": {"__oailb=" + lb + "; Path=/"}}})
	completeRequest(requestCompletion{RequestID: "rec", Outcome: "succeeded", StatusCode: 200, CompletedAt: now})
	record := lastAttempt(t)
	entry, err = poolCookieByHand("idx-a", record.ID, "")
	if err != nil || entry.Host != "gw-record" {
		t.Fatalf("from a record: %+v %v", entry, err)
	}
	entries, _ := cookiePoolFor("idx-a")
	for _, e := range entries {
		if e.Host == "gw-record" && e.Cookie != "sid=1; __oailb="+lb {
			t.Fatalf("the record's session is pooled: %q", e.Cookie)
		}
	}
	if _, err := poolCookieByHand("idx-a", "no-such", ""); statusOf(err) != http.StatusNotFound {
		t.Fatalf("an unknown record is not found: %v", err)
	}

	// Replacing the credential cookie leaves the liveness clock alone.
	live := now.Add(-time.Minute).UTC()
	if err := store.SaveSession(credentialSession{AuthIndex: "idx-a", Cookie: "old=1", LastLiveAt: live}); err != nil {
		t.Fatal(err)
	}
	if _, err := useCookieForCredential("idx-a", "gw-pasted"); err != nil {
		t.Fatal(err)
	}
	jar, _, _ := store.Session("idx-a", "")
	if jar.Cookie != pasted || !jar.LastLiveAt.Equal(live) {
		t.Fatalf("jar=%+v", jar)
	}
	if _, err := useCookieForCredential("idx-a", "gw-none"); statusOf(err) != http.StatusNotFound {
		t.Fatalf("an unknown routing target is not found: %v", err)
	}
}
