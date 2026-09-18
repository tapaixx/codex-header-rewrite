package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"runtime"
	"sort"
	"strings"
	"time"
)

const (
	codexBackendHost  = "chatgpt.com"
	defaultTestURL    = "https://chatgpt.com/backend-api/codex/responses"
	defaultTestModel  = "gpt-5.6-luna"
	defaultTestPrompt = "Reply with exactly OK"
	// Upstream error text is returned to the panel to explain a failure. It is
	// truncated, returned once, and never written to history.
	testErrorPreviewLimit = 600
)

// testRequest is one operator-issued request used to exercise a header rule.
// Dry runs resolve headers without contacting the upstream; a real run sends
// one Codex request with the credential the rule belongs to.
type testRequest struct {
	AuthIndex    string            `json:"auth_index"`
	Model        string            `json:"model"`
	Prompt       string            `json:"prompt"`
	Instructions string            `json:"instructions"`
	Effort       string            `json:"reasoning_effort"`
	Stream       bool              `json:"stream"`
	ApplyRule    bool              `json:"apply_rule"`
	Headers      map[string]string `json:"headers"`
	Remove       []string          `json:"remove"`
	Endpoint     string            `json:"endpoint"`
	Body         string            `json:"body"`
	DryRun       bool              `json:"dry_run"`
	Record       bool              `json:"record"`
	// AttachCredential overrides the default: send the credential to the Codex
	// backend, and to nowhere else unless asked.
	AttachCredential *bool `json:"attach_credential"`
}

type testResult struct {
	AuthIndex          string `json:"auth_index"`
	DryRun             bool   `json:"dry_run"`
	Endpoint           string `json:"endpoint"`
	CodexBackend       bool   `json:"codex_backend"`
	CredentialAttached bool   `json:"credential_attached"`

	Method          string      `json:"method"`
	SentModel       string      `json:"sent_model,omitempty"`
	RuleApplied     bool        `json:"rule_applied"`
	BeforeHeaders   http.Header `json:"before_headers"`
	AfterHeaders    http.Header `json:"after_headers"`
	StatusCode      int         `json:"status_code,omitempty"`
	ResponseHeaders http.Header `json:"response_headers,omitempty"`
	UpstreamModel   string      `json:"upstream_model,omitempty"`
	ModelMismatch   *bool       `json:"model_mismatch,omitempty"`
	ModelConflict   bool        `json:"model_conflict,omitempty"`
	LatencyMS       int64       `json:"latency_ms,omitempty"`
	Outcome         string      `json:"outcome"`
	Error           string      `json:"error,omitempty"`
	ErrorPreview    string      `json:"error_preview,omitempty"`
	Recorded        bool        `json:"recorded"`
	HistoryID       string      `json:"history_id,omitempty"`
}

type testRequestError struct {
	status  int
	message string
}

func (e *testRequestError) Error() string { return e.message }

func badTestRequest(format string, args ...any) error {
	return &testRequestError{status: http.StatusBadRequest, message: fmt.Sprintf(format, args...)}
}

// runTestRequest resolves the outgoing headers for a test request and, unless
// this is a dry run, sends it upstream through the host.
//
// A real run spends real quota on the selected credential: it is an explicit
// operator action, never something the plugin schedules on its own.
func runTestRequest(req testRequest) (testResult, error) {
	authIndex := strings.TrimSpace(req.AuthIndex)
	if authIndex == "" {
		return testResult{}, badTestRequest("auth_index is required")
	}
	runtimeAuth, err := hostAuthGetRuntimeFunc(authIndex)
	if err != nil {
		return testResult{}, badTestRequest("credential not found")
	}
	if !isCodexCredential(runtimeAuth.Auth.Provider, runtimeAuth.Auth.Type) {
		return testResult{}, badTestRequest("credential is not Codex type")
	}
	endpoint, codexBackend, err := resolveTestEndpoint(req.Endpoint)
	if err != nil {
		return testResult{}, err
	}
	carriesCredential := attachCredential(req, codexBackend)
	body, model, err := resolveTestBody(req)
	if err != nil {
		return testResult{}, err
	}

	rule, applyRule, err := resolveTestRule(authIndex, req)
	if err != nil {
		return testResult{}, err
	}

	result := testResult{
		AuthIndex:          authIndex,
		DryRun:             req.DryRun,
		Endpoint:           endpoint,
		CodexBackend:       codexBackend,
		CredentialAttached: carriesCredential,
		Method:             http.MethodPost,
		SentModel:          model,
		RuleApplied:        applyRule,
	}

	started := time.Now().UTC()
	base, material, err := testBaseHeaders(req, authIndex, carriesCredential)
	if err != nil {
		return testResult{}, err
	}
	outgoing := cloneHeader(base)
	if applyRule {
		outgoing = applyRuleToHeaders(base, rule)
	}
	result.BeforeHeaders = redactHeaders(base)
	result.AfterHeaders = redactHeaders(outgoing)

	if req.DryRun {
		result.Outcome = "dry_run"
		return result, nil
	}
	if carriesCredential && material.accessToken == "" {
		return testResult{}, &testRequestError{status: http.StatusBadGateway, message: "credential has no usable access token"}
	}

	// Timed around the upstream call only: reading the credential through the
	// host is plugin overhead, not upstream latency.
	callStarted := time.Now()
	response, callErr := hostHTTPDoFunc(hostHTTPRequest{
		Method:      http.MethodPost,
		URL:         endpoint,
		Headers:     outgoing,
		Body:        body,
		WireProfile: testWireProfile(outgoing, codexBackend),
	})
	result.LatencyMS = time.Since(callStarted).Milliseconds()
	result.StatusCode = response.StatusCode
	result.ResponseHeaders = redactHeaders(response.Headers)
	if callErr != nil {
		result.Outcome = "error"
		result.Error = callErr.Error()
	} else {
		var observer modelObserver
		observer.observeBody(response.Body)
		result.UpstreamModel = observer.model()
		result.ModelConflict = observer.conflicted()
		result.ModelMismatch = modelMismatch(model, result.UpstreamModel)
		switch {
		case response.StatusCode >= 200 && response.StatusCode < 300:
			result.Outcome = "succeeded"
		default:
			result.Outcome = "failed"
			result.ErrorPreview = truncateText(string(response.Body), testErrorPreviewLimit)
		}
	}

	if req.Record {
		result.HistoryID = recordTestAttempt(req, result, runtimeAuth.Auth, started)
		result.Recorded = result.HistoryID != ""
	}
	return result, nil
}

type testAuthMaterial struct {
	accessToken string
	accountID   string
}

// resolveTestRule merges the saved rule, when requested, with the ad-hoc
// removals of this test. Validating the merged rule is what makes a request
// that both sets and removes the same header fail loudly instead of silently
// keeping the header.
func resolveTestRule(authIndex string, req testRequest) (headerRule, bool, error) {
	saved, hasSaved := getRule(authIndex)
	merged := headerRule{AuthIndex: authIndex, Enabled: true}
	apply := false
	if req.ApplyRule && hasSaved && saved.Enabled {
		merged.Set = saved.Set
		merged.Remove = append([]string(nil), saved.Remove...)
		apply = true
	}
	if len(req.Remove) > 0 {
		merged.Remove = append(merged.Remove, req.Remove...)
		apply = true
	}
	if !apply {
		return headerRule{}, false, nil
	}
	validated, err := validateRule(merged)
	if err != nil {
		return headerRule{}, false, badTestRequest("%s", err.Error())
	}
	return validated, true, nil
}

// testBaseHeaders builds the headers a Codex request needs before any rule is
// applied, then layers the operator's custom headers on top.
//
// Credential material is read only when this request is actually going to carry
// it, and only for the single call that follows. Returned header maps are
// redacted before they leave the plugin.
func testBaseHeaders(req testRequest, authIndex string, carriesCredential bool) (http.Header, testAuthMaterial, error) {
	material := testAuthMaterial{}
	if !req.DryRun && carriesCredential {
		document, err := hostAuthGetFunc(authIndex)
		if err != nil {
			return nil, material, &testRequestError{status: http.StatusBadGateway, message: "credential is not readable through the host"}
		}
		material = parseTestAuthMaterial(document)
		if material.accountID == "" {
			return nil, material, &testRequestError{status: http.StatusBadGateway, message: "credential has no ChatGPT account id"}
		}
	}
	accept := "application/json"
	if req.Stream {
		accept = "text/event-stream"
	}
	base := http.Header{
		"Content-Type": {"application/json"},
		"Accept":       {accept},
		"Originator":   {"codex-tui"},
		"User-Agent":   {fmt.Sprintf("codex-header-rewrite/%s (Linux; %s)", pluginVersion, runtime.GOARCH)},
	}
	switch {
	case !carriesCredential:
		// Nothing authenticated is added, so an operator pointing the test at
		// their own gateway can supply whatever authorization that gateway wants
		// through the ad-hoc headers instead.
	case req.DryRun:
		// A dry run must show the same header shape without reading credential
		// material, so the two authenticated values are named, not valued.
		base.Set("Authorization", "Bearer [CREDENTIAL]")
		base.Set("Chatgpt-Account-Id", "[ACCOUNT]")
	default:
		base.Set("Authorization", "Bearer "+material.accessToken)
		base.Set("Chatgpt-Account-Id", material.accountID)
	}
	for rawName, value := range req.Headers {
		name := http.CanonicalHeaderKey(strings.TrimSpace(rawName))
		if name == "" || !tokenRE.MatchString(name) {
			return nil, material, badTestRequest("invalid header name %q", rawName)
		}
		if strings.ContainsAny(value, "\r\n") {
			return nil, material, badTestRequest("header %s contains CR/LF", name)
		}
		base.Set(name, value)
	}
	return base, material, nil
}

// parseTestAuthMaterial reads the access token and the ChatGPT account id from
// a credential document.
//
// The account id is only read from id_token.chatgpt_account_id and its explicit
// aliases. The sibling account and id fields are not substitutes: they carry
// the operator's email address, and sending an address as an account id both
// fails upstream and leaks an identity.
func parseTestAuthMaterial(document json.RawMessage) testAuthMaterial {
	material := testAuthMaterial{}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(document, &object); err != nil {
		return material
	}
	material.accessToken = firstStringField(object, "access_token", "accessToken", "AccessToken", "oauth_token", "oauthToken", "token")
	material.accountID = firstStringField(object, "chatgpt_account_id", "chatgptAccountId", "account_id", "accountId", "AccountID")
	for _, nestedName := range []string{"id_token", "idToken", "IDToken", "tokens", "oauth", "credential", "auth", "data"} {
		nestedRaw := firstRawField(object, nestedName)
		if len(nestedRaw) == 0 {
			continue
		}
		var nested map[string]json.RawMessage
		if json.Unmarshal(nestedRaw, &nested) != nil {
			continue
		}
		if material.accessToken == "" {
			material.accessToken = firstStringField(nested, "access_token", "accessToken", "AccessToken", "oauth_token", "oauthToken", "token")
		}
		if material.accountID == "" {
			material.accountID = firstStringField(nested, "chatgpt_account_id", "chatgptAccountId", "account_id", "accountId", "AccountID")
		}
	}
	return material
}

func firstStringField(object map[string]json.RawMessage, names ...string) string {
	raw := firstRawField(object, names...)
	if len(raw) == 0 {
		return ""
	}
	var value string
	if json.Unmarshal(raw, &value) != nil {
		return ""
	}
	return strings.TrimSpace(value)
}

// resolveTestEndpoint accepts any endpoint the operator wants to exercise and
// reports whether it is the Codex backend.
//
// The destination is not restricted, because testing a gateway, a mock, or a
// staging host is a legitimate thing to want. What is restricted is the
// credential: it travels only to the Codex backend unless the operator asks for
// it explicitly (see attachCredential). Transport still has to protect the
// request, so plaintext http is allowed only against loopback, and credentials
// embedded in the URL are rejected outright.
func resolveTestEndpoint(raw string) (endpoint string, codexBackend bool, err error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return defaultTestURL, true, nil
	}
	parsed, parseErr := url.Parse(raw)
	if parseErr != nil {
		return "", false, badTestRequest("endpoint is not a valid URL")
	}
	if !parsed.IsAbs() || parsed.Host == "" {
		return "", false, badTestRequest("endpoint must be an absolute URL with a host")
	}
	if parsed.User != nil {
		return "", false, badTestRequest("endpoint must not embed credentials in the URL")
	}
	switch parsed.Scheme {
	case "https":
	case "http":
		if !isLoopbackHost(parsed.Hostname()) {
			return "", false, badTestRequest("plaintext http is only allowed for loopback hosts")
		}
	default:
		return "", false, badTestRequest("endpoint scheme must be https (or http for loopback)")
	}
	return parsed.String(), strings.EqualFold(parsed.Hostname(), codexBackendHost), nil
}

func isLoopbackHost(hostname string) bool {
	switch strings.ToLower(strings.TrimSpace(hostname)) {
	case "localhost", "127.0.0.1", "::1":
		return true
	default:
		return false
	}
}

// attachCredential decides whether this request carries the credential.
//
// The Codex backend gets it by default because that is what the rule is for.
// Any other destination gets it only when the operator sets attach_credential,
// so pointing the test at a third-party host cannot leak a token by accident.
func attachCredential(req testRequest, codexBackend bool) bool {
	if req.AttachCredential != nil {
		return *req.AttachCredential
	}
	return codexBackend
}

// testWireProfile asks the CPA host to preserve a deterministic HTTP/1.1
// header layout for Codex test calls. The network call still belongs to
// host.http.do (and
// therefore inherits the host's proxy), while custom endpoints retain the
// host's normal transport behavior instead of receiving a Codex-specific
// profile unexpectedly.
func testWireProfile(headers http.Header, codexBackend bool) *hostWireProfile {
	if !codexBackend {
		return nil
	}
	preferred := []string{
		"Host",
		"Content-Type",
		"Authorization",
		"User-Agent",
		"Accept",
		"Chatgpt-Account-Id",
		"Originator",
	}
	seen := make(map[string]struct{}, len(preferred)+len(headers))
	profile := make([]string, 0, len(preferred)+len(headers))
	for _, name := range preferred {
		profile = append(profile, name)
		seen[strings.ToLower(name)] = struct{}{}
	}
	extra := make([]string, 0, len(headers))
	for rawName := range headers {
		name := http.CanonicalHeaderKey(strings.TrimSpace(rawName))
		if name == "" {
			continue
		}
		if _, exists := seen[strings.ToLower(name)]; exists {
			continue
		}
		extra = append(extra, name)
	}
	sort.Slice(extra, func(i, j int) bool {
		return strings.ToLower(extra[i]) < strings.ToLower(extra[j])
	})
	profile = append(profile, extra...)
	return &hostWireProfile{
		HTTP1Only:              true,
		DisableAutoCompression: true,
		HeaderProfile:          profile,
	}
}

// resolveTestBody returns the request body and the model that body sends. A
// raw body override wins, and its own model field is what the mismatch check
// compares against.
func resolveTestBody(req testRequest) ([]byte, string, error) {
	model := strings.TrimSpace(req.Model)
	if raw := strings.TrimSpace(req.Body); raw != "" {
		var declared struct {
			Model string `json:"model"`
		}
		if err := json.Unmarshal([]byte(raw), &declared); err != nil {
			return nil, "", badTestRequest("body is not valid JSON")
		}
		if declared.Model != "" {
			model = strings.TrimSpace(declared.Model)
		}
		return []byte(raw), model, nil
	}
	if model == "" {
		model = defaultTestModel
	}
	prompt := strings.TrimSpace(req.Prompt)
	if prompt == "" {
		prompt = defaultTestPrompt
	}
	instructions := strings.TrimSpace(req.Instructions)
	if instructions == "" {
		instructions = "Return exactly OK."
	}
	effort := strings.ToLower(strings.TrimSpace(req.Effort))
	switch effort {
	case "":
		effort = "low"
	case "minimal", "low", "medium", "high":
	default:
		return nil, "", badTestRequest("reasoning_effort must be minimal, low, medium, or high")
	}
	payload := map[string]any{
		"model":        model,
		"instructions": instructions,
		"input": []map[string]any{{
			"role":    "user",
			"content": []map[string]any{{"type": "input_text", "text": prompt}},
		}},
		"stream":              req.Stream,
		"store":               false,
		"parallel_tool_calls": true,
		"reasoning":           map[string]any{"effort": effort},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, "", err
	}
	return body, model, nil
}

// recordTestAttempt files a test request in the same history the live path
// writes to, marked as a test so the two are never confused.
func recordTestAttempt(req testRequest, result testResult, auth hostAuthFileEntry, started time.Time) string {
	state.mu.Lock()
	writer := state.writer
	state.mu.Unlock()
	if writer == nil {
		return ""
	}
	snapshot := snapshotFromEntry(auth)
	id := fmt.Sprintf("test-%d#1", started.UnixNano())
	record := historyRecord{
		ID:              id,
		RequestID:       id,
		Attempt:         1,
		AuthIndex:       result.AuthIndex,
		AuthID:          snapshot.AuthID,
		CredentialName:  snapshot.Name,
		CredentialLabel: snapshot.Label,
		Model:           result.SentModel,
		RequestedModel:  strings.TrimSpace(req.Model),
		SourceFormat:    "plugin_test",
		Stream:          req.Stream,
		StartedAt:       started,
		CompletedAt:     time.Now().UTC(),
		BeforeHeaders:   result.BeforeHeaders,
		AfterHeaders:    result.AfterHeaders,
		ResponseHeaders: result.ResponseHeaders,
		StatusCode:      result.StatusCode,
		Outcome:         result.Outcome,
		Error:           result.Error,
		UpstreamModel:   result.UpstreamModel,
		ModelMismatch:   result.ModelMismatch,
		ModelConflict:   result.ModelConflict,
		Origin:          originTest,
	}
	writer.Enqueue(record)
	return id
}

func truncateText(text string, limit int) string {
	text = strings.TrimSpace(text)
	if limit <= 0 || text == "" {
		return ""
	}
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit]) + "…"
}
