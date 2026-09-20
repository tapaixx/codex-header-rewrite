//go:build localtest

package main

import (
	"encoding/json"
	"net/url"
	"path/filepath"
	"strings"
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

func TestCredentialEmailExtraction(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{name: "account", raw: `{"account":"alex@example.com","access_token":"secret"}`, want: "alex@example.com"},
		{name: "email", raw: `{"email":"dev@example.com"}`, want: "dev@example.com"},
		{name: "nested", raw: `{"profile":{"email":"nested@example.com"}}`, want: "nested@example.com"},
		{name: "account id is not email", raw: `{"account_id":"5417aaaa-bbbb-cccc","access_token":"secret"}`, want: ""},
		{name: "non email account", raw: `{"account":"team-a"}`, want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := credentialEmail(json.RawMessage(tc.raw)); got != tc.want {
				t.Fatalf("credentialEmail()=%q, want %q", got, tc.want)
			}
		})
	}
}

func TestCredentialListExposesEmailOnly(t *testing.T) {
	resetState(t)
	oldList, oldGet := hostAuthListFunc, hostAuthGetFunc
	hostAuthListFunc = func() (hostAuthListResponse, error) {
		return hostAuthListResponse{Files: []hostAuthFileEntry{{ID: "a", AuthIndex: "idx-a", Name: "codex-a.json", Provider: "codex"}}}, nil
	}
	hostAuthGetFunc = func(string) (json.RawMessage, error) {
		return json.RawMessage(`{"account":"user@example.com","access_token":"must-not-leak"}`), nil
	}
	t.Cleanup(func() {
		hostAuthListFunc = oldList
		hostAuthGetFunc = oldGet
	})
	items, err := listCredentialViews()
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Email != "user@example.com" {
		t.Fatalf("credentials=%#v", items)
	}
	raw, err := json.Marshal(items[0])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "must-not-leak") {
		t.Fatalf("credential response leaked auth material: %s", raw)
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

func TestTurnStatePoolAPIFiltersByCredential(t *testing.T) {
	resetState(t)
	resetTurnStates(t)
	state.mu.Lock()
	noteTurnStateMintLocked(fernetToken(0x80, time.Now(), 1), "idx-a", "A", "gpt-5.6-luna", "team")
	noteTurnStateMintLocked(fernetToken(0x80, time.Now().Add(time.Second), 1), "idx-b", "B", "gpt-5.6-luna", "team")
	state.mu.Unlock()

	resp, _ := handleManagementAPI(managementRequest{Method: "GET", Path: "/v0/management" + apiTurnStatesPath, Query: url.Values{"auth_index": {"idx-a"}}})
	var payload struct {
		TurnStates []struct {
			AuthIndex string `json:"auth_index"`
		} `json:"turn_states"`
	}
	if err := json.Unmarshal(resp.Body, &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.TurnStates) != 1 || payload.TurnStates[0].AuthIndex != "idx-a" {
		t.Fatalf("filtered states=%#v", payload.TurnStates)
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
