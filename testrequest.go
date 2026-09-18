package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"runtime"
	"strings"
	"time"
)

const (
	testEndpointHost   = "chatgpt.com"
	testEndpointPrefix = "/backend-api/codex/"
	defaultTestURL     = "https://chatgpt.com/backend-api/codex/responses"
	defaultTestModel   = "gpt-5.6-luna"
	defaultTestPrompt  = "Reply with exactly OK"
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
}

type testResult struct {
	AuthIndex       string      `json:"auth_index"`
	DryRun          bool        `json:"dry_run"`
	Endpoint        string      `json:"endpoint"`
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
	endpoint, err := resolveTestEndpoint(req.Endpoint)
	if err != nil {
		return testResult{}, err
	}
	body, model, err := resolveTestBody(req)
	if err != nil {
		return testResult{}, err
	}

	rule, applyRule, err := resolveTestRule(authIndex, req)
	if err != nil {
		return testResult{}, err
	}

	result := testResult{
		AuthIndex:   authIndex,
		DryRun:      req.DryRun,
		Endpoint:    endpoint,
		Method:      http.MethodPost,
		SentModel:   model,
		RuleApplied: applyRule,
	}

	started := time.Now().UTC()
	base, material, err := testBaseHeaders(req, authIndex)
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
	if material.accessToken == "" {
		return testResult{}, &testRequestError{status: http.StatusBadGateway, message: "credential has no usable access token"}
	}

	// Timed around the upstream call only: reading the credential through the
	// host is plugin overhead, not upstream latency.
	callStarted := time.Now()
	response, callErr := hostHTTPDoFunc(hostHTTPRequest{Method: http.MethodPost, URL: endpoint, Headers: outgoing, Body: body})
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
// Authentication material is read here and used for the single call that
// follows. Returned header maps are redacted before they leave the plugin.
func testBaseHeaders(req testRequest, authIndex string) (http.Header, testAuthMaterial, error) {
	material := testAuthMaterial{}
	if !req.DryRun {
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
	if req.DryRun {
		// A dry run must show the same header shape without reading credential
		// material, so the two authenticated values are named, not valued.
		base.Set("Authorization", "Bearer [CREDENTIAL]")
		base.Set("Chatgpt-Account-Id", "[ACCOUNT]")
	} else {
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

// resolveTestEndpoint keeps a test request pointed at the Codex backend. The
// path is adjustable so an operator can exercise a different Codex route, but
// the scheme and host are fixed: the request carries a bearer token, and an
// arbitrary destination would hand that token to a third party.
func resolveTestEndpoint(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return defaultTestURL, nil
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", badTestRequest("endpoint is not a valid URL")
	}
	if parsed.Scheme != "https" || parsed.Host != testEndpointHost || !strings.HasPrefix(parsed.Path, testEndpointPrefix) {
		return "", badTestRequest("endpoint must be an https://%s%s… URL", testEndpointHost, testEndpointPrefix)
	}
	return parsed.String(), nil
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
