package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

// A degraded response can be answered by asking the same credential for a
// fresh turn state: the state arrives in a response header, so a minimal
// request is enough and the body is never read.
//
// The credential is the one that served the rejected response, not whichever
// one a new request would land on. That matters because the pool is keyed by
// credential and model: a state minted under a different account is exactly
// the cross-account echo the guard exists to catch. Pinning the credential
// means reading its material, which follows the same rules as the test
// request -- read just in time for one call, never logged, never persisted,
// never returned to the panel, and only ever sent to the Codex backend.
const (
	maxRetryAttempts     = 5
	defaultRetryAttempts = 2
	retryPrompt          = "hi"
	retryGap             = 400 * time.Millisecond
	retryInFlightLimit   = 4
	// Long enough for a short series of minimal requests, short enough that a
	// stuck retry never becomes a stuck client.
	retryWaitTimeout = 25 * time.Second
)

var retryInFlight = make(chan struct{}, retryInFlightLimit)

// retryOutcome is what one attempt produced. The blob is held only long
// enough to classify and pool it.
type retryOutcome struct {
	statusCode  int
	blob        string
	nonDegraded bool
	err         string
	plan        string
	stop        bool
	// What actually went out and came back, so the detail can show the retry
	// the same way it shows a proxied request. Redacted before it is stored.
	sentHeaders     http.Header
	responseHeaders http.Header
}

func retryAttemptCount(rule headerRule) int {
	if rule.RetryAttempts > 0 {
		return min(rule.RetryAttempts, maxRetryAttempts)
	}
	return defaultRetryAttempts
}

// scheduleDegradedRetry runs the series off the response path so the client is
// never waiting on it. state.mu must be held by the caller.
// scheduleDegradedRetry starts the series and returns a channel that closes
// when it ends -- including when it never starts, so a caller can always wait
// on it without checking why.
func scheduleDegradedRetry(attempt *pendingAttempt, attempts int) chan struct{} {
	done := make(chan struct{})
	if state.quiescing || state.store == nil || attempt.AuthID == "" {
		close(done)
		return nil
	}
	if state.retryStop == nil {
		state.retryStop = make(chan struct{})
	}
	stop := state.retryStop
	select {
	case retryInFlight <- struct{}{}:
	default:
		close(done)
		return nil
	}
	state.retryWG.Add(1)
	authIndex, authID := attempt.AuthIndex, attempt.AuthID
	model := sentModel(attempt.Model, attempt.RequestedModel)
	label, name, plan := attempt.CredentialLabel, attempt.CredentialName, attempt.CredentialPlan
	go func() {
		defer close(done)
		defer state.retryWG.Done()
		defer func() { <-retryInFlight }()
		runDegradedRetry(authIndex, authID, label, name, model, plan, attempts, stop)
	}()
	return done
}

// waitForDegradedRetry holds a withheld response until the series ends.
// Returning the error first sends the client straight back at an upstream
// whose pool has not been refilled, so its own retry is degraded too. The wait
// is capped and also ends at quiesce: a client must never hang on the plugin.
func waitForDegradedRetry(done chan struct{}) {
	if done == nil {
		return
	}
	timer := time.NewTimer(retryWaitTimeout)
	defer timer.Stop()
	state.mu.Lock()
	stop := state.retryStop
	state.mu.Unlock()
	select {
	case <-done:
	case <-stop:
	case <-timer.C:
	}
}

// runDegradedRetry makes up to attempts requests, stopping at the first
// non-degraded state, and records the whole series as one history row.
func runDegradedRetry(authIndex, authID, label, name, model, plan string, attempts int, stop <-chan struct{}) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-stop:
			cancel()
		case <-ctx.Done():
		}
	}()
	started := time.Now().UTC()
	var (
		used   int
		last   retryOutcome
		pooled bool
		info   turnStateInfo
	)
	for used < attempts {
		if used > 0 {
			timer := time.NewTimer(retryGap)
			select {
			case <-stop:
				timer.Stop()
				return
			case <-timer.C:
			}
		}
		select {
		case <-stop:
			return
		default:
		}
		used++
		last = retryOnce(ctx, authIndex, authID, model)
		select {
		case <-stop:
			return
		default:
		}
		if last.stop {
			break
		}
		if last.err != "" || last.blob == "" {
			continue
		}
		state.mu.Lock()
		if state.quiescing {
			state.mu.Unlock()
			return
		}
		classified := classifyTurnState(decodeTurnState(last.blob), last.blob, last.plan)
		classified.Pooled = noteTurnStateMintLocked(last.blob, authIndex, label, model, last.plan)
		state.mu.Unlock()
		info = classified
		if last.nonDegraded {
			pooled = classified.Pooled
			break
		}
	}
	recordRetrySeries(retrySeries{
		authIndex: authIndex, authID: authID, label: label, name: name,
		model: model, attempts: used, started: started, last: last,
		pooled: pooled, info: info,
	})
}

// retryOnce sends one minimal Codex request and reports only what the
// response header said. The body is discarded unread.
func retryOnce(ctx context.Context, authIndex, authID, model string) retryOutcome {
	if !retryIdentityMatches(authIndex, authID) {
		return retryOutcome{err: "credential identity changed or unavailable", stop: true}
	}
	document, err := hostAuthGetFunc(authIndex)
	if err != nil {
		return retryOutcome{err: "credential is not readable through the host"}
	}
	material := parseTestAuthMaterial(document)
	plan := credentialPlanType(document)
	if plan == "" {
		return retryOutcome{err: "credential plan is unknown", stop: true}
	}
	if !retryIdentityMatches(authIndex, authID) {
		return retryOutcome{err: "credential identity changed or unavailable", stop: true}
	}
	if material.accessToken == "" {
		return retryOutcome{err: "credential has no usable access token"}
	}
	headers := retryHeaders(material)
	body, err := json.Marshal(retryPayload(model))
	if err != nil {
		return retryOutcome{err: err.Error()}
	}
	response, callErr := retryHTTPDoFunc(ctx, hostHTTPRequest{
		Method:  http.MethodPost,
		URL:     defaultTestURL,
		Headers: headers,
		Body:    body,
	}, retryProxyFor(authIndex))
	sent := redactHeaders(headers)
	got := redactHeaders(response.Headers)
	if callErr != nil {
		return retryOutcome{statusCode: response.StatusCode, err: callErr.Error(), sentHeaders: sent, responseHeaders: got}
	}
	if !retryIdentityMatches(authIndex, authID) {
		return retryOutcome{err: "credential identity changed during retry", stop: true}
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return retryOutcome{statusCode: response.StatusCode, err: fmt.Sprintf("retry returned HTTP %d", response.StatusCode), sentHeaders: sent, responseHeaders: got}
	}
	blob := headerTurnState(response.Headers)
	if blob == "" {
		return retryOutcome{statusCode: response.StatusCode, err: fmt.Sprintf("no %s in the response (HTTP %d)", turnStateHeader, response.StatusCode), sentHeaders: sent, responseHeaders: got}
	}
	nonDegraded, _, knownPlan := nonDegradedTurnState(blob, plan)
	return retryOutcome{statusCode: response.StatusCode, blob: blob, nonDegraded: knownPlan && nonDegraded, plan: plan, sentHeaders: sent, responseHeaders: got}
}

func retryIdentityMatches(authIndex, authID string) bool {
	if authID == "" {
		return false
	}
	runtime, err := hostAuthGetRuntimeFunc(authIndex)
	return err == nil && runtime.Auth.ID == authID && isCodexCredential(runtime.Auth.Provider, runtime.Auth.Type)
}

// retryHeaders is the smallest set the Codex backend needs. Nothing about the
// operator or the original request is copied in.
func retryHeaders(material testAuthMaterial) http.Header {
	headers := http.Header{
		"Content-Type": {"application/json"},
		"Accept":       {"text/event-stream"},
		"Originator":   {"codex-cli"},
	}
	headers.Set("Authorization", "Bearer "+material.accessToken)
	if material.accountID != "" {
		headers.Set("Chatgpt-Account-Id", material.accountID)
	}
	return headers
}

func retryPayload(model string) map[string]any {
	return map[string]any{
		"model":               model,
		"instructions":        "You are Codex, a coding agent.",
		"input":               []any{map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": retryPrompt}}}},
		"stream":              true,
		"store":               false,
		"tools":               []any{},
		"parallel_tool_calls": false,
	}
}

type retrySeries struct {
	authIndex, authID, label, name, model string
	attempts                              int
	started                               time.Time
	last                                  retryOutcome
	pooled                                bool
	info                                  turnStateInfo
}

var retryRecordSeq struct {
	sync.Mutex
	n int64
}

// recordRetrySeries writes one row for the series, so a run of retries reads
// as a single event in the history rather than as several requests.
func recordRetrySeries(s retrySeries) {
	state.mu.Lock()
	defer state.mu.Unlock()
	writer := state.writer
	if state.quiescing || writer == nil || s.attempts == 0 {
		return
	}
	retryRecordSeq.Lock()
	retryRecordSeq.n++
	id := fmt.Sprintf("retry-%d-%d#1", s.started.UnixNano(), retryRecordSeq.n)
	retryRecordSeq.Unlock()

	outcome := "failed"
	if s.pooled {
		outcome = "succeeded"
	}
	record := historyRecord{
		ID: id, RequestID: id, Attempt: 1,
		AuthIndex: s.authIndex, AuthID: s.authID,
		CredentialName: s.name, CredentialLabel: s.label,
		Model: s.model, RequestedModel: s.model,
		SourceFormat: "plugin_retry", Stream: true,
		StartedAt: s.started, CompletedAt: time.Now().UTC(),
		StatusCode: s.last.statusCode, Outcome: outcome,
		Error: s.last.err, Origin: originRetry,
		RetryAttempts: s.attempts,
		// The plugin does not rewrite its own retry, so both views of the
		// request are the headers it sent.
		BeforeHeaders:   s.last.sentHeaders,
		AfterHeaders:    s.last.sentHeaders,
		ResponseHeaders: s.last.responseHeaders,
	}
	if s.info.Digest != "" {
		info := s.info
		record.TurnStateMinted = &info
	}
	writer.Enqueue(record)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// retryEnabledFor reports the attempt count for a credential whose rule asks
// for retries; zero means the feature is off for it.
func retryEnabledForLocked(authIndex string) int {
	rule, ok := state.rules[authIndex]
	if !ok || !rule.RejectDegradedResponse || !rule.RetryOnDegraded {
		return 0
	}
	if strings.TrimSpace(authIndex) == "" {
		return 0
	}
	return retryAttemptCount(rule)
}
