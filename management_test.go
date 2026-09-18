//go:build localtest

package main

import (
	"encoding/json"
	"net/url"
	"path/filepath"
	"testing"
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
