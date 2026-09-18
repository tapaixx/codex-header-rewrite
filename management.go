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
	apiOrphansCleanupPath  = "/codex-header-rewrite/orphans/cleanup"
	apiTestPath            = "/codex-header-rewrite/test"
	apiTurnStateDecodePath = "/codex-header-rewrite/turn-state/decode"
	apiTurnStatesPath      = "/codex-header-rewrite/turn-states"
)

type credentialView struct {
	credentialSnapshot
	Configurable bool `json:"configurable"`
	HasRule      bool `json:"has_rule"`
	HistoryCount int  `json:"history_count"`
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
		{Method: http.MethodPost, Path: apiOrphansCleanupPath, Description: "Remove orphaned credential data"},
		{Method: http.MethodPost, Path: apiTestPath, Description: "Run a header rewrite test request"},
		{Method: http.MethodPost, Path: apiTurnStateDecodePath, Description: "Decode an X-Codex-Turn-State envelope"},
		{Method: http.MethodGet, Path: apiTurnStatesPath, Description: "List the newest turn state per credential and model"},
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
	case req.Method == http.MethodGet && strings.HasSuffix(req.Path, apiHistoryPath):
		authIndex := strings.TrimSpace(req.Query.Get("auth_index"))
		if authIndex == "" {
			return jsonError(http.StatusBadRequest, "auth_index is required"), nil
		}
		page, _ := strconv.Atoi(req.Query.Get("page"))
		if page < 1 {
			page = 1
		}
		state.mu.Lock()
		store := state.store
		state.mu.Unlock()
		if store == nil {
			return jsonError(http.StatusServiceUnavailable, "persistence is not initialized"), nil
		}
		result, err := store.History(authIndex, page)
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
	case req.Method == http.MethodGet && strings.HasSuffix(req.Path, apiTurnStatesPath):
		// One row per credential and model: a newer pooled state for the same pair
		// replaces the older one, so this is exactly the set a client could
		// still be echoing.
		state.mu.Lock()
		recent := recentTurnStatesLocked()
		state.mu.Unlock()
		items := make([]map[string]any, 0, len(recent))
		for _, origin := range recent {
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
				"expired":     time.Since(origin.mintedAt) > turnStateReuseWindow,
			})
		}
		return jsonResponse(http.StatusOK, map[string]any{
			"turn_states":          items,
			"reuse_window_seconds": int64(turnStateReuseWindow.Seconds()),
		}), nil
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
		if snap.AuthIndex != "" {
			updates[snap.AuthIndex] = snap
		}
		count := 0
		if store != nil && snap.AuthIndex != "" {
			count, _ = store.HistoryCount(snap.AuthIndex)
		}
		_, hasRule := rules[snap.AuthIndex]
		items = append(items, credentialView{credentialSnapshot: snap, Configurable: snap.AuthIndex != "", HasRule: hasRule, HistoryCount: count})
	}
	state.mu.Lock()
	for k, v := range updates {
		if existing, ok := state.credentials[k]; ok && existing.PlanResolved && sameCredentialIdentity(existing, v) {
			v.PlanType = existing.PlanType
			v.PlanResolved = true
		}
		state.credentials[k] = v
	}
	state.mu.Unlock()
	sort.Slice(items, func(i, j int) bool {
		li := items[i].Label
		if li == "" {
			li = items[i].Name
		}
		lj := items[j].Label
		if lj == "" {
			lj = items[j].Name
		}
		return strings.ToLower(li) < strings.ToLower(lj)
	})
	return items, nil
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
