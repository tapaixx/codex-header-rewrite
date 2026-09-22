package main

import (
	"net/http"
	"strings"
	"testing"
)

func TestValidateRuleAndApply(t *testing.T) {
	r, err := validateRule(headerRule{AuthIndex: " idx ", Set: map[string]string{"x-test": "v"}, Remove: []string{"x-old", "X-OLD"}})
	if err != nil { t.Fatal(err) }
	if r.AuthIndex != "idx" || r.Set["X-Test"] != "v" || len(r.Remove) != 1 || r.Remove[0] != "X-Old" { t.Fatalf("normalized=%#v", r) }
	base := http.Header{"X-Test": {"old"}, "X-Old": {"gone"}, "Keep": {"yes"}}
	got := applyRuleToHeaders(base, r)
	if got.Get("X-Test") != "v" || got.Get("X-Old") != "" || got.Get("Keep") != "yes" { t.Fatalf("headers=%#v", got) }
	if base.Get("X-Test") != "old" { t.Fatal("base mutated") }
}

func TestValidateRejectsCRLFAndConflict(t *testing.T) {
	if _, err := validateRule(headerRule{AuthIndex: "i", Set: map[string]string{"X-Test": "ok\r\nInjected: yes"}}); err == nil { t.Fatal("expected CRLF error") }
	_, err := validateRule(headerRule{AuthIndex: "i", Set: map[string]string{"X-Test": "v"}, Remove: []string{"x-test"}})
	if err == nil || !strings.Contains(err.Error(), "set and removed") { t.Fatalf("err=%v", err) }
}

func TestRedactHeaders(t *testing.T) {
	in := http.Header{"Authorization": {"Bearer secret-token"}, "X-Api-Key": {"abc"}, "X-Custom": {"visible"}}
	out := redactHeaders(in)
	if out.Get("Authorization") != "Bearer [REDACTED]" || out.Get("X-Api-Key") != "[REDACTED]" || out.Get("X-Custom") != "visible" { t.Fatalf("out=%#v", out) }
}

// Only an authorization scheme survives redaction; every other sensitive value
// goes whole. Cookies are not redacted at all -- the operator reads them to
// compare the session a retry presents against the live request's.
func TestRedactionKeepsSchemesAndNothingElse(t *testing.T) {
	cases := []struct{ name, value, want string }{
		{"Authorization", "Bearer secret-token", "Bearer [REDACTED]"},
		{"Authorization", "Basic dXNlcjpwYXNz", "Basic [REDACTED]"},
		{"Cookie", "session=secret-value; oai-did=device", "session=secret-value; oai-did=device"},
		{"Set-Cookie", "session=secret-value; Path=/; HttpOnly", "session=secret-value; Path=/; HttpOnly"},
		{"X-Auth-Token", "tok en", "[REDACTED]"},
		{"X-Api-Key", "abc", "[REDACTED]"},
	}
	for _, tc := range cases {
		if got := redactHeaderValue(tc.name, tc.value); got != tc.want {
			t.Fatalf("%s: got %q want %q", tc.name, got, tc.want)
		}
	}
}

func TestCodexCredentialProviderPrecedence(t *testing.T) {
	if !isCodexCredential("codex", "anything") { t.Fatal("codex provider should match") }
	if !isCodexCredential("", "CoDeX") { t.Fatal("type fallback") }
	if isCodexCredential("xai", "codex") { t.Fatal("provider must take precedence") }
}

// The window is typed in by an operator, so a value outside the range is
// refused rather than quietly rewritten: a saved number that is not the one
// they typed would misreport what the pool is doing.
func TestStateTTLRange(t *testing.T) {
	cases := []struct {
		seconds int
		ok      bool
	}{
		{0, true}, // the field's way of saying "use the default"
		{minStateTTLSeconds, true},
		{200, true},
		{maxStateTTLSeconds, true},
		{minStateTTLSeconds - 1, false},
		{-1, false},
		{maxStateTTLSeconds + 1, false},
	}
	for _, tc := range cases {
		out, err := validateRule(headerRule{AuthIndex: "i", StateTTLSeconds: tc.seconds})
		if tc.ok {
			if err != nil {
				t.Fatalf("%d should be accepted: %v", tc.seconds, err)
			}
			if out.StateTTLSeconds != tc.seconds {
				t.Fatalf("%d was rewritten to %d", tc.seconds, out.StateTTLSeconds)
			}
			continue
		}
		if err == nil {
			t.Fatalf("%d should be refused", tc.seconds)
		}
	}
}

// The probe and the retry both fetch states for one credential on their own
// schedule; together they would be two writers competing for one pool. And a
// probe with no models, no interval or no liveness window has nothing to act
// on, so those are required rather than defaulted into something arbitrary.
func TestProbeRuleValidation(t *testing.T) {
	base := func() headerRule {
		return headerRule{
			AuthIndex: "i", ProbeEnabled: true, ProbeModels: []string{"m"},
			ProbeCookieTTLSeconds: 1800, ProbeIntervalSeconds: 5,
		}
	}
	cases := []struct {
		name    string
		mutate  func(*headerRule)
		wantErr string
	}{
		{"a complete rule is accepted", func(*headerRule) {}, ""},
		{"not both switches", func(r *headerRule) { r.RetryOnDegraded = true }, "cannot both be on"},
		{"models are required", func(r *headerRule) { r.ProbeModels = nil }, "probe_models is required"},
		{"blank models do not count", func(r *headerRule) { r.ProbeModels = []string{" ", ""} }, "probe_models is required"},
		{"cookie ttl is required", func(r *headerRule) { r.ProbeCookieTTLSeconds = 0 }, "probe_cookie_ttl_seconds"},
		{"interval is required", func(r *headerRule) { r.ProbeIntervalSeconds = 0 }, "probe_interval_seconds"},
		{"interval has no upper bound", func(r *headerRule) { r.ProbeIntervalSeconds = 86400 }, ""},
		{"window minutes are bounded", func(r *headerRule) { r.ProbeWindowStartMinute = 1440 }, "probe_window_start_minute"},
		{"a wrapping window is allowed", func(r *headerRule) {
			r.ProbeWindowStartMinute, r.ProbeWindowEndMinute = 1320, 120
		}, ""},
		{"an unknown cookie mode is refused", func(r *headerRule) { r.ProbeCookieMode = "whatever" }, "probe_cookie_mode must be one of"},
		{"proxies force an explicit mode", func(r *headerRule) {
			r.ProbeProxies = []string{"socks5://127.0.0.1:1080"}
		}, "probe_cookie_mode must be chosen"},
		{"an explicit mode satisfies it", func(r *headerRule) {
			r.ProbeProxies = []string{"socks5://127.0.0.1:1080"}
			r.ProbeCookieMode = probeCookieRotatingProxy
		}, ""},
		{"a bad proxy is refused", func(r *headerRule) { r.ProbeProxies = []string{"http://x"} }, "probe_proxies[1]"},
		{"an off probe validates nothing", func(r *headerRule) {
			r.ProbeEnabled = false
			r.ProbeModels = nil
			r.ProbeIntervalSeconds = 0
			r.RetryOnDegraded = true
		}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rule := base()
			tc.mutate(&rule)
			out, err := validateRule(rule)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err=%v want it to mention %q (out=%#v)", err, tc.wantErr, out)
			}
		})
	}
}

// With no proxy the egress is the host's own, so there is exactly one coherent
// jar and asking would be noise.
func TestProbeCookieModeIsForcedWithoutProxies(t *testing.T) {
	rule := headerRule{
		AuthIndex: "i", ProbeEnabled: true, ProbeModels: []string{"m"},
		ProbeCookieTTLSeconds: 60, ProbeIntervalSeconds: 1,
		ProbeCookieMode: probeCookieRotatingProxy,
	}
	out, err := validateRule(rule)
	if err != nil {
		t.Fatal(err)
	}
	if out.ProbeCookieMode != probeCookieCredential {
		t.Fatalf("mode=%q", out.ProbeCookieMode)
	}
}
