package main

import (
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

type pluginState struct {
	mu          sync.Mutex
	store       persistence
	writer      *queuedPersistence
	dataPath    string
	rules       map[string]headerRule
	pending     map[string]*pendingRequest
	credentials map[string]credentialSnapshot
}

var state = &pluginState{rules: map[string]headerRule{}, pending: map[string]*pendingRequest{}, credentials: map[string]credentialSnapshot{}}

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
	state.store = backend
	state.writer = newQueuedPersistence(backend)
	state.dataPath = cfg.DataPath
	state.rules = make(map[string]headerRule, len(rules))
	state.pending = map[string]*pendingRequest{}
	for _, r := range rules {
		state.rules[r.AuthIndex] = r
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
	defer state.mu.Unlock()
	if state.store == nil {
		return rule, fmt.Errorf("persistence is not initialized")
	}
	if err := state.store.SaveRule(normalized); err != nil {
		return rule, err
	}
	state.rules[normalized.AuthIndex] = normalized
	return normalized, nil
}
func deleteRule(authIndex string) error {
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.store == nil {
		return fmt.Errorf("persistence is not initialized")
	}
	if err := state.store.DeleteRule(authIndex); err != nil {
		return err
	}
	delete(state.rules, authIndex)
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
	pr.current = &pendingAttempt{historyRecord: historyRecord{ID: fmt.Sprintf("%s#%d", req.RequestID, pr.attempts), RequestID: req.RequestID, Attempt: pr.attempts, AuthIndex: authIndex, AuthID: authID, CredentialName: cred.Name, CredentialLabel: cred.Label, Model: req.Model, RequestedModel: req.RequestedModel, SourceFormat: req.SourceFormat, Stream: req.Stream, StartedAt: time.Now().UTC(), BeforeHeaders: redactHeaders(before), AfterHeaders: redactHeaders(after), Outcome: "in_flight", Origin: originLive}}
	state.mu.Unlock()
	return requestInterceptResponse{Headers: updates, ClearHeaders: clears}, nil
}

func observeResponse(req responseInterceptRequest) {
	state.mu.Lock()
	defer state.mu.Unlock()
	pr := state.pending[req.RequestID]
	if pr == nil || pr.current == nil {
		return
	}
	pr.current.ResponseHeaders = redactHeaders(req.ResponseHeaders)
	pr.current.StatusCode = req.StatusCode
	// The body is read here only to learn which model the upstream served. The
	// model name is kept; the body itself is not stored anywhere.
	pr.current.models.observeBody(req.Body)
}
func observeStreamHeaders(req streamChunkInterceptRequest) {
	state.mu.Lock()
	defer state.mu.Unlock()
	pr := state.pending[req.RequestID]
	if pr == nil || pr.current == nil {
		return
	}
	if req.ChunkIndex == streamChunkHeaderInitIndex {
		pr.current.ResponseHeaders = redactHeaders(req.ResponseHeaders)
		return
	}
	pr.current.models.observeStream(req.Body)
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
	if cred, ok := state.credentials[authIndex]; ok {
		state.mu.Unlock()
		return cred, isCodexCredential(cred.Provider, cred.Type)
	}
	state.mu.Unlock()
	entry, err := hostAuthGetRuntimeFunc(authIndex)
	if err != nil {
		return credentialSnapshot{AuthIndex: authIndex, AuthID: authID}, false
	}
	cred := snapshotFromEntry(entry.Auth)
	if cred.AuthID == "" {
		cred.AuthID = authID
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
