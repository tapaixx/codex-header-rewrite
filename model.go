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
	// StripForeignTurnState removes an echoed X-Codex-Turn-State when this
	// plugin knows it came from a different credential. Detection alone only
	// reports the contradiction; this is what stops it reaching the upstream.
	StripForeignTurnState bool      `json:"strip_foreign_turn_state"`
	UpdatedAt             time.Time `json:"updated_at"`
}

type credentialSnapshot struct {
	AuthIndex    string `json:"auth_index"`
	AuthID       string `json:"auth_id,omitempty"`
	Name         string `json:"name,omitempty"`
	Label        string `json:"label,omitempty"`
	Provider     string `json:"provider,omitempty"`
	Type         string `json:"type,omitempty"`
	Status       string `json:"status,omitempty"`
	Disabled     bool   `json:"disabled,omitempty"`
	Unavailable  bool   `json:"unavailable,omitempty"`
	PlanType     string `json:"plan_type,omitempty"`
	PlanResolved bool   `json:"-"`
}

type historyRecord struct {
	ID              string      `json:"id"`
	RequestID       string      `json:"request_id"`
	Attempt         int         `json:"attempt"`
	AuthIndex       string      `json:"auth_index"`
	AuthID          string      `json:"auth_id,omitempty"`
	CredentialName  string      `json:"credential_name,omitempty"`
	CredentialLabel string      `json:"credential_label,omitempty"`
	CredentialPlan  string      `json:"credential_plan,omitempty"`
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
	// UpstreamModel is the model the upstream response declared. Empty means
	// the response never declared one, which is distinct from a match.
	UpstreamModel string `json:"upstream_model,omitempty"`
	ModelMismatch *bool  `json:"model_mismatch,omitempty"`
	ModelConflict bool   `json:"model_conflict,omitempty"`
	// Origin separates live proxy traffic from operator-issued test requests.
	Origin string `json:"origin,omitempty"`

	// TurnStateEcho is the blob this request echoed, TurnStateMinted the blob
	// the response returned (the JSON name is retained for compatibility).
	// TurnStateCrossAccount is set only when the echoed blob is known to have
	// come from a different credential; a blob
	// whose origin is no longer remembered stays unset rather than guessed.
	TurnStateEcho         *turnStateInfo `json:"turn_state_echo,omitempty"`
	TurnStateMinted       *turnStateInfo `json:"turn_state_minted,omitempty"`
	TurnStateOriginIndex  string         `json:"turn_state_origin_index,omitempty"`
	TurnStateOriginLabel  string         `json:"turn_state_origin_label,omitempty"`
	TurnStateOriginModel  string         `json:"turn_state_origin_model,omitempty"`
	TurnStateCrossAccount *bool          `json:"turn_state_cross_account,omitempty"`
	TurnStateCrossModel   *bool          `json:"turn_state_cross_model,omitempty"`
	TurnStateExpired      bool           `json:"turn_state_expired,omitempty"`
	TurnStateAgeSeconds   int64          `json:"turn_state_age_seconds,omitempty"`
	TurnStateStripped     bool           `json:"turn_state_stripped,omitempty"`
	TurnStateSessionID    string         `json:"turn_state_session_id,omitempty"`
}

const (
	originLive = "live"
	originTest = "test"
)

type pendingAttempt struct {
	historyRecord
	persisted bool
	models    modelObserver
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
	if h == nil {
		return nil
	}
	out := make(http.Header, len(h))
	for k, values := range h {
		out[k] = append([]string(nil), values...)
	}
	return out
}

func applyRuleToHeaders(base http.Header, rule headerRule) http.Header {
	out := cloneHeader(base)
	if out == nil {
		out = make(http.Header)
	}
	for _, key := range rule.Remove {
		deleteHeaderFold(out, key)
	}
	for key, value := range rule.Set {
		deleteHeaderFold(out, key)
		out.Set(http.CanonicalHeaderKey(key), value)
	}
	return out
}

func deleteHeaderFold(h http.Header, key string) {
	for existing := range h {
		if strings.EqualFold(existing, key) {
			delete(h, existing)
		}
	}
}

func normalizedRemove(in []string) []string {
	seen := map[string]string{}
	for _, raw := range in {
		key := http.CanonicalHeaderKey(strings.TrimSpace(raw))
		if key != "" {
			seen[strings.ToLower(key)] = key
		}
	}
	out := make([]string, 0, len(seen))
	for _, key := range seen {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}
