package main

import (
	"net/http"
	"sort"
	"strings"
	"time"
)

const (
	pluginID        = "codex-header-rewrite"
	pluginName      = "Codex Header Rewrite"
	defaultDataPath = "plugins/data/codex-header-rewrite.db"
	historyLimit    = 50
	pageSize        = 10
)

type pluginConfig struct{ DataPath string }

type headerRule struct {
	AuthIndex string            `json:"auth_index"`
	Enabled   bool              `json:"enabled"`
	Set       map[string]string `json:"set"`
	Remove    []string          `json:"remove"`
	UpdatedAt time.Time         `json:"updated_at"`
}

type credentialSnapshot struct {
	AuthIndex   string `json:"auth_index"`
	AuthID      string `json:"auth_id,omitempty"`
	Name        string `json:"name,omitempty"`
	Label       string `json:"label,omitempty"`
	Provider    string `json:"provider,omitempty"`
	Type        string `json:"type,omitempty"`
	Status      string `json:"status,omitempty"`
	Disabled    bool   `json:"disabled,omitempty"`
	Unavailable bool   `json:"unavailable,omitempty"`
}

type historyRecord struct {
	ID              string      `json:"id"`
	RequestID       string      `json:"request_id"`
	Attempt         int         `json:"attempt"`
	AuthIndex       string      `json:"auth_index"`
	AuthID          string      `json:"auth_id,omitempty"`
	CredentialName  string      `json:"credential_name,omitempty"`
	CredentialLabel string      `json:"credential_label,omitempty"`
	Model           string      `json:"model,omitempty"`
	RequestedModel  string      `json:"requested_model,omitempty"`
	SourceFormat    string      `json:"source_format,omitempty"`
	Stream          bool        `json:"stream"`
	StartedAt       time.Time   `json:"started_at"`
	CompletedAt     time.Time   `json:"completed_at"`
	BeforeHeaders   http.Header `json:"before_headers,omitempty"`
	AfterHeaders    http.Header `json:"after_headers,omitempty"`
	ResponseHeaders http.Header `json:"response_headers,omitempty"`
	StatusCode      int         `json:"status_code,omitempty"`
	Outcome         string      `json:"outcome"`
	Error           string      `json:"error,omitempty"`
}

type pendingAttempt struct {
	historyRecord
	persisted bool
}

type pendingRequest struct {
	attempts int
	current  *pendingAttempt
}

type historyPage struct {
	AuthIndex  string          `json:"auth_index"`
	Page       int             `json:"page"`
	PageSize   int             `json:"page_size"`
	Total      int             `json:"total"`
	TotalPages int             `json:"total_pages"`
	Items      []historyRecord `json:"items"`
}

func cloneHeader(h http.Header) http.Header {
	if h == nil { return nil }
	out := make(http.Header, len(h))
	for k, values := range h { out[k] = append([]string(nil), values...) }
	return out
}

func applyRuleToHeaders(base http.Header, rule headerRule) http.Header {
	out := cloneHeader(base)
	if out == nil { out = make(http.Header) }
	for _, key := range rule.Remove { deleteHeaderFold(out, key) }
	for key, value := range rule.Set {
		deleteHeaderFold(out, key)
		out.Set(http.CanonicalHeaderKey(key), value)
	}
	return out
}

func deleteHeaderFold(h http.Header, key string) {
	for existing := range h {
		if strings.EqualFold(existing, key) { delete(h, existing) }
	}
}

func normalizedRemove(in []string) []string {
	seen := map[string]string{}
	for _, raw := range in {
		key := http.CanonicalHeaderKey(strings.TrimSpace(raw))
		if key != "" { seen[strings.ToLower(key)] = key }
	}
	out := make([]string, 0, len(seen))
	for _, key := range seen { out = append(out, key) }
	sort.Strings(out)
	return out
}
