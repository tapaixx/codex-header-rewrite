//go:build localtest

package main

import (
	"encoding/json"
	"net/http"
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

func TestHistoryHeaderTemplatesKeepOnlyReusableRequestHeaders(t *testing.T) {
	templates := historyHeaderTemplates([]historyRecord{{
		ID:        "request-1#1",
		Model:     "gpt-5.6-luna",
		StartedAt: time.Date(2026, 9, 18, 8, 30, 0, 0, time.UTC),
		BeforeHeaders: http.Header{
			"Accept":             {"text/event-stream"},
			"Authorization":      {"Bearer [REDACTED]"},
			"Chatgpt-Account-Id": {"account-private"},
			"Content-Length":     {"123"},
			"Cookie":             {"[REDACTED]"},
			"Host":               {"chatgpt.com"},
			"X-Api-Key":          {"[REDACTED]"},
			"X-Redacted-Later":   {"value [REDACTED]"},
			"X-Team":             {"alpha", "beta"},
		},
	}})

	if len(templates) != 1 {
		t.Fatalf("templates=%#v", templates)
	}
	got := templates[0]
	if got.ID != "request-1#1" || got.Model != "gpt-5.6-luna" || !got.StartedAt.Equal(time.Date(2026, 9, 18, 8, 30, 0, 0, time.UTC)) {
		t.Fatalf("metadata=%#v", got)
	}
	want := map[string]string{"Accept": "text/event-stream", "X-Team": "alpha, beta"}
	if len(got.Headers) != len(want) {
		t.Fatalf("headers=%#v", got.Headers)
	}
	for name, value := range want {
		if got.Headers[name] != value {
			t.Fatalf("header %s=%q, want %q (all=%#v)", name, got.Headers[name], value, got.Headers)
		}
	}
}

func TestHistoryRouteReturnsSafeTemplatesAlongsideRecords(t *testing.T) {
	shutdownPlugin()
	p, err := openPersistence(filepath.Join(t.TempDir(), "history-templates.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := p.AppendHistory(historyRecord{
		ID:        "request-2#1",
		AuthIndex: "idx-a",
		StartedAt: time.Date(2026, 9, 18, 9, 0, 0, 0, time.UTC),
		Outcome:   "succeeded",
		BeforeHeaders: http.Header{
			"Authorization": {"Bearer secret"},
			"X-Trace":       {"trace-1"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	state.mu.Lock()
	state.store = p
	state.writer = newQueuedPersistence(p)
	state.rules = map[string]headerRule{}
	state.pending = map[string]*pendingRequest{}
	state.credentials = map[string]credentialSnapshot{}
	state.mu.Unlock()
	t.Cleanup(shutdownPlugin)

	resp, err := handleManagementAPI(managementRequest{
		Method: "GET",
		Path:   "/v0/management" + apiHistoryPath,
		Query:  url.Values{"auth_index": {"idx-a"}},
	})
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("status=%d err=%v body=%s", resp.StatusCode, err, resp.Body)
	}
	var decoded struct {
		Items     []historyRecord         `json:"items"`
		Templates []historyHeaderTemplate `json:"templates"`
	}
	if err := json.Unmarshal(resp.Body, &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.Items) != 1 || len(decoded.Templates) != 1 {
		t.Fatalf("response=%s", resp.Body)
	}
	if got := decoded.Templates[0].Headers; len(got) != 1 || got["X-Trace"] != "trace-1" {
		t.Fatalf("template headers=%#v", got)
	}
}
