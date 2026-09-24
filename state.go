package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

type pluginState struct {
	mu          sync.Mutex
	retryStop   chan struct{}
	retryWG     sync.WaitGroup
	quiescing   bool
	store       persistence
	writer      *queuedPersistence
	dataPath    string
	rules       map[string]headerRule
	pending     map[string]*pendingRequest
	credentials map[string]credentialSnapshot
	// turnStates maps a blob digest to the credential that returned it. It is a
	// short-lived correlation index rebuilt from the durable state pool.
	turnStates map[string]turnStateOrigin
	// turnStateLatest holds the newest blob per credential and model, which is
	// the one a client could still legitimately be echoing.
	turnStateLatest map[string]turnStateOrigin
	turnStateWrites uint64
}

var state = &pluginState{rules: map[string]headerRule{}, pending: map[string]*pendingRequest{}, credentials: map[string]credentialSnapshot{}, turnStates: map[string]turnStateOrigin{}, turnStateLatest: map[string]turnStateOrigin{}}

func configurePlugin(raw []byte) error {
	var req lifecycleRequest
	if len(raw) > 0 {
		if err := unmarshalJSON(raw, &req); err != nil {
			return err
		}
	}
	if req.SchemaVersion != 0 && req.SchemaVersion < schemaVersion {
		return fmt.Errorf("%s requires plugin schema version %d or newer", pluginName, schemaVersion)
	}
	cfg := parsePluginConfig(req.ConfigYAML)
	if cfg.DataPath == "" {
		cfg.DataPath = defaultDataPath
	}
	state.mu.Lock()
	changing := state.store != nil && state.dataPath != cfg.DataPath
	state.mu.Unlock()
	if changing {
		if err := quiescePlugin(); err != nil {
			return err
		}
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.store != nil && state.dataPath == cfg.DataPath {
		return nil
	}
	if state.writer != nil {
		_ = state.writer.Close()
		state.writer = nil
		state.store = nil
	}
	backend, err := openPersistence(cfg.DataPath)
	if err != nil {
		return fmt.Errorf("open persistence: %w", err)
	}
	rules, err := backend.ListRules()
	if err != nil {
		_ = backend.Close()
		return fmt.Errorf("load rules: %w", err)
	}
	storedTurnStates, err := backend.ListTurnStates()
	if err != nil {
		_ = backend.Close()
		return fmt.Errorf("load turn states: %w", err)
	}
	state.store = backend
	state.quiescing = false
	state.retryStop = make(chan struct{})
	state.writer = newQueuedPersistence(backend)
	startProbeScheduler(state.retryStop)
	state.dataPath = cfg.DataPath
	state.rules = make(map[string]headerRule, len(rules))
	state.pending = map[string]*pendingRequest{}
	state.turnStates = map[string]turnStateOrigin{}
	state.turnStateLatest = map[string]turnStateOrigin{}
	for _, r := range rules {
		state.rules[r.AuthIndex] = r
	}
	for _, stored := range storedTurnStates {
		origin, ok := restoreTurnState(stored)
		if !ok {
			continue
		}
		state.turnStates[origin.digest] = origin
		key := turnStateLatestKey(origin.authIndex, origin.model)
		if previous, exists := state.turnStateLatest[key]; !exists || !previous.mintedAt.After(origin.mintedAt) {
			state.turnStateLatest[key] = origin
		}
	}
	return nil
}

func parsePluginConfig(raw []byte) pluginConfig {
	cfg := pluginConfig{DataPath: defaultDataPath}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok || strings.TrimSpace(key) != "data_path" {
			continue
		}
		value = strings.Trim(strings.TrimSpace(value), "\"'")
		if value != "" {
			cfg.DataPath = value
		}
	}
	return cfg
}

// quiescePlugin releases process-external resources before the host loads a
// replacement .so. In particular, bbolt holds an exclusive file lock: if the
// old instance keeps it while the new instance registers, hot reload waits
// forever and also blocks later uninstall operations behind the host apply
// lock. The host can re-register this instance after a failed replacement, in
// which case configurePlugin opens the store again.
func quiescePlugin() error {
	state.mu.Lock()
	state.quiescing = true
	if state.retryStop != nil {
		close(state.retryStop)
		state.retryStop = nil
	}
	state.mu.Unlock()
	state.retryWG.Wait()
	state.mu.Lock()
	writer := state.writer
	store := state.store
	state.writer = nil
	state.store = nil
	state.pending = map[string]*pendingRequest{}
	state.mu.Unlock()
	if writer != nil {
		return writer.Close()
	}
	if store != nil {
		return store.Close()
	}
	return nil
}

func shutdownPlugin() {
	_ = quiescePlugin()
}

func getRule(authIndex string) (headerRule, bool) {
	state.mu.Lock()
	defer state.mu.Unlock()
	r, ok := state.rules[authIndex]
	return r, ok
}
func saveRule(rule headerRule) (headerRule, error) {
	normalized, err := validateRule(rule)
	if err != nil {
		return rule, err
	}
	normalized.UpdatedAt = time.Now().UTC()
	state.mu.Lock()
	previous, existed := state.rules[normalized.AuthIndex]
	state.mu.Unlock()
	// The auth file is edited first, outside the lock -- it is a host call --
	// and a failure refuses the whole change, so the switch never claims a
	// forwarding the file does not carry.
	before, after := cookieForwardingWanted(previous, existed), cookieForwardingWanted(normalized, true)
	if before != after {
		if err := setCookieForwarding(normalized.AuthIndex, after); err != nil {
			return rule, fmt.Errorf("cookie forwarding: %w", err)
		}
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.store == nil {
		return rule, fmt.Errorf("persistence is not initialized")
	}
	if err := state.store.SaveRule(normalized); err != nil {
		if before != after {
			_ = setCookieForwarding(normalized.AuthIndex, before)
		}
		return rule, err
	}
	state.rules[normalized.AuthIndex] = normalized
	rescheduleProbe(normalized.AuthIndex)
	return normalized, nil
}
func deleteRule(authIndex string) error {
	state.mu.Lock()
	previous, existed := state.rules[authIndex]
	state.mu.Unlock()
	if cookieForwardingWanted(previous, existed) {
		if err := setCookieForwarding(authIndex, false); err != nil {
			return fmt.Errorf("cookie forwarding: %w", err)
		}
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.store == nil {
		return fmt.Errorf("persistence is not initialized")
	}
	if err := state.store.DeleteRule(authIndex); err != nil {
		return err
	}
	delete(state.rules, authIndex)
	rescheduleProbe(authIndex)
	return nil
}

func selectedMetadata(meta map[string]any) (string, string) {
	if meta == nil {
		return "", ""
	}
	ai, _ := meta["selected_auth_index"].(string)
	id, _ := meta["selected_auth_id"].(string)
	return strings.TrimSpace(ai), strings.TrimSpace(id)
}

func interceptAfter(req requestInterceptRequest) (requestInterceptResponse, error) {
	authIndex, authID := selectedMetadata(req.Metadata)
	if authIndex == "" {
		return requestInterceptResponse{}, nil
	}
	cred, isCodex := resolveCodexCredential(authIndex, authID)
	state.mu.Lock()
	pr := state.pending[req.RequestID]
	if pr == nil {
		pr = &pendingRequest{}
		state.pending[req.RequestID] = pr
	}
	if pr.current != nil {
		pr.current.Outcome = "switched"
		pr.current.CompletedAt = time.Now().UTC()
		finalizeLocked(pr.current)
		pr.current = nil
	}
	if !isCodex {
		state.mu.Unlock()
		return requestInterceptResponse{}, nil
	}
	pr.attempts++
	rule, hasRule := state.rules[authIndex]
	before := cloneHeader(req.Headers)
	after := cloneHeader(req.Headers)
	var updates http.Header
	var clears []string
	if hasRule && rule.Enabled {
		after = applyRuleToHeaders(req.Headers, rule)
		updates = make(http.Header, len(rule.Set))
		for k, v := range rule.Set {
			updates.Set(k, v)
		}
		clears = append([]string(nil), rule.Remove...)
	}
	echo, echoed := evaluateTurnStateEchoLocked(req.Headers, authIndex, sentModel(req.Model, req.RequestedModel))
	// The pool has its own two switches, on the State 池 card: the pool's master
	// switch and the injection switch. Neither is part of header rewriting, so
	// a credential whose rule is off still injects when both are on.
	injecting := hasRule && rule.InjectTurnState && rule.poolMaintained()
	stripped := false
	if echoed && echo.unusable() && injecting {
		// The blob came from another credential or model, so
		// no upstream turn chain can accept it here. It is dropped from this
		// request and the recorded "after" view shows it gone.
		clears = append(clears, turnStateHeader)
		deleteHeaderFold(after, turnStateHeader)
		stripped = true
	}
	// Within an enabled rule, anything the operator said about this header by
	// hand outranks the pool -- a pinned value stays pinned, and a removal stays
	// removed rather than being quietly refilled. A disabled rule pins nothing,
	// so its Set and Remove do not hold the pool back.
	pinned := hasRule && rule.Enabled && ruleMentionsHeader(rule, turnStateHeader)
	injected := false
	var injectedFrom turnStateOrigin
	if injecting && !pinned {
		if pooled, ok := turnStateForInjectionLocked(authIndex, sentModel(req.Model, req.RequestedModel), cred.PlanType); ok {
			injectedFrom = pooled
			if updates == nil {
				updates = make(http.Header)
			}
			updates.Set(turnStateHeader, pooled.blob)
			// With the rule off, nothing above has built the "after" view yet.
			if after == nil {
				after = make(http.Header)
			}
			deleteHeaderFold(after, turnStateHeader)
			after.Set(turnStateHeader, pooled.blob)
			// The guard may have queued this header for removal a moment ago.
			// Clearing and setting the same header in one response is undefined,
			// so the removal is withdrawn in favour of the replacement.
			clears = removeHeaderNameFold(clears, turnStateHeader)
			injected = true
		}
	}
	// The cookie switch is the state switch's twin: same pool, same entry, same
	// master switch, and a Cookie the operator set or removed by hand in an
	// enabled rule outranks it. The two do not depend on each other.
	injectingCookie := hasRule && rule.InjectCookie && rule.poolMaintained()
	cookiePinned := hasRule && rule.Enabled && ruleMentionsHeader(rule, "Cookie")
	outboundCookie := ""
	if injectingCookie && !cookiePinned {
		if pooled, ok := turnStateForInjectionLocked(authIndex, sentModel(req.Model, req.RequestedModel), cred.PlanType); ok && pooled.cookie != "" {
			if after == nil {
				after = make(http.Header)
			}
			outboundCookie = mergeCookieHeader(joinCookieHeader(after), pooled.cookie)
			if updates == nil {
				updates = make(http.Header)
			}
			updates.Set("Cookie", outboundCookie)
			deleteHeaderFold(after, "Cookie")
			after.Set("Cookie", outboundCookie)
			clears = removeHeaderNameFold(clears, "Cookie")
		}
	}
	pr.current = &pendingAttempt{historyRecord: historyRecord{ID: fmt.Sprintf("%s#%d", req.RequestID, pr.attempts), RequestID: req.RequestID, Attempt: pr.attempts, AuthIndex: authIndex, AuthID: authID, CredentialName: cred.Name, CredentialLabel: cred.Label, CredentialPlan: cred.PlanType, Model: req.Model, RequestedModel: req.RequestedModel, SourceFormat: req.SourceFormat, Stream: req.Stream, StartedAt: time.Now().UTC(), BeforeHeaders: redactHeaders(before), AfterHeaders: redactHeaders(after), Outcome: "in_flight", Origin: originLive}}
	if echoed {
		info := echo.info
		pr.current.TurnStateEcho = &info
		pr.current.TurnStateOriginIndex = echo.originIndex
		pr.current.TurnStateOriginLabel = echo.originLabel
		pr.current.TurnStateStripped = stripped
		pr.current.TurnStateExpired = echo.expired
		pr.current.TurnStateAgeSeconds = echo.ageSeconds
		if echo.known {
			cross := echo.crossAccount
			crossModel := echo.crossModel
			pr.current.TurnStateCrossAccount = &cross
			pr.current.TurnStateCrossModel = &crossModel
			pr.current.TurnStateOriginModel = echo.originModel
		}
	}
	pr.current.TurnStateInjected = injected
	if injected {
		pr.current.injectedDigest = injectedFrom.digest
	}
	pr.current.TurnStateSessionID = clientSessionID(req.Headers)
	pr.current.clientCookie = joinCookieHeader(req.Headers)
	// With the pooled session merged in, that is the session the upstream
	// sees, so it is the one a state minted by this response belongs to.
	if outboundCookie != "" {
		pr.current.clientCookie = outboundCookie
		pr.current.CookieInjected = true
	}
	// The payload as it goes upstream. This plugin rewrites headers and never
	// the body, so what arrives here is what is sent.
	pr.current.requestBody, pr.current.requestBytes = maskRequestBody(req.Body), len(req.Body)
	pr.current.RequestEffort = requestThinkingLevel(req.Body, req.SourceFormat, sentModel(req.Model, req.RequestedModel))
	state.mu.Unlock()
	return requestInterceptResponse{Headers: updates, ClearHeaders: clears}, nil
}

// ruleMentionsHeader reports whether the operator named this header in the
// rule, either by pinning a value or by removing it.
func ruleMentionsHeader(rule headerRule, target string) bool {
	for name := range rule.Set {
		if strings.EqualFold(strings.TrimSpace(name), target) {
			return true
		}
	}
	for _, name := range rule.Remove {
		if strings.EqualFold(strings.TrimSpace(name), target) {
			return true
		}
	}
	return false
}

func removeHeaderNameFold(names []string, target string) []string {
	out := names[:0]
	for _, name := range names {
		if !strings.EqualFold(name, target) {
			out = append(out, name)
		}
	}
	return out
}

func observeResponse(req responseInterceptRequest) responseInterceptResponse {
	state.mu.Lock()
	pr := state.pending[req.RequestID]
	if pr == nil || pr.current == nil {
		state.mu.Unlock()
		return responseInterceptResponse{}
	}
	pr.current.ResponseHeaders = redactHeaders(req.ResponseHeaders)
	pr.current.StatusCode = req.StatusCode
	noteTurnStateMintLocked2(pr.current, req.ResponseHeaders)
	// The body is read here only to learn which model the upstream served. The
	// model name is kept; the body itself is not stored anywhere.
	pr.current.models.observeBody(req.Body)
	pr.current.responseBody, pr.current.responseBytes = appendBody(pr.current.responseBody, pr.current.responseBytes, req.Body)
	rejecting, done, info := pr.current.rejecting, pr.current.retryDone, pr.current.TurnStateMinted
	// Released before waiting: a withheld response must not hold every other
	// request that is still in flight.
	state.mu.Unlock()
	if rejecting {
		waitForDegradedRetry(done)
		return responseInterceptResponse{
			Headers: rejectionHeaders("application/json"),
			Body:    rejectionBody(info),
		}
	}
	return responseInterceptResponse{}
}

// rejectionHeaders marks a withheld response so the client and the operator
// can tell it apart from an upstream error. The status code is out of reach.
func rejectionHeaders(contentType string) http.Header {
	return http.Header{
		"Content-Type":           {contentType},
		"X-Codex-Header-Rewrite": {"rejected-degraded-turn-state"},
	}
}

func rejectionMessage(info *turnStateInfo) string {
	if info != nil && info.MaxChars > 0 {
		return fmt.Sprintf("codex-header-rewrite withheld this response: the upstream X-Codex-Turn-State classifies as degraded (%d characters, limit %d for this plan). Retry the request.", info.Chars, info.MaxChars)
	}
	return "codex-header-rewrite withheld this response: the upstream returned a new X-Codex-Turn-State over the one this request carried, which means the turn was degraded. Retry the request."
}

// rejectionBody is the non-stream replacement: an error object in the shape
// the Responses API uses for failures.
func rejectionBody(info *turnStateInfo) []byte {
	body, _ := json.Marshal(map[string]any{"error": map[string]any{
		"type": "degraded_turn_state", "code": "turn_state_degraded", "message": rejectionMessage(info), "param": nil,
	}})
	return body
}

// rejectionEvent is the stream replacement: a terminal `error` event, which
// Codex clients treat as the end of the response.
func rejectionEvent(info *turnStateInfo) []byte {
	data, _ := json.Marshal(map[string]any{
		"type": "error", "code": "turn_state_degraded", "message": rejectionMessage(info), "param": nil, "sequence_number": 0,
	})
	return []byte("event: error\ndata: " + string(data) + "\n\n")
}

// noteTurnStateMintLocked2 classifies an upstream response state, mints it into
// the pool only when qualified, and attaches the result to history.
func noteTurnStateMintLocked2(attempt *pendingAttempt, responseHeaders http.Header) {
	blob := headerTurnState(responseHeaders)
	// The session this state belongs to: what the request presented, updated by
	// what this very response set. A response that rotates the session mints a
	// state under the new one, not the old one. Both this and the allowance are
	// read from every response -- under the injection rule a healthy turn
	// returns no state at all, and those responses still carry both.
	session := applySetCookies(attempt.clientCookie, responseHeaders)
	rememberLiveSessionLocked(attempt, session)
	noteQuotaLocked(attempt.AuthIndex, responseHeaders)
	verdict := judgeInjectedTurn(attempt.TurnStateInjected, blob != "")
	if blob == "" {
		// Nothing was minted, and under the injection rule that silence is the
		// verdict: the turn kept the state it went out with.
		attempt.TurnStateHeld = verdict.Judged && !verdict.Degraded
		// The session that carried the healthy turn is a non-degraded cookie.
		if attempt.TurnStateHeld {
			noteCookiePoolLocked(attempt.AuthIndex, session, "live", sentModel(attempt.Model, attempt.RequestedModel), attempt.injectedDigest)
		}
		return
	}
	info := classifyTurnStateWith(decodeTurnState(blob), attempt.CredentialPlan, verdict)
	label := attempt.CredentialLabel
	if label == "" {
		label = attempt.CredentialName
	}
	// A frozen pool takes nothing in. The state is still classified so the
	// history row says what the upstream sent; it just does not enter the pool.
	if rule, ok := state.rules[attempt.AuthIndex]; !ok || rule.poolMaintained() {
		// A live state never pools itself. Either the rule judged it degraded,
		// or it judged nothing at all and an operator decides. Where it came
		// from is recorded either way, so a later echo can still be traced.
		info.ManualPool = !verdict.Degraded
		info.Pooled = mintTurnStateLocked(blob, attempt.AuthIndex, label, sentModel(attempt.Model, attempt.RequestedModel), attempt.CredentialPlan, session, mintRecordOnly)
	}
	attempt.TurnStateMinted = &info
	// The pooled state went out on this request and the upstream minted one
	// anyway: that state has stopped carrying the chain, and left in the pool
	// it would go out again on the next request. Age does not enter into it --
	// the reuse window only says when a state is due for renewal.
	if attempt.injectedDigest != "" && info.NonDegraded != nil && !*info.NonDegraded {
		attempt.TurnStateInvalidated = invalidateTurnStateLocked(attempt.AuthIndex, sentModel(attempt.Model, attempt.RequestedModel), attempt.injectedDigest)
		// The cookie that went out with it is as suspect as the state: its
		// backend's entry in the Cookie 池 stops being drawn.
		if attempt.CookieInjected {
			attempt.CookieInvalidated = invalidateCookiePoolLocked(attempt.AuthIndex, attempt.clientCookie, "live")
		}
	}
	// Response rejection has its own switch and model scope, independent of
	// request rewriting. Both streaming and non-streaming use this decision.
	degraded := info.NonDegraded != nil && !*info.NonDegraded
	if rule, ok := state.rules[attempt.AuthIndex]; ok && degraded && rejectsDegradedModel(rule, sentModel(attempt.Model, attempt.RequestedModel)) {
		attempt.rejecting = true
		attempt.TurnStateRejected = true
	}
	// The retry answers the same condition the interception does, but it runs
	// off the response path and reports as its own history row.
	if attempt.rejecting && !attempt.retryScheduled {
		if attempts := retryEnabledForLocked(attempt.AuthIndex); attempts > 0 {
			attempt.retryScheduled = true
			// This very response may have rotated or issued the session cookie,
			// so the retry sends what the request carried updated by what the
			// response set -- not the stale copy the request came in with.
			attempt.clientCookie = applySetCookies(attempt.clientCookie, responseHeaders)
			attempt.retryDone = scheduleDegradedRetry(attempt, attempts)
		}
	}
}
func observeStreamHeaders(req streamChunkInterceptRequest) streamChunkInterceptResponse {
	state.mu.Lock()
	pr := state.pending[req.RequestID]
	if pr == nil || pr.current == nil {
		state.mu.Unlock()
		return streamChunkInterceptResponse{}
	}
	if req.ChunkIndex == streamChunkHeaderInitIndex {
		pr.current.ResponseHeaders = redactHeaders(req.ResponseHeaders)
		noteTurnStateMintLocked2(pr.current, req.ResponseHeaders)
		rejecting := pr.current.rejecting
		state.mu.Unlock()
		if rejecting {
			return streamChunkInterceptResponse{Headers: rejectionHeaders("text/event-stream")}
		}
		return streamChunkInterceptResponse{}
	}
	if pr.current.rejecting {
		// Withholding a response from the client is no reason to stop reading
		// it: the chunk still arrives here, and the history row for an
		// intercepted request is exactly where the upstream's own answer is
		// worth having.
		pr.current.models.observeCallback(req.Body)
		pr.current.responseBody, pr.current.responseBytes = appendBody(pr.current.responseBody, pr.current.responseBytes, req.Body)
		// The first payload chunk becomes the terminal error; nothing of the
		// upstream body reaches the client after that.
		pr.current.rejectedChunks++
		first := pr.current.rejectedChunks == 1
		done, info := pr.current.retryDone, pr.current.TurnStateMinted
		state.mu.Unlock()
		if first {
			// Hold the terminal event until the pool has been refilled, so the
			// client's own retry is not sent straight back into a degraded turn.
			waitForDegradedRetry(done)
			return streamChunkInterceptResponse{Body: rejectionEvent(info)}
		}
		return streamChunkInterceptResponse{DropChunk: true}
	}
	pr.current.models.observeCallback(req.Body)
	pr.current.responseBody, pr.current.responseBytes = appendBody(pr.current.responseBody, pr.current.responseBytes, req.Body)
	state.mu.Unlock()
	return streamChunkInterceptResponse{}
}
func completeRequest(c requestCompletion) {
	state.mu.Lock()
	defer state.mu.Unlock()
	pr := state.pending[c.RequestID]
	if pr == nil {
		return
	}
	if pr.current != nil {
		pr.current.Outcome = c.Outcome
		if c.StatusCode != 0 {
			pr.current.StatusCode = c.StatusCode
		}
		pr.current.Error = c.Error
		pr.current.CompletedAt = c.CompletedAt.UTC()
		if pr.current.CompletedAt.IsZero() {
			pr.current.CompletedAt = time.Now().UTC()
		}
		if !c.StartedAt.IsZero() {
			pr.current.StartedAt = c.StartedAt.UTC()
		}
		finalizeLocked(pr.current)
	}
	delete(state.pending, c.RequestID)
}
func finalizeLocked(attempt *pendingAttempt) {
	if attempt == nil || attempt.persisted {
		return
	}
	attempt.persisted = true
	attempt.models.flushStream()
	attempt.UpstreamModel = attempt.models.model()
	attempt.ModelMismatch = modelMismatch(sentModel(attempt.Model, attempt.RequestedModel), attempt.UpstreamModel)
	attempt.ModelConflict = attempt.models.conflicted()
	attempt.UpstreamEffort = attempt.models.effort()
	// Masked once the whole stream is in hand: a frame split across two chunks
	// would slip through a per-chunk mask.
	attempt.RequestBody, _ = storedBody(attempt.requestBody)
	attempt.RequestBytes = attempt.requestBytes
	attempt.ResponseBody, _ = storedBody(maskResponseBody(attempt.responseBody))
	attempt.ResponseBytes = attempt.responseBytes
	rec := attempt.historyRecord
	rec.BeforeHeaders = redactHeaders(rec.BeforeHeaders)
	rec.AfterHeaders = redactHeaders(rec.AfterHeaders)
	rec.ResponseHeaders = redactHeaders(rec.ResponseHeaders)
	if state.writer != nil {
		state.writer.Enqueue(rec)
	}
}

func resolveCodexCredential(authIndex, authID string) (credentialSnapshot, bool) {
	state.mu.Lock()
	cred, ok := state.credentials[authIndex]
	state.mu.Unlock()
	if !ok {
		entry, err := hostAuthGetRuntimeFunc(authIndex)
		if err != nil {
			return credentialSnapshot{AuthIndex: authIndex, AuthID: authID}, false
		}
		cred = snapshotFromEntry(entry.Auth)
		if cred.AuthID == "" {
			cred.AuthID = authID
		}
	}
	document, err := hostAuthGetFunc(authIndex)
	if err == nil {
		cred.PlanType = credentialPlanType(document)
		cred.PlanResolved = true
	} else {
		cred.PlanType = ""
		cred.PlanResolved = false
	}
	state.mu.Lock()
	state.credentials[authIndex] = cred
	state.mu.Unlock()
	return cred, isCodexCredential(cred.Provider, cred.Type)
}
func isCodexCredential(provider, typ string) bool {
	provider = strings.TrimSpace(provider)
	typ = strings.TrimSpace(typ)
	if provider != "" {
		return strings.EqualFold(provider, "codex")
	}
	return strings.EqualFold(typ, "codex")
}
func snapshotFromEntry(e hostAuthFileEntry) credentialSnapshot {
	return credentialSnapshot{AuthIndex: e.AuthIndex, AuthID: e.ID, Name: e.Name, Label: e.Label, Provider: e.Provider, Type: e.Type, Status: e.Status, Disabled: e.Disabled, Unavailable: e.Unavailable}
}
