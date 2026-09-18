//go:build localtest

package main

import (
	"encoding/json"
	"net/url"
	"path/filepath"
	"testing"
	"time"
)

func TestManagementCredentialFilterAndRule(t *testing.T) {
	shutdownPlugin()
	p, _ := openPersistence(filepath.Join(t.TempDir(), "mgmt.json"))
	state.mu.Lock()
	state.store = p
	state.writer = newQueuedPersistence(p)
	state.rules = map[string]headerRule{}
	state.pending = map[string]*pendingRequest{}
	state.credentials = map[string]credentialSnapshot{}
	state.mu.Unlock()
	oldList, oldRuntime := hostAuthListFunc, hostAuthGetRuntimeFunc
	hostAuthListFunc = func() (hostAuthListResponse, error) {
		return hostAuthListResponse{Files: []hostAuthFileEntry{{ID: "a", AuthIndex: "idx-a", Name: "a.json", Provider: "codex"}, {ID: "c", AuthIndex: "idx-c", Name: "c.json", Provider: "claude"}}}, nil
	}
	hostAuthGetRuntimeFunc = func(idx string) (hostAuthGetRuntimeResponse, error) {
		return hostAuthGetRuntimeResponse{Auth: hostAuthFileEntry{ID: "a", AuthIndex: idx, Name: "a.json", Provider: "codex"}}, nil
	}
	t.Cleanup(func() { hostAuthListFunc = oldList; hostAuthGetRuntimeFunc = oldRuntime; shutdownPlugin() })
	resp, _ := handleManagementAPI(managementRequest{Method: "GET", Path: "/v0/management" + apiCredentialsPath})
	var list struct {
		Credentials   []credentialView `json:"credentials"`
		PluginVersion string           `json:"plugin_version"`
	}
	if err := json.Unmarshal(resp.Body, &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Credentials) != 1 || list.Credentials[0].AuthIndex != "idx-a" {
		t.Fatalf("creds=%#v", list.Credentials)
	}
	if list.PluginVersion != pluginVersion {
		t.Fatalf("plugin_version=%q, want %q", list.PluginVersion, pluginVersion)
	}
	body, _ := json.Marshal(headerRule{AuthIndex: "idx-a", Enabled: true, Set: map[string]string{"X-Test": "v"}})
	resp, _ = handleManagementAPI(managementRequest{Method: "PUT", Path: "/v0/management" + apiRulePath, Body: body})
	if resp.StatusCode != 200 {
		t.Fatalf("put=%d %s", resp.StatusCode, resp.Body)
	}
	resp, _ = handleManagementAPI(managementRequest{Method: "GET", Path: "/v0/management" + apiRulePath, Query: url.Values{"auth_index": {"idx-a"}}})
	var got struct {
		Rule headerRule `json:"rule"`
	}
	_ = json.Unmarshal(resp.Body, &got)
	if got.Rule.Set["X-Test"] != "v" {
		t.Fatalf("rule=%#v", got.Rule)
	}
}

func TestTurnStatePoolAPIExposesCredentialAndValue(t *testing.T) {
	resetState(t)
	resetTurnStates(t)
	blob := fernetToken(0x80, time.Now(), 1)
	state.mu.Lock()
	pooled := noteTurnStateMintLocked(blob, "idx-team-a", "Team A", "gpt-5.6-luna", "team")
	state.mu.Unlock()
	if !pooled {
		t.Fatal("fixture state did not enter the pool")
	}

	resp, _ := handleManagementAPI(managementRequest{Method: "GET", Path: "/v0/management" + apiTurnStatesPath})
	var payload struct {
		TurnStates []struct {
			AuthIndex string `json:"auth_index"`
			Label     string `json:"label"`
			State     string `json:"state"`
		} `json:"turn_states"`
	}
	if err := json.Unmarshal(resp.Body, &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.TurnStates) != 1 {
		t.Fatalf("turn states=%#v", payload.TurnStates)
	}
	got := payload.TurnStates[0]
	if got.AuthIndex != "idx-team-a" || got.Label != "Team A" || got.State != blob {
		t.Fatalf("pool row=%#v", got)
	}
}

func TestCredentialRefreshInvalidatesPlanWhenIdentityChanges(t *testing.T) {
	resetState(t)
	state.mu.Lock()
	state.credentials["idx-a"] = credentialSnapshot{AuthIndex: "idx-a", AuthID: "old-id", Name: "old.json", Provider: "codex", PlanType: "team", PlanResolved: true}
	state.mu.Unlock()
	oldList := hostAuthListFunc
	hostAuthListFunc = func() (hostAuthListResponse, error) {
		return hostAuthListResponse{Files: []hostAuthFileEntry{{ID: "new-id", AuthIndex: "idx-a", Name: "new.json", Provider: "codex"}}}, nil
	}
	t.Cleanup(func() { hostAuthListFunc = oldList })
	if _, err := listCredentialViews(); err != nil {
		t.Fatal(err)
	}
	state.mu.Lock()
	got := state.credentials["idx-a"]
	state.mu.Unlock()
	if got.PlanResolved || got.PlanType != "" {
		t.Fatalf("changed credential kept stale plan: %#v", got)
	}
}

func TestOrphanCleanupEvictsDurableAndInMemoryTurnState(t *testing.T) {
	resetState(t)
	resetTurnStates(t)
	blob := fernetToken(0x80, time.Now(), 1)
	state.mu.Lock()
	if !noteTurnStateMintLocked(blob, "idx-gone", "Gone", "gpt-5.6-luna", "team") {
		state.mu.Unlock()
		t.Fatal("fixture state did not enter the pool")
	}
	state.mu.Unlock()
	oldList := hostAuthListFunc
	hostAuthListFunc = func() (hostAuthListResponse, error) { return hostAuthListResponse{}, nil }
	t.Cleanup(func() { hostAuthListFunc = oldList })
	deleted, err := cleanupOrphans()
	if err != nil || len(deleted) != 1 || deleted[0] != "idx-gone" {
		t.Fatalf("deleted=%v err=%v", deleted, err)
	}
	state.mu.Lock()
	recent := recentTurnStatesLocked()
	_, known := lookupTurnStateOriginLocked(blob)
	state.mu.Unlock()
	if len(recent) != 0 || known {
		t.Fatalf("orphan remained in memory: recent=%#v known=%v", recent, known)
	}
}
