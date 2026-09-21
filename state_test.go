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
	pooled := noteTurnStateMintLocked(blob, "idx-pro-a", "Pro A", "gpt-5.6-luna", "pro")
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
	observeResponse(responseInterceptRequest{RequestID: "req", StatusCode: 200, ResponseHeaders: http.Header{"Set-Cookie": {"secret"}, "X-Upstream": {"ok"}}})
	completeRequest(requestCompletion{RequestID: "req", Outcome: "succeeded", StatusCode: 200, StartedAt: time.Now().Add(-time.Second), CompletedAt: time.Now()})
	state.mu.Lock()
	writer := state.writer
	state.writer = nil
	state.store = nil
	state.mu.Unlock()
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	pa, _ := p.History("idx-a", 1)
	pb, _ := p.History("idx-b", 1)
	if len(pa.Items) != 1 || pa.Items[0].Outcome != "switched" || pa.Items[0].StatusCode != 0 {
		t.Fatalf("A=%#v", pa.Items)
	}
	if len(pb.Items) != 1 || pb.Items[0].Outcome != "succeeded" || pb.Items[0].StatusCode != 200 {
		t.Fatalf("B=%#v", pb.Items)
	}
	if pa.Items[0].BeforeHeaders.Get("Authorization") != "Bearer [REDACTED]" {
		t.Fatalf("request secret not redacted %#v", pa.Items[0].BeforeHeaders)
	}
	if pb.Items[0].ResponseHeaders.Get("Set-Cookie") != "[REDACTED]" {
		t.Fatalf("response secret not redacted %#v", pb.Items[0].ResponseHeaders)
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
	page, _ := p.History("idx-a", 1)
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
	page, err := p.History("idx-callback", 1)
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
	page, _ := p.History("idx-a", 1)
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

		minted, _ := p.History("idx-a", 1)
		if len(minted.Items) != 1 || minted.Items[0].TurnStateMinted == nil {
			t.Fatalf("mint not recorded: %#v", minted.Items)
		}
		if !minted.Items[0].TurnStateMinted.FernetLike {
			t.Fatalf("minted envelope not decoded: %#v", minted.Items[0].TurnStateMinted)
		}
		quality := minted.Items[0].TurnStateMinted
		if !quality.Pooled || quality.NonDegraded == nil || !*quality.NonDegraded || quality.PlanType != "team" || quality.MaxChars != teamStateMaxChars {
			t.Fatalf("non-degraded state was not minted into the pool: %#v", quality)
		}

		echoed, _ := p.History("idx-b", 1)
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
	page, _ := p.History("idx-a", 1)
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
	if !noteTurnStateMintLocked(blob, "idx-a", "A", "gpt-5.6-luna", "team") {
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
	noteTurnStateMintLocked(pooled, "idx-a", "A", "gpt-5.6-luna", "team")
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
	ok := noteTurnStateMintLocked(blob, "idx-a", "A", "gpt-5.6-luna", "team")
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

// Injection is part of rewriting, so a credential whose rule switch is off is
// passed through untouched even when the pool has a qualified state for it.
func TestPoolDoesNotInjectWhileTheRuleIsOff(t *testing.T) {
	stubCredentialPlan(t, "team")
	resetState(t)
	resetTurnStates(t)
	poolFixture(t, headerRule{AuthIndex: "idx-a", Enabled: false})
	if got := injectTestRequest(t, "rule-off", nil).Headers.Get(turnStateHeader); got != "" {
		t.Fatalf("a disabled rule must not inject: %q", got)
	}
	state.mu.Lock()
	attempt := state.pending["rule-off"].current
	state.mu.Unlock()
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
	ok := noteTurnStateMintLocked(blob, "idx-a", "A", "gpt-5.6-luna", "team")
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
	page, err := state.store.History("idx-a", 1)
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

// Inside the window the same outcome proves nothing about the pooled state:
// one degraded turn can have other causes, and the fresh state stays.
func TestFreshInjectedStateSurvivesADegradedResponse(t *testing.T) {
	stubCredentialPlan(t, "team")
	resetState(t)
	resetTurnStates(t)
	fresh := pooledAt(t, time.Now().Add(-5*time.Minute))
	injectTestRequest(t, "fresh", nil)
	observeResponse(responseInterceptRequest{RequestID: "fresh", StatusCode: 200, ResponseHeaders: http.Header{turnStateHeader: {fernetToken(0x80, time.Now(), 40)}}})
	completeRequest(requestCompletion{RequestID: "fresh", Outcome: "succeeded", StatusCode: 200, CompletedAt: time.Now()})
	if origin, ok := pooledFor(t); !ok || origin.blob != fresh {
		t.Fatal("a state inside the window must not be invalidated by one degraded turn")
	}
	if got := lastAttempt(t); got.TurnStateInvalidated {
		t.Fatalf("attempt wrongly flagged: %#v", got)
	}
}

// An expired state that came back non-degraded is simply superseded by the new
// mint, which is the ordinary path; nothing is invalidated.
func TestExpiredInjectedStateIsSupersededByAGoodResponse(t *testing.T) {
	stubCredentialPlan(t, "team")
	resetState(t)
	resetTurnStates(t)
	pooledAt(t, time.Now().Add(-2*time.Hour))
	injectTestRequest(t, "renew", nil)
	good := fernetToken(0x80, time.Now(), 1)
	observeResponse(responseInterceptRequest{RequestID: "renew", StatusCode: 200, ResponseHeaders: http.Header{turnStateHeader: {good}}})
	completeRequest(requestCompletion{RequestID: "renew", Outcome: "succeeded", StatusCode: 200, CompletedAt: time.Now()})
	if origin, ok := pooledFor(t); !ok || origin.blob != good {
		t.Fatal("the fresh mint should have replaced the expired state")
	}
	if got := lastAttempt(t); got.TurnStateInvalidated {
		t.Fatalf("attempt wrongly flagged: %#v", got)
	}
}

func rejectFixture(t *testing.T, reject bool) {
	t.Helper()
	stubCredentialPlan(t, "team")
	resetState(t)
	resetTurnStates(t)
	state.mu.Lock()
	state.credentials["idx-a"] = credentialSnapshot{AuthIndex: "idx-a", AuthID: "auth-a", Provider: "codex", Name: "a.json"}
	state.rules["idx-a"] = headerRule{AuthIndex: "idx-a", Enabled: true, RejectDegradedResponse: reject}
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

// The flag off, or a state that is not degraded, leaves the response alone.
func TestResponsesPassThroughWithoutTheFlagOrWithoutDegradation(t *testing.T) {
	rejectFixture(t, false)
	degraded := fernetToken(0x80, time.Now(), 40)
	if out := observeResponse(responseInterceptRequest{RequestID: "r", StatusCode: 200, ResponseHeaders: http.Header{turnStateHeader: {degraded}}}); len(out.Body) != 0 || len(out.Headers) != 0 {
		t.Fatalf("flag off must pass through: %#v", out)
	}
	rejectFixture(t, true)
	good := fernetToken(0x80, time.Now(), 1)
	if out := observeResponse(responseInterceptRequest{RequestID: "r", StatusCode: 200, ResponseHeaders: http.Header{turnStateHeader: {good}}}); len(out.Body) != 0 || len(out.Headers) != 0 {
		t.Fatalf("a non-degraded state must pass through: %#v", out)
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
	noteTurnStateMintLocked(foreign, "idx-b", "B", "gpt-5.6-luna", "team")
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
	noteTurnStateMintLocked(foreign, "idx-b", "B", "gpt-5.6-luna", "team")
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
