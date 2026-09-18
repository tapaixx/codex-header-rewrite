//go:build localtest

package main

import (
	"encoding/json"
	"path/filepath"
	"reflect"
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
	wantProfile := []string{"Host", "Content-Type", "Authorization", "User-Agent", "Accept", "Chatgpt-Account-Id", "Originator", "X-Probe"}
	if sent.WireProfile == nil || !sent.WireProfile.HTTP1Only || !sent.WireProfile.DisableAutoCompression || !reflect.DeepEqual(sent.WireProfile.HeaderProfile, wantProfile) {
		t.Fatalf("Codex wire profile=%#v, want headers %#v", sent.WireProfile, wantProfile)
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

func TestCustomEndpointsAreAllowedButUnsafeTransportIsNot(t *testing.T) {
	for _, endpoint := range []string{
		"https://gateway.internal.example/v1/responses",
		"http://localhost:8317/v1/responses",
		"http://127.0.0.1:8317/v1/responses",
		"https://chatgpt.com/backend-api/codex/responses?x=1",
	} {
		if _, _, err := resolveTestEndpoint(endpoint); err != nil {
			t.Fatalf("endpoint %q should be allowed: %v", endpoint, err)
		}
	}
	for _, endpoint := range []string{
		"http://gateway.example.com/v1/responses", // plaintext to a remote host
		"ftp://example.com/x",                     // not an HTTP scheme
		"/backend-api/codex/responses",            // not absolute
		"https://user:pass@example.com/v1",        // credentials in the URL
	} {
		if _, _, err := resolveTestEndpoint(endpoint); err == nil {
			t.Fatalf("endpoint %q should be rejected", endpoint)
		}
	}
	if _, codex, _ := resolveTestEndpoint("https://gateway.example.com/v1"); codex {
		t.Fatal("a non-Codex host must not be reported as the Codex backend")
	}
	if _, codex, _ := resolveTestEndpoint(""); !codex {
		t.Fatal("the default endpoint is the Codex backend")
	}
}

func TestCredentialTravelsToTheCodexBackendAndNowhereElseByDefault(t *testing.T) {
	resetState(t)
	testStubCredential(t, stubCredentialDocument)
	credentialReads := 0
	baseGet := hostAuthGetFunc
	hostAuthGetFunc = func(index string) (json.RawMessage, error) {
		credentialReads++
		return baseGet(index)
	}
	var sent hostHTTPRequest
	hostHTTPDoFunc = func(request hostHTTPRequest) (hostHTTPResponse, error) {
		sent = request
		return hostHTTPResponse{StatusCode: 200, Body: []byte(`{"model":"gpt-5.6-luna"}`)}, nil
	}

	custom, err := runTestRequest(testRequest{AuthIndex: "idx-a", Endpoint: "https://gateway.example.com/v1/responses"})
	if err != nil {
		t.Fatal(err)
	}
	if custom.CredentialAttached || custom.CodexBackend {
		t.Fatalf("a custom host must not carry the credential by default: %#v", custom)
	}
	if sent.Headers.Get("Authorization") != "" || sent.Headers.Get("Chatgpt-Account-Id") != "" {
		t.Fatalf("credential leaked to a custom host: %#v", sent.Headers)
	}
	if sent.WireProfile != nil {
		t.Fatalf("a custom endpoint must keep the host's normal transport profile: %#v", sent.WireProfile)
	}
	if credentialReads != 0 {
		t.Fatalf("credential was read %d times for a request that does not carry it", credentialReads)
	}

	attach := true
	optedIn, err := runTestRequest(testRequest{AuthIndex: "idx-a", Endpoint: "https://gateway.example.com/v1/responses", AttachCredential: &attach})
	if err != nil {
		t.Fatal(err)
	}
	if !optedIn.CredentialAttached || sent.Headers.Get("Authorization") != "Bearer tok-secret" {
		t.Fatalf("explicit opt-in should attach the credential: %#v %#v", optedIn, sent.Headers)
	}

	withheld := false
	refused, err := runTestRequest(testRequest{AuthIndex: "idx-a", AttachCredential: &withheld})
	if err != nil {
		t.Fatal(err)
	}
	if refused.CredentialAttached || sent.Headers.Get("Authorization") != "" {
		t.Fatalf("opting out must withhold the credential even from the Codex backend: %#v", sent.Headers)
	}

	defaulted, err := runTestRequest(testRequest{AuthIndex: "idx-a"})
	if err != nil {
		t.Fatal(err)
	}
	if !defaulted.CredentialAttached || sent.Headers.Get("Authorization") != "Bearer tok-secret" {
		t.Fatalf("the Codex backend should carry the credential by default: %#v", sent.Headers)
	}
}

func TestCustomEndpointWithoutCredentialDoesNotNeedAnAccountID(t *testing.T) {
	resetState(t)
	testStubCredential(t, `{"access_token":"tok"}`)
	hostHTTPDoFunc = func(hostHTTPRequest) (hostHTTPResponse, error) {
		return hostHTTPResponse{StatusCode: 200, Body: []byte(`{"model":"m"}`)}, nil
	}
	result, err := runTestRequest(testRequest{AuthIndex: "idx-a", Endpoint: "http://localhost:8317/v1/responses"})
	if err != nil {
		t.Fatalf("a request that carries no credential must not require one: %v", err)
	}
	if result.Outcome != "succeeded" {
		t.Fatalf("result=%#v", result)
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
