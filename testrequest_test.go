//go:build localtest

package main

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

func testStubCredential(t *testing.T, document string) {
	t.Helper()
	oldRuntime, oldGet, oldDo := hostAuthGetRuntimeFunc, hostAuthGetFunc, hostHTTPDoFunc
	hostAuthGetRuntimeFunc = func(idx string) (hostAuthGetRuntimeResponse, error) {
		return hostAuthGetRuntimeResponse{Auth: hostAuthFileEntry{ID: "a", AuthIndex: idx, Name: "a.json", Provider: "codex"}}, nil
	}
	hostAuthGetFunc = func(string) (json.RawMessage, error) { return json.RawMessage(document), nil }
	t.Cleanup(func() {
		hostAuthGetRuntimeFunc, hostAuthGetFunc, hostHTTPDoFunc = oldRuntime, oldGet, oldDo
	})
}

const stubCredentialDocument = `{"access_token":"tok-secret","account":"alex_nnn@qq.com","id":"codex-alex_nnn@qq.com-team.json","id_token":{"chatgpt_account_id":"5417aaaa-bbbb-cccc-dddd-eeee00008d01"}}`

func TestDryRunResolvesHeadersWithoutTouchingTheUpstream(t *testing.T) {
	resetState(t)
	testStubCredential(t, stubCredentialDocument)
	hostHTTPDoFunc = func(hostHTTPRequest) (hostHTTPResponse, error) {
		t.Fatal("dry run must not send an upstream request")
		return hostHTTPResponse{}, nil
	}
	if _, err := saveRule(headerRule{AuthIndex: "idx-a", Enabled: true, Set: map[string]string{"X-Team": "A"}, Remove: []string{"Originator"}}); err != nil {
		t.Fatal(err)
	}
	result, err := runTestRequest(testRequest{AuthIndex: "idx-a", ApplyRule: true, DryRun: true, Record: true})
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome != "dry_run" || !result.RuleApplied {
		t.Fatalf("result=%#v", result)
	}
	if result.AfterHeaders.Get("X-Team") != "A" || result.AfterHeaders.Get("Originator") != "" {
		t.Fatalf("rule not applied: %#v", result.AfterHeaders)
	}
	if result.BeforeHeaders.Get("Originator") != "codex-tui" {
		t.Fatalf("base headers missing Originator: %#v", result.BeforeHeaders)
	}
	// A dry run reads no credential material at all, yet still shows that the
	// authenticated headers are part of the request a rule would act on.
	if got := result.BeforeHeaders.Get("Authorization"); got != "Bearer [REDACTED]" {
		t.Fatalf("authorization=%q", got)
	}
	if result.BeforeHeaders.Get("Chatgpt-Account-Id") == "" {
		t.Fatalf("account header missing from a dry run: %#v", result.BeforeHeaders)
	}
	if result.Recorded {
		t.Fatal("a dry run is not history")
	}
}

func TestRealTestRequestReportsUpstreamModelMismatchAndRecordsIt(t *testing.T) {
	p := resetState(t)
	testStubCredential(t, stubCredentialDocument)
	var sent hostHTTPRequest
	hostHTTPDoFunc = func(request hostHTTPRequest) (hostHTTPResponse, error) {
		sent = request
		body := "event: response.created\ndata: {\"response\":{\"model\":\"gpt-5.6-luna\"}}\n\n" +
			"event: response.completed\ndata: {\"response\":{\"model\":\"gpt-5.6-luna-mini\"}}\n\n"
		return hostHTTPResponse{StatusCode: 200, Headers: map[string][]string{"Set-Cookie": {"secret"}, "X-Request-Id": {"r1"}}, Body: []byte(body)}, nil
	}
	result, err := runTestRequest(testRequest{AuthIndex: "idx-a", Model: "gpt-5.6-luna", Stream: true, Record: true, Headers: map[string]string{"x-probe": "1"}})
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome != "succeeded" || result.StatusCode != 200 {
		t.Fatalf("result=%#v", result)
	}
	if result.UpstreamModel != "gpt-5.6-luna-mini" || result.ModelMismatch == nil || !*result.ModelMismatch {
		t.Fatalf("mismatch not reported: %#v", result)
	}
	if !result.ModelConflict {
		t.Fatal("two disagreeing declarations should surface a conflict")
	}
	if sent.Headers.Get("Authorization") != "Bearer tok-secret" {
		t.Fatalf("upstream request lost its credential: %#v", sent.Headers)
	}
	if sent.Headers.Get("Chatgpt-Account-Id") != "5417aaaa-bbbb-cccc-dddd-eeee00008d01" {
		t.Fatalf("account id not taken from id_token: %#v", sent.Headers)
	}
	if sent.Headers.Get("X-Probe") != "1" {
		t.Fatalf("custom header dropped: %#v", sent.Headers)
	}
	if got := result.BeforeHeaders.Get("Authorization"); got != "Bearer [REDACTED]" {
		t.Fatalf("returned headers must be redacted, got %q", got)
	}
	if got := result.ResponseHeaders.Get("Set-Cookie"); got != "[REDACTED]" {
		t.Fatalf("response secret not redacted: %q", got)
	}

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
	record := page.Items[0]
	if record.Origin != originTest || record.UpstreamModel != "gpt-5.6-luna-mini" || record.ModelMismatch == nil || !*record.ModelMismatch {
		t.Fatalf("record=%#v", record)
	}
	if record.BeforeHeaders.Get("Authorization") != "Bearer [REDACTED]" {
		t.Fatalf("history kept a credential: %#v", record.BeforeHeaders)
	}
}

func TestMatchingModelIsReportedAsNoMismatch(t *testing.T) {
	resetState(t)
	testStubCredential(t, stubCredentialDocument)
	hostHTTPDoFunc = func(hostHTTPRequest) (hostHTTPResponse, error) {
		return hostHTTPResponse{StatusCode: 200, Body: []byte(`{"model":"gpt-5.6-luna","output":[]}`)}, nil
	}
	result, err := runTestRequest(testRequest{AuthIndex: "idx-a", Model: "gpt-5.6-luna"})
	if err != nil {
		t.Fatal(err)
	}
	if result.ModelMismatch == nil || *result.ModelMismatch {
		t.Fatalf("expected an explicit match verdict, got %#v", result.ModelMismatch)
	}
}

func TestUpstreamFailureKeepsAnErrorPreviewOutOfHistory(t *testing.T) {
	p := resetState(t)
	testStubCredential(t, stubCredentialDocument)
	hostHTTPDoFunc = func(hostHTTPRequest) (hostHTTPResponse, error) {
		return hostHTTPResponse{StatusCode: 429, Body: []byte(`{"error":{"message":"rate limit"}}`)}, nil
	}
	result, err := runTestRequest(testRequest{AuthIndex: "idx-a", Record: true})
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome != "failed" || result.StatusCode != 429 || !strings.Contains(result.ErrorPreview, "rate limit") {
		t.Fatalf("result=%#v", result)
	}
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
	raw, _ := json.Marshal(page.Items[0])
	if strings.Contains(string(raw), "rate limit") {
		t.Fatalf("upstream body text reached persistence: %s", raw)
	}
}

func TestEndpointIsPinnedToTheCodexBackend(t *testing.T) {
	resetState(t)
	testStubCredential(t, stubCredentialDocument)
	hostHTTPDoFunc = func(hostHTTPRequest) (hostHTTPResponse, error) {
		t.Fatal("a rejected endpoint must not be contacted")
		return hostHTTPResponse{}, nil
	}
	for _, endpoint := range []string{
		"https://example.com/backend-api/codex/responses",
		"http://chatgpt.com/backend-api/codex/responses",
		"https://chatgpt.com/admin",
	} {
		if _, err := runTestRequest(testRequest{AuthIndex: "idx-a", Endpoint: endpoint}); err == nil {
			t.Fatalf("endpoint %q should be rejected", endpoint)
		}
	}
	if got, err := resolveTestEndpoint("https://chatgpt.com/backend-api/codex/responses?x=1"); err != nil || !strings.HasPrefix(got, defaultTestURL) {
		t.Fatalf("got=%q err=%v", got, err)
	}
}

func TestSetAndRemoveOfTheSameHeaderIsRejected(t *testing.T) {
	resetState(t)
	testStubCredential(t, stubCredentialDocument)
	if _, err := saveRule(headerRule{AuthIndex: "idx-a", Enabled: true, Set: map[string]string{"X-Team": "A"}}); err != nil {
		t.Fatal(err)
	}
	_, err := runTestRequest(testRequest{AuthIndex: "idx-a", ApplyRule: true, DryRun: true, Remove: []string{"x-team"}})
	if err == nil || !strings.Contains(err.Error(), "cannot be set and removed") {
		t.Fatalf("err=%v", err)
	}
}

func TestAdHocRemovalWorksWithoutASavedRule(t *testing.T) {
	resetState(t)
	testStubCredential(t, stubCredentialDocument)
	result, err := runTestRequest(testRequest{AuthIndex: "idx-a", DryRun: true, Remove: []string{"originator"}})
	if err != nil {
		t.Fatal(err)
	}
	if !result.RuleApplied || result.AfterHeaders.Get("Originator") != "" {
		t.Fatalf("result=%#v", result)
	}
}

func TestCredentialAccountIDNeverFallsBackToAnAddress(t *testing.T) {
	material := parseTestAuthMaterial(json.RawMessage(`{"access_token":"t","account":"alex@qq.com","id":"codex-alex@qq.com.json"}`))
	if material.accountID != "" {
		t.Fatalf("account id must stay empty rather than borrow an address, got %q", material.accountID)
	}
	if material.accessToken != "t" {
		t.Fatalf("token=%q", material.accessToken)
	}
	nested := parseTestAuthMaterial(json.RawMessage(`{"tokens":{"access_token":"t2","chatgpt_account_id":"acc-2"}}`))
	if nested.accessToken != "t2" || nested.accountID != "acc-2" {
		t.Fatalf("nested=%#v", nested)
	}
}

func TestMissingAccountIDFailsBeforeSendingAnything(t *testing.T) {
	resetState(t)
	testStubCredential(t, `{"access_token":"tok"}`)
	hostHTTPDoFunc = func(hostHTTPRequest) (hostHTTPResponse, error) {
		t.Fatal("must not send a request without an account id")
		return hostHTTPResponse{}, nil
	}
	if _, err := runTestRequest(testRequest{AuthIndex: "idx-a"}); err == nil {
		t.Fatal("expected a credential error")
	}
}

func TestManagementTestRouteReturnsAResult(t *testing.T) {
	shutdownPlugin()
	p, err := openPersistence(filepath.Join(t.TempDir(), "test-route.json"))
	if err != nil {
		t.Fatal(err)
	}
	state.mu.Lock()
	state.store = p
	state.writer = newQueuedPersistence(p)
	state.rules = map[string]headerRule{}
	state.pending = map[string]*pendingRequest{}
	state.credentials = map[string]credentialSnapshot{}
	state.mu.Unlock()
	t.Cleanup(shutdownPlugin)
	testStubCredential(t, stubCredentialDocument)

	body, _ := json.Marshal(testRequest{AuthIndex: "idx-a", DryRun: true})
	resp, err := handleManagementAPI(managementRequest{Method: "POST", Path: "/v0/management" + apiTestPath, Body: body})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("status=%d body=%s", resp.StatusCode, resp.Body)
	}
	var decoded struct {
		Result testResult `json:"result"`
	}
	if err := json.Unmarshal(resp.Body, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Result.Outcome != "dry_run" || decoded.Result.Endpoint != defaultTestURL {
		t.Fatalf("result=%#v", decoded.Result)
	}

	bad, _ := json.Marshal(testRequest{DryRun: true})
	resp, _ = handleManagementAPI(managementRequest{Method: "POST", Path: "/v0/management" + apiTestPath, Body: bad})
	if resp.StatusCode != 400 {
		t.Fatalf("missing auth_index should be a 400, got %d %s", resp.StatusCode, resp.Body)
	}
}
