package main

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

//go:embed web/index.html
var indexHTML []byte

const (
	resourceIndexPath      = "/index"
	apiCredentialsPath     = "/codex-header-rewrite/credentials"
	apiRulePath            = "/codex-header-rewrite/rule"
	apiHistoryPath         = "/codex-header-rewrite/history"
	apiHistoryClearPath    = "/codex-header-rewrite/history/clear"
	apiHistoryBodyPath     = "/codex-header-rewrite/history/body"
	apiProbeHistoryPath    = "/codex-header-rewrite/probes"
	apiProbeClearPath      = "/codex-header-rewrite/probes/clear"
	apiOrphansCleanupPath  = "/codex-header-rewrite/orphans/cleanup"
	apiTestPath            = "/codex-header-rewrite/test"
	apiTurnStateDecodePath = "/codex-header-rewrite/turn-state/decode"
	apiTurnStatesPath      = "/codex-header-rewrite/turn-states"
	apiTurnStatePoolPath   = "/codex-header-rewrite/turn-state/pool"
	apiQuotaPath           = "/codex-header-rewrite/quota"
	apiSessionPath         = "/codex-header-rewrite/session"
	apiCookiePoolPath      = "/codex-header-rewrite/cookie-pool"
)

type credentialView struct {
	credentialSnapshot
	Email        string `json:"email,omitempty"`
	Configurable bool   `json:"configurable"`
	HasRule      bool   `json:"has_rule"`
	HistoryCount int    `json:"history_count"`
}
type cleanupResponse struct {
	Deleted []string `json:"deleted"`
}

func registerManagement() managementRegistration {
	return managementRegistration{Routes: []managementRoute{
		{Method: http.MethodGet, Path: apiCredentialsPath, Description: "List Codex credentials"},
		{Method: http.MethodGet, Path: apiRulePath, Description: "Get credential header rule"},
		{Method: http.MethodPut, Path: apiRulePath, Description: "Save credential header rule"},
		{Method: http.MethodDelete, Path: apiRulePath, Description: "Delete credential header rule"},
		{Method: http.MethodGet, Path: apiHistoryPath, Description: "List credential header history"},
		{Method: http.MethodPost, Path: apiHistoryClearPath, Description: "Clear credential history"},
		{Method: http.MethodGet, Path: apiHistoryBodyPath, Description: "Read one record's request and response bodies"},
		{Method: http.MethodGet, Path: apiProbeHistoryPath, Description: "List automatic state probe history"},
		{Method: http.MethodPost, Path: apiProbeClearPath, Description: "Clear automatic state probe history"},
		{Method: http.MethodPost, Path: apiOrphansCleanupPath, Description: "Remove orphaned credential data"},
		{Method: http.MethodPost, Path: apiTestPath, Description: "Run a header rewrite test request"},
		{Method: http.MethodPost, Path: apiTurnStateDecodePath, Description: "Decode an X-Codex-Turn-State envelope"},
		{Method: http.MethodGet, Path: apiTurnStatesPath, Description: "List the newest turn state per credential and model"},
		{Method: http.MethodPost, Path: apiTurnStatePoolPath, Description: "Pool the turn state a recorded live response returned"},
		{Method: http.MethodGet, Path: apiQuotaPath, Description: "The credential's allowance as the upstream last reported it"},
		{Method: http.MethodGet, Path: apiSessionPath, Description: "The credential-level cookie jar the live traffic fills"},
		{Method: http.MethodGet, Path: apiCookiePoolPath, Description: "The non-degraded cookie pool, one cookie per backend"},
	}, Resources: []resourceRoute{{Path: resourceIndexPath, Menu: pluginName, Description: "Codex credential header rewrite and history"}}}
}

func handleManagement(raw []byte) ([]byte, error) {
	var req managementRequest
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, err
		}
	}
	var resp managementResponse
	var err error
	if strings.HasSuffix(req.Path, resourceIndexPath) && strings.Contains(req.Path, "/v0/resource/plugins/") {
		resp = managementResponse{StatusCode: http.StatusOK, Headers: http.Header{"Content-Type": {"text/html; charset=utf-8"}, "Cache-Control": {"no-store"}}, Body: indexHTML}
	} else {
		resp, err = handleManagementAPI(req)
	}
	if err != nil {
		resp = jsonError(http.StatusInternalServerError, err.Error())
	}
	return okEnvelope(resp)
}

func handleManagementAPI(req managementRequest) (managementResponse, error) {
	switch {
	case req.Method == http.MethodGet && strings.HasSuffix(req.Path, apiCredentialsPath):
		items, err := listCredentialViews()
		if err != nil {
			return jsonError(http.StatusBadGateway, err.Error()), nil
		}
		// plugin_version is the version of the library actually serving this
		// request. A native plugin keeps running until the host restarts, so an
		// operator who just installed an update needs to see which build answered.
		return jsonResponse(http.StatusOK, map[string]any{"credentials": items, "plugin_version": pluginVersion, "test_defaults": map[string]any{
			"model":    defaultTestModel,
			"endpoint": defaultTestURL,
			"prompt":   defaultTestPrompt,
		}}), nil
	case req.Method == http.MethodGet && strings.HasSuffix(req.Path, apiRulePath):
		authIndex := strings.TrimSpace(req.Query.Get("auth_index"))
		if authIndex == "" {
			return jsonError(http.StatusBadRequest, "auth_index is required"), nil
		}
		rule, ok := getRule(authIndex)
		if !ok {
			rule = headerRule{AuthIndex: authIndex, Enabled: false, Set: map[string]string{}, Remove: []string{}}
		}
		return jsonResponse(http.StatusOK, map[string]any{"rule": rule, "warnings": specialHeaderWarnings(rule)}), nil
	case req.Method == http.MethodPut && strings.HasSuffix(req.Path, apiRulePath):
		var rule headerRule
		if err := json.Unmarshal(req.Body, &rule); err != nil {
			return jsonError(http.StatusBadRequest, "invalid JSON body"), nil
		}
		runtime, err := hostAuthGetRuntimeFunc(strings.TrimSpace(rule.AuthIndex))
		if err != nil {
			return jsonError(http.StatusBadRequest, "credential not found"), nil
		}
		if !isCodexCredential(runtime.Auth.Provider, runtime.Auth.Type) {
			return jsonError(http.StatusBadRequest, "credential is not Codex type"), nil
		}
		if runtime.Auth.AuthIndex == "" {
			return jsonError(http.StatusBadRequest, "credential has no stable auth_index"), nil
		}
		saved, err := saveRule(rule)
		if err != nil {
			return jsonError(http.StatusBadRequest, err.Error()), nil
		}
		return jsonResponse(http.StatusOK, map[string]any{"rule": saved, "warnings": specialHeaderWarnings(saved)}), nil
	case req.Method == http.MethodDelete && strings.HasSuffix(req.Path, apiRulePath):
		authIndex := strings.TrimSpace(req.Query.Get("auth_index"))
		if authIndex == "" {
			return jsonError(http.StatusBadRequest, "auth_index is required"), nil
		}
		if err := deleteRule(authIndex); err != nil {
			return jsonError(http.StatusInternalServerError, err.Error()), nil
		}
		return jsonResponse(http.StatusOK, map[string]any{"deleted": authIndex}), nil
	case req.Method == http.MethodPost && strings.HasSuffix(req.Path, apiProbeClearPath):
		var body struct {
			AuthIndex string `json:"auth_index"`
		}
		if err := json.Unmarshal(req.Body, &body); err != nil {
			return jsonError(http.StatusBadRequest, "invalid JSON body"), nil
		}
		authIndex := strings.TrimSpace(body.AuthIndex)
		if authIndex == "" {
			return jsonError(http.StatusBadRequest, "auth_index is required"), nil
		}
		state.mu.Lock()
		store := state.store
		state.mu.Unlock()
		if store == nil {
			return jsonError(http.StatusServiceUnavailable, "persistence is not initialized"), nil
		}
		if err := store.ClearProbeHistory(authIndex); err != nil {
			return jsonError(http.StatusInternalServerError, err.Error()), nil
		}
		return jsonResponse(http.StatusOK, map[string]any{"cleared": authIndex}), nil
	case req.Method == http.MethodGet && strings.HasSuffix(req.Path, apiProbeHistoryPath):
		authIndex := strings.TrimSpace(req.Query.Get("auth_index"))
		if authIndex == "" {
			return jsonError(http.StatusBadRequest, "auth_index is required"), nil
		}
		page, _ := strconv.Atoi(req.Query.Get("page"))
		size := historyPageSize(req.Query.Get("page_size"))
		state.mu.Lock()
		store := state.store
		rule := state.rules[authIndex]
		state.mu.Unlock()
		if store == nil {
			return jsonError(http.StatusServiceUnavailable, "persistence is not initialized"), nil
		}
		result, err := store.ProbeHistory(authIndex, page, size)
		if err != nil {
			return jsonError(http.StatusInternalServerError, err.Error()), nil
		}
		// The schedule is reported beside the rows, because a probe list that
		// is empty can mean "nothing to do" or "not due yet" or "the account
		// went quiet", and those read identically without it.
		session, _, _ := store.Session(authIndex, "")
		probeSchedule.Lock()
		due := probeSchedule.nextDue[authIndex]
		probeSchedule.Unlock()
		payload := map[string]any{
			"auth_index": result.AuthIndex, "page": result.Page, "page_size": result.PageSize,
			"total": result.Total, "total_pages": result.TotalPages, "items": result.Items,
			"limit": probeHistoryLimit, "enabled": rule.ProbeEnabled,
			"within_window": withinProbeWindow(rule, time.Now().UTC()),
			"pool_paused":   !rule.poolMaintained(),
		}
		if !session.LastLiveAt.IsZero() {
			payload["last_live_at"] = session.LastLiveAt
			payload["live_age_seconds"] = int64(time.Since(session.LastLiveAt).Seconds())
		}
		if !due.IsZero() && due.Before(time.Now().Add(probeRunning/2)) {
			payload["next_due_at"] = due
		} else if !due.IsZero() {
			payload["running"] = true
		}
		return jsonResponse(http.StatusOK, payload), nil
	case req.Method == http.MethodGet && strings.HasSuffix(req.Path, apiHistoryBodyPath):
		authIndex := strings.TrimSpace(req.Query.Get("auth_index"))
		id := strings.TrimSpace(req.Query.Get("id"))
		if authIndex == "" || id == "" {
			return jsonError(http.StatusBadRequest, "auth_index and id are required"), nil
		}
		state.mu.Lock()
		store := state.store
		state.mu.Unlock()
		if store == nil {
			return jsonError(http.StatusServiceUnavailable, "persistence is not initialized"), nil
		}
		body, found, err := store.HistoryBody(authIndex, id)
		if err != nil {
			return jsonError(http.StatusInternalServerError, err.Error()), nil
		}
		// A record older than this feature, or one that carried no payload, is
		// not an error: the panel says so rather than showing a failure.
		return jsonResponse(http.StatusOK, map[string]any{
			"id": id, "found": found, "masked_request_fields": requestContentFields,
			"masked_response_fields": responseContentFields, "max_stored_bytes": maxStoredBodyBytes,
			"request_body": body.RequestBody, "request_bytes": body.RequestBytes,
			"response_body": body.ResponseBody, "response_bytes": body.ResponseBytes,
		}), nil
	case req.Method == http.MethodGet && strings.HasSuffix(req.Path, apiHistoryPath):
		authIndex := strings.TrimSpace(req.Query.Get("auth_index"))
		if authIndex == "" {
			return jsonError(http.StatusBadRequest, "auth_index is required"), nil
		}
		page, _ := strconv.Atoi(req.Query.Get("page"))
		size := historyPageSize(req.Query.Get("page_size"))
		if page < 1 {
			page = 1
		}
		state.mu.Lock()
		store := state.store
		state.mu.Unlock()
		if store == nil {
			return jsonError(http.StatusServiceUnavailable, "persistence is not initialized"), nil
		}
		result, err := store.History(authIndex, page, size)
		if err != nil {
			return jsonError(http.StatusInternalServerError, err.Error()), nil
		}
		return jsonResponse(http.StatusOK, result), nil
	case req.Method == http.MethodPost && strings.HasSuffix(req.Path, apiHistoryClearPath):
		var body struct {
			AuthIndex string `json:"auth_index"`
		}
		if err := json.Unmarshal(req.Body, &body); err != nil || strings.TrimSpace(body.AuthIndex) == "" {
			return jsonError(http.StatusBadRequest, "auth_index is required"), nil
		}
		state.mu.Lock()
		store := state.store
		state.mu.Unlock()
		if store == nil {
			return jsonError(http.StatusServiceUnavailable, "persistence is not initialized"), nil
		}
		if err := store.ClearHistory(strings.TrimSpace(body.AuthIndex)); err != nil {
			return jsonError(http.StatusInternalServerError, err.Error()), nil
		}
		return jsonResponse(http.StatusOK, map[string]any{"cleared": strings.TrimSpace(body.AuthIndex)}), nil
	case req.Method == http.MethodGet && strings.HasSuffix(req.Path, apiCookiePoolPath):
		authIndex := strings.TrimSpace(req.Query.Get("auth_index"))
		if authIndex == "" {
			return jsonError(http.StatusBadRequest, "auth_index is required"), nil
		}
		entries, err := cookiePoolFor(authIndex)
		if err != nil {
			return jsonError(http.StatusInternalServerError, err.Error()), nil
		}
		now := time.Now()
		items := make([]map[string]any, 0, len(entries))
		for _, entry := range entries {
			items = append(items, map[string]any{
				"host": entry.Host, "cookie": entry.Cookie, "issued_at": entry.IssuedAt, "expires_at": entry.ExpiresAt,
				"source": entry.Source, "model": entry.Model, "digest": entry.Digest, "saved_at": entry.SavedAt,
				"usable": entry.usable(now),
			})
		}
		return jsonResponse(http.StatusOK, map[string]any{"auth_index": authIndex, "entries": items}), nil
	case req.Method == http.MethodGet && strings.HasSuffix(req.Path, apiSessionPath):
		// The credential-level jar: the cookie the live traffic presented,
		// updated by what its responses set. Cookies are recorded in plain text
		// by design, so the panel may show the value.
		authIndex := strings.TrimSpace(req.Query.Get("auth_index"))
		if authIndex == "" {
			return jsonError(http.StatusBadRequest, "auth_index is required"), nil
		}
		state.mu.Lock()
		store := state.store
		state.mu.Unlock()
		if store == nil {
			return jsonError(http.StatusServiceUnavailable, "persistence is not initialized"), nil
		}
		session, found, err := store.Session(authIndex, "")
		if err != nil {
			return jsonError(http.StatusInternalServerError, err.Error()), nil
		}
		payload := map[string]any{"auth_index": authIndex, "found": found && session.Cookie != "", "cookie": session.Cookie}
		if !session.RefreshAt.IsZero() {
			payload["refreshed_at"] = session.RefreshAt
		}
		if !session.LastLiveAt.IsZero() {
			payload["last_live_at"] = session.LastLiveAt
		}
		return jsonResponse(http.StatusOK, payload), nil
	case req.Method == http.MethodGet && strings.HasSuffix(req.Path, apiQuotaPath):
		authIndex := strings.TrimSpace(req.Query.Get("auth_index"))
		if authIndex == "" {
			return jsonError(http.StatusBadRequest, "auth_index is required"), nil
		}
		// refresh=1 asks the upstream's usage endpoint -- a read that spends no
		// allowance. A failed refresh still answers with the stored reading,
		// and says why it could not be renewed.
		if req.Query.Get("refresh") == "1" {
			quota, errRefresh := refreshQuota(authIndex)
			if errRefresh == nil {
				return jsonResponse(http.StatusOK, map[string]any{"auth_index": authIndex, "quota": quota, "refreshed": true}), nil
			}
			stored, found := credentialQuotaFor(authIndex)
			payload := map[string]any{"auth_index": authIndex, "quota": nil, "refresh_error": errRefresh.Error()}
			if found {
				payload["quota"] = stored
			}
			return jsonResponse(http.StatusOK, payload), nil
		}
		quota, found := credentialQuotaFor(authIndex)
		if !found {
			return jsonResponse(http.StatusOK, map[string]any{"auth_index": authIndex, "quota": nil}), nil
		}
		return jsonResponse(http.StatusOK, map[string]any{"auth_index": authIndex, "quota": quota}), nil
	case req.Method == http.MethodGet && strings.HasSuffix(req.Path, apiTurnStatesPath):
		// One row per credential and model: a newer pooled state for the same pair
		// replaces the older one, so this is exactly the set a client could
		// still be echoing.
		authIndex := strings.TrimSpace(req.Query.Get("auth_index"))
		// The freshness window belongs to the credential now, so it is read
		// under the same lock as the pool rather than being a constant.
		state.mu.Lock()
		recent := recentTurnStatesLocked()
		windows := make(map[string]time.Duration, len(recent)+1)
		for _, origin := range recent {
			if _, ok := windows[origin.authIndex]; !ok {
				windows[origin.authIndex] = turnStateReuseWindowLocked(origin.authIndex)
			}
		}
		window := turnStateReuseWindowLocked(authIndex)
		state.mu.Unlock()
		items := make([]map[string]any, 0, len(recent))
		for _, origin := range recent {
			if authIndex != "" && origin.authIndex != authIndex {
				continue
			}
			age := int64(time.Since(origin.mintedAt).Seconds())
			items = append(items, map[string]any{
				"state":       origin.blob,
				"digest":      origin.digest,
				"auth_index":  origin.authIndex,
				"label":       origin.label,
				"model":       origin.model,
				"plan_type":   origin.planType,
				"chars":       origin.chars,
				"max_chars":   origin.maxChars,
				"minted_at":   origin.mintedAt,
				"age_seconds": age,
				"cookie":      origin.cookie,
				"expired":     time.Since(origin.mintedAt) > windows[origin.authIndex],
				// Per item as well, so a view that is not filtered to a single
				// credential can still say what each row was judged against.
				"reuse_window_seconds": int64(windows[origin.authIndex].Seconds()),
			})
		}
		return jsonResponse(http.StatusOK, map[string]any{
			"turn_states":          items,
			"reuse_window_seconds": int64(window.Seconds()),
		}), nil
	case req.Method == http.MethodPost && strings.HasSuffix(req.Path, apiTurnStatePoolPath):
		var body struct {
			AuthIndex string `json:"auth_index"`
			ID        string `json:"id"`
		}
		if err := json.Unmarshal(req.Body, &body); err != nil {
			return jsonError(http.StatusBadRequest, "invalid JSON body"), nil
		}
		result, err := poolFromHistory(body.AuthIndex, body.ID)
		if err != nil {
			var failure *manualPoolError
			if errors.As(err, &failure) {
				return jsonError(failure.status, failure.message), nil
			}
			return jsonError(http.StatusInternalServerError, err.Error()), nil
		}
		return jsonResponse(http.StatusOK, result), nil
	case req.Method == http.MethodPost && strings.HasSuffix(req.Path, apiTurnStateDecodePath):
		var body struct {
			Token string `json:"token"`
		}
		if err := json.Unmarshal(req.Body, &body); err != nil {
			return jsonError(http.StatusBadRequest, "invalid JSON body"), nil
		}
		token := strings.TrimSpace(body.Token)
		if token == "" {
			return jsonError(http.StatusBadRequest, "token is required"), nil
		}
		info := decodeTurnState(token)
		payload := map[string]any{"info": info, "chars": len(token)}
		// The provenance table is the only thing that can say which credential
		// supplied a pasted blob, so the answer is included when it is known.
		state.mu.Lock()
		origin, known := lookupTurnStateOriginLocked(token)
		state.mu.Unlock()
		if known {
			payload["origin_auth_index"] = origin.authIndex
			payload["origin_label"] = origin.label
		}
		if !info.IssuedAt.IsZero() {
			payload["age_seconds"] = int64(time.Since(info.IssuedAt).Seconds())
		}
		return jsonResponse(http.StatusOK, payload), nil
	case req.Method == http.MethodPost && strings.HasSuffix(req.Path, apiTestPath):
		var body testRequest
		if err := json.Unmarshal(req.Body, &body); err != nil {
			return jsonError(http.StatusBadRequest, "invalid JSON body"), nil
		}
		result, err := runTestRequest(body)
		if err != nil {
			var testErr *testRequestError
			if errors.As(err, &testErr) {
				return jsonError(testErr.status, testErr.message), nil
			}
			return jsonError(http.StatusInternalServerError, err.Error()), nil
		}
		return jsonResponse(http.StatusOK, map[string]any{"result": result}), nil
	case req.Method == http.MethodPost && strings.HasSuffix(req.Path, apiOrphansCleanupPath):
		deleted, err := cleanupOrphans()
		if err != nil {
			return jsonError(http.StatusInternalServerError, err.Error()), nil
		}
		return jsonResponse(http.StatusOK, cleanupResponse{Deleted: deleted}), nil
	default:
		return jsonError(http.StatusNotFound, "route not found"), nil
	}
}

func listCredentialViews() ([]credentialView, error) {
	listed, err := hostAuthListFunc()
	if err != nil {
		return nil, err
	}
	items := make([]credentialView, 0)
	updates := map[string]credentialSnapshot{}
	state.mu.Lock()
	store := state.store
	rules := make(map[string]headerRule, len(state.rules))
	for k, v := range state.rules {
		rules[k] = v
	}
	state.mu.Unlock()
	for _, entry := range listed.Files {
		if !isCodexCredential(entry.Provider, entry.Type) {
			continue
		}
		snap := snapshotFromEntry(entry)
		count := 0
		email := ""
		if snap.AuthIndex != "" {
			if store != nil {
				count, _ = store.HistoryCount(snap.AuthIndex)
			}
			// The document is already being read here for the email, so the
			// plan claim is read in the same pass. Leaving it unresolved until
			// live traffic flowed made every idle credential report an
			// unknown plan in the panel.
			if document, readErr := hostAuthGetFunc(snap.AuthIndex); readErr == nil {
				email = credentialEmail(document)
				snap.PlanType = credentialPlanType(document)
				snap.PlanResolved = true
			}
			updates[snap.AuthIndex] = snap
		}
		_, hasRule := rules[snap.AuthIndex]
		items = append(items, credentialView{credentialSnapshot: snap, Email: email, Configurable: snap.AuthIndex != "", HasRule: hasRule, HistoryCount: count})
	}
	state.mu.Lock()
	for k, v := range updates {
		if existing, ok := state.credentials[k]; ok && !v.PlanResolved && existing.PlanResolved && sameCredentialIdentity(existing, v) {
			v.PlanType = existing.PlanType
			v.PlanResolved = true
		}
		state.credentials[k] = v
	}
	state.mu.Unlock()
	sort.Slice(items, func(i, j int) bool {
		li := items[i].Email
		if li == "" {
			li = items[i].Label
		}
		if li == "" {
			li = items[i].Name
		}
		lj := items[j].Email
		if lj == "" {
			lj = items[j].Label
		}
		if lj == "" {
			lj = items[j].Name
		}
		return strings.ToLower(li) < strings.ToLower(lj)
	})
	return items, nil
}

func credentialEmail(document json.RawMessage) string {
	var object map[string]json.RawMessage
	if len(document) == 0 || json.Unmarshal(document, &object) != nil {
		return ""
	}
	return credentialEmailObject(object, 0)
}

func credentialEmailObject(object map[string]json.RawMessage, depth int) string {
	if object == nil || depth > 2 {
		return ""
	}
	for _, key := range []string{"email", "account", "user_email", "userEmail", "username"} {
		raw := firstRawField(object, key)
		if len(raw) == 0 {
			continue
		}
		var value string
		if json.Unmarshal(raw, &value) == nil {
			value = strings.TrimSpace(value)
			if strings.Contains(value, "@") && !strings.Contains(value, " ") {
				return value
			}
		}
	}
	for _, key := range []string{"user", "profile", "identity", "id_token", "idToken", "data", "auth"} {
		raw := firstRawField(object, key)
		if len(raw) == 0 {
			continue
		}
		var nested map[string]json.RawMessage
		if json.Unmarshal(raw, &nested) == nil {
			if email := credentialEmailObject(nested, depth+1); email != "" {
				return email
			}
		}
	}
	return ""
}

func sameCredentialIdentity(existing, current credentialSnapshot) bool {
	if existing.AuthID != "" || current.AuthID != "" {
		return existing.AuthID == current.AuthID
	}
	return existing.Name == current.Name
}

func cleanupOrphans() ([]string, error) {
	listed, err := hostAuthListFunc()
	if err != nil {
		return nil, err
	}
	current := map[string]struct{}{}
	for _, entry := range listed.Files {
		if isCodexCredential(entry.Provider, entry.Type) && entry.AuthIndex != "" {
			current[entry.AuthIndex] = struct{}{}
		}
	}
	state.mu.Lock()
	store := state.store
	state.mu.Unlock()
	if store == nil {
		return nil, fmt.Errorf("persistence is not initialized")
	}
	keys, err := store.KnownAuthIndexes()
	if err != nil {
		return nil, err
	}
	deleted := []string{}
	for _, key := range keys {
		if _, ok := current[key]; ok {
			continue
		}
		if err := store.DeleteCredentialData(key); err != nil {
			return deleted, err
		}
		state.mu.Lock()
		delete(state.rules, key)
		delete(state.credentials, key)
		for latestKey, origin := range state.turnStateLatest {
			if origin.authIndex == key {
				delete(state.turnStateLatest, latestKey)
			}
		}
		for digest, origin := range state.turnStates {
			if origin.authIndex == key {
				delete(state.turnStates, digest)
			}
		}
		state.mu.Unlock()
		deleted = append(deleted, key)
	}
	return deleted, nil
}

func jsonResponse(status int, v any) managementResponse {
	raw, err := json.Marshal(v)
	if err != nil {
		return jsonError(http.StatusInternalServerError, err.Error())
	}
	return managementResponse{StatusCode: status, Headers: http.Header{"Content-Type": {"application/json; charset=utf-8"}, "Cache-Control": {"no-store"}}, Body: raw}
}
func jsonError(status int, message string) managementResponse {
	return jsonResponseUnsafe(status, map[string]any{"error": message})
}
func jsonResponseUnsafe(status int, v any) managementResponse {
	raw, _ := json.Marshal(v)
	return managementResponse{StatusCode: status, Headers: http.Header{"Content-Type": {"application/json; charset=utf-8"}, "Cache-Control": {"no-store"}}, Body: raw}
}
