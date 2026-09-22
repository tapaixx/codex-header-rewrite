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

// Only a scheme survives redaction. A Cookie has no scheme: its first crumb is
// a name=value pair, and keeping everything before the first space published it.
func TestRedactionKeepsSchemesAndNothingElse(t *testing.T) {
	cases := []struct{ name, value, want string }{
		{"Authorization", "Bearer secret-token", "Bearer [REDACTED]"},
		{"Authorization", "Basic dXNlcjpwYXNz", "Basic [REDACTED]"},
		{"Cookie", "session=secret-value; oai-did=device", "[REDACTED]"},
		{"Cookie", "a=1", "[REDACTED]"},
		{"Set-Cookie", "session=secret-value; Path=/; HttpOnly", "[REDACTED]"},
		{"X-Auth-Token", "tok en", "[REDACTED]"},
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
