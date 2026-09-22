package main

import (
	"net/http"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	pluginID        = "codex-header-rewrite"
	pluginName      = "Codex Header Rewrite"
	defaultDataPath = "plugins/data/codex-header-rewrite.db"
	historyLimit    = 50
	// A request body observed on this deployment reached 3.17 MB. Fifty of
	// those per credential is not a history, it is a transcript archive, so a
	// body is kept up to this much and the rest is dropped with its original
	// size recorded.
	maxStoredBodyBytes = 256 << 10
	pageSize        = 20
)

type pluginConfig struct{ DataPath string }

// poolMaintained reports whether this credential's state pool is live. The
// field is unset on every rule saved before it existed, and unset means on.
// The proxy pools a request may actually use: nil while the list's switch is off.
func (r headerRule) retryProxyPool() []string {
	if !r.RetryProxyEnabled {
		return nil
	}
	return r.RetryProxies
}
func (r headerRule) probeProxyPool() []string {
	if !r.ProbeProxyEnabled {
		return nil
	}
	return r.ProbeProxies
}

func (r headerRule) poolMaintained() bool { return r.MaintainStatePool == nil || *r.MaintainStatePool }

type headerRule struct {
	AuthIndex string            `json:"auth_index"`
	Enabled   bool              `json:"enabled"`
	Set       map[string]string `json:"set"`
	Remove    []string          `json:"remove"`
	// InjectTurnState is the single switch for this header: on, the plugin
	// supplies X-Codex-Turn-State from the pool and drops an echo it can prove
	// is unusable; off, it leaves the header exactly as the client sent it.
	InjectTurnState bool `json:"inject_turn_state"`
	// LegacyGuard carried the same switch before it became an injection
	// control. It is read so rules saved earlier keep working, and never
	// written; validateRule folds it into InjectTurnState.
	LegacyGuard bool `json:"strip_foreign_turn_state,omitempty"`
	// MaintainStatePool is the pool's master switch for this credential and the
	// one thing that outranks InjectTurnState. Off, the pool is frozen: no state
	// is minted from any response, nothing is injected, and the probe and the
	// degraded retry -- which exist to fill it -- stand down. A pointer, so a
	// rule saved before the field existed reads as on, which is the default.
	MaintainStatePool *bool `json:"maintain_state_pool,omitempty"`
	// RejectDegradedResponse withholds a response whose minted
	// X-Codex-Turn-State classifies as degraded: the body is replaced with an
	// error and, on a stream, every later chunk is dropped. The status code
	// cannot be changed from a plugin, so the client sees the error inside a
	// 200 -- as an `error` event on a stream, as an error object otherwise.
	RejectDegradedResponse bool `json:"reject_degraded_response"`
	// Empty matches all models; otherwise match the exact after-auth model ID.
	RejectDegradedModels []string `json:"reject_degraded_models,omitempty"`
	// RetryOnDegraded asks for one minimal request under the same credential
	// and model after a degraded state is seen, to obtain a non-degraded one
	// for the pool. RetryAttempts caps how many are made for one response.
	RetryOnDegraded bool `json:"retry_on_degraded"`
	RetryAttempts   int  `json:"retry_attempts,omitempty"`
	// Empty means direct; each background retry randomly selects one SOCKS URL.
	RetryProxies []string `json:"retry_proxies,omitempty"`
	// RetryProxyEnabled is the list's switch: off, retries go direct and the
	// list is only kept. Off by default.
	RetryProxyEnabled bool `json:"retry_proxy_enabled,omitempty"`
	// StateTTLSeconds is how long a pooled state counts as fresh for this
	// credential. Zero means defaultStateTTLSeconds, which is what every rule
	// saved before the field existed carries.
	StateTTLSeconds int `json:"state_ttl_seconds,omitempty"`

	// ProbeEnabled keeps the pool warm on a timer instead of waiting for a
	// degraded response to react to. It is mutually exclusive with
	// RetryOnDegraded: both fetch states on the same credential, and running
	// them together would have two schedules competing for the same pool.
	ProbeEnabled bool `json:"probe_enabled,omitempty"`
	// The window the probe may run in, as minutes into a UTC day. Equal values
	// or both zero mean all day; start greater than end crosses midnight,
	// which a local-time window routinely becomes once converted.
	ProbeWindowStartMinute int `json:"probe_window_start_minute,omitempty"`
	ProbeWindowEndMinute   int `json:"probe_window_end_minute,omitempty"`
	// Which models to keep warm. Required while the probe is on: a probe with
	// no models has nothing to do.
	ProbeModels []string `json:"probe_models,omitempty"`
	// Empty means direct; each probe request selects one at random, the same
	// way a retry does.
	ProbeProxies []string `json:"probe_proxies,omitempty"`
	// ProbeProxyEnabled is the probe list's switch, same contract as the retry's.
	ProbeProxyEnabled bool `json:"probe_proxy_enabled,omitempty"`
	// ProbeCookieMode decides which cookie jar a probe request presents, which
	// only has one right answer once the egress is known -- see probe.go. With
	// no proxies configured this is forced to probeCookieCredential; with
	// proxies it must be chosen, because the wrong choice sends a cookie bound
	// to one IP out through another.
	ProbeCookieMode string `json:"probe_cookie_mode,omitempty"`
	// How long after the last live request the credential still counts as in
	// use. Past it the probe stops rather than keeping an idle account warm.
	ProbeCookieTTLSeconds int `json:"probe_cookie_ttl_seconds,omitempty"`
	// The gap between the end of one probe task and the start of the next.
	ProbeIntervalSeconds int `json:"probe_interval_seconds,omitempty"`
	// ProbeResolveExitIP asks for the exact egress address behind a proxy. It
	// cannot be answered under a rotating pool -- the resolver reaches a
	// different host and therefore a different exit -- so it is left blank
	// there rather than answered wrongly.
	ProbeResolveExitIP bool `json:"probe_resolve_exit_ip,omitempty"`

	UpdatedAt time.Time `json:"updated_at"`
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
	// The reasoning effort asked for and the one the response reports. Shown
	// beside each model and never compared: an effort is a setting, not a
	// claim about which model answered.
	RequestEffort  string `json:"request_effort,omitempty"`
	UpstreamEffort string `json:"upstream_effort,omitempty"`
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
	// TurnStateInjected reports that the plugin supplied the header from the
	// pool rather than passing through whatever the client sent.
	TurnStateInjected bool `json:"turn_state_injected,omitempty"`
	// TurnStateInvalidated reports that the injected state was past the reuse
	// window and the upstream still minted a degraded state, so the pooled
	// entry was dropped rather than injected again.
	TurnStateInvalidated bool `json:"turn_state_invalidated,omitempty"`
	// TurnStateRejected reports that the response was withheld from the client
	// because its minted state classified as degraded.
	TurnStateRejected bool `json:"turn_state_rejected,omitempty"`
	// RetryAttempts is how many retries one degraded response triggered; the
	// whole series is a single record, not one per attempt.
	RetryAttempts      int    `json:"retry_attempts,omitempty"`
	TurnStateSessionID string `json:"turn_state_session_id,omitempty"`
	// What a probe chose for itself. The egress is the proxy it went out
	// through (empty means direct); the region is the datacentre the response
	// came back through, read from Cf-Ray, which is free and accurate for this
	// request even when the exact address is not knowable.
	ProbeEgress     string `json:"probe_egress,omitempty"`
	ProbeExitRegion string `json:"probe_exit_region,omitempty"`
	ProbeExitIP     string `json:"probe_exit_ip,omitempty"`
	ProbeCookieMode string `json:"probe_cookie_mode,omitempty"`
	// ProbePrimed reports that this probe spent an extra request obtaining a
	// cookie for its connection, which is the other half of its quota cost.
	ProbePrimed bool `json:"probe_primed,omitempty"`
	// Bodies travel on the record only between the request path and the store,
	// which splits them into their own bucket. They are absent from a record
	// read back by History, because a page of fifty records would otherwise
	// carry megabytes the list never displays; the detail fetches them by id.
	RequestBody   string `json:"request_body,omitempty"`
	ResponseBody  string `json:"response_body,omitempty"`
	RequestBytes  int    `json:"request_bytes,omitempty"`
	ResponseBytes int    `json:"response_bytes,omitempty"`
}

// bodyRecord is what one attempt sent and received. Sizes are of the payload
// as it was, so a truncated body can still say what it was truncated from.
type bodyRecord struct {
	RequestBody   string `json:"request_body,omitempty"`
	ResponseBody  string `json:"response_body,omitempty"`
	RequestBytes  int    `json:"request_bytes,omitempty"`
	ResponseBytes int    `json:"response_bytes,omitempty"`
}

func (b bodyRecord) empty() bool {
	return b.RequestBody == "" && b.ResponseBody == "" && b.RequestBytes == 0 && b.ResponseBytes == 0
}

// takeBodies moves the bodies off a record so the record and its payloads can
// be stored apart.
func (r *historyRecord) takeBodies() bodyRecord {
	out := bodyRecord{
		RequestBody: r.RequestBody, ResponseBody: r.ResponseBody,
		RequestBytes: r.RequestBytes, ResponseBytes: r.ResponseBytes,
	}
	r.RequestBody, r.ResponseBody = "", ""
	r.RequestBytes, r.ResponseBytes = 0, 0
	return out
}

const (
	originLive  = "live"
	originTest  = "test"
	originRetry = "retry"
	originProbe = "probe"
)

type pendingAttempt struct {
	historyRecord
	persisted bool
	models    modelObserver
	// Which pooled state went out on this request, and whether it was already
	// past the reuse window when it did. Read again when the response arrives.
	injectedDigest  string
	injectedExpired bool
	// rejecting is set when the response headers classified as degraded and
	// the rule asks for such responses to be withheld; rejectedChunks counts
	// stream chunks seen since, so the first carries the error and the rest
	// are dropped.
	rejecting      bool
	rejectedChunks int
	// clientCookie is the Cookie header of the request whose response was
	// withheld, kept so the retry can present the same browser session the
	// intercepted request did. It lives on the in-flight attempt only: it is
	// not part of historyRecord, so it is never serialised or persisted, and
	// the copy that reaches the history goes through redactHeaders.
	clientCookie string
	// The request payload as it went upstream, and the response as it came
	// back -- for a stream, the chunks in order. Both are capped at
	// maxStoredBodyBytes; responseBytes counts what arrived, not what was kept.
	requestBody   []byte
	requestBytes  int
	responseBody  []byte
	responseBytes int
	// A streamed response calls the mint path on its header chunk and again
	// per chunk; the retry series must be started once.
	retryScheduled bool
	// Closed when that series ends. A withheld response waits on it, so the
	// pool is refilled before the client is told to try again.
	retryDone chan struct{}
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

// storedBody returns a payload capped for storage together with the size it
// had. A body is kept verbatim up to maxStoredBodyBytes and cut on a rune
// boundary so the text stays decodable; the recorded size is always the whole
// payload, so a truncated body can still say what it was cut from.
func storedBody(body []byte) (string, int) {
	if len(body) == 0 {
		return "", 0
	}
	if len(body) <= maxStoredBodyBytes {
		return string(body), len(body)
	}
	cut := body[:maxStoredBodyBytes]
	for len(cut) > 0 && !utf8.Valid(cut) {
		cut = cut[:len(cut)-1]
	}
	return string(cut), len(body)
}

// appendBody accumulates a streamed response under the same cap. total counts
// everything that arrived, including what was not kept.
func appendBody(buffer []byte, total int, chunk []byte) ([]byte, int) {
	total += len(chunk)
	if room := maxStreamBufferBytes - len(buffer); room > 0 {
		if len(chunk) > room {
			chunk = chunk[:room]
		}
		buffer = append(buffer, chunk...)
	}
	return buffer, total
}

// The three ways a probe request can present a cookie. Which one is right
// follows from what the egress is, not from preference:
//
//   - probeCookieCredential uses the jar the live traffic fills. Correct when
//     the probe goes out the same way live traffic does, which is the case
//     when no proxy is configured.
//   - probeCookieStaticProxy keeps a jar per proxy URL. Correct when that URL
//     always exits from the same address, because a Cloudflare bot token is
//     bound to the address that obtained it.
//   - probeCookieRotatingProxy keeps no jar at all. A rotating pool exits from
//     a different address per connection, so a stored cookie is always the
//     wrong address's; instead each probe primes a cookie and uses it over the
//     same connection, which a SOCKS5 tunnel guarantees is one exit.
const (
	probeCookieCredential    = "credential"
	probeCookieStaticProxy   = "static_proxy"
	probeCookieRotatingProxy = "rotating_proxy"
)

func validProbeCookieMode(mode string) bool {
	switch mode {
	case probeCookieCredential, probeCookieStaticProxy, probeCookieRotatingProxy:
		return true
	}
	return false
}

// credentialSession is one cookie jar: what a given egress last handed back,
// and when. A jar is only ever written from a response that came back through
// the egress it is keyed to, so a proxied probe can never overwrite the jar
// the live traffic fills.
type credentialSession struct {
	AuthIndex string    `json:"auth_index"`
	Egress    string    `json:"egress"`
	Cookie    string    `json:"cookie,omitempty"`
	RefreshAt time.Time `json:"refreshed_at"`
	// LastLiveAt is only set on the direct jar and only by real client
	// traffic. It answers "is this account still in use", which is a different
	// question from "is this cookie still fresh" and must not share a field
	// with it: a probe that refreshed its own clock would keep an abandoned
	// account warm forever.
	LastLiveAt time.Time `json:"last_live_at,omitempty"`
}

const (
	probeHistoryLimit       = 500
	defaultProbeIntervalSec = 30
	minProbeIntervalSec     = 1
	minProbeCookieTTLSec    = 1
	minutesPerDay           = 24 * 60
)
