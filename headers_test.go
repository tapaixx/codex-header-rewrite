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

func TestCodexCredentialProviderPrecedence(t *testing.T) {
	if !isCodexCredential("codex", "anything") { t.Fatal("codex provider should match") }
	if !isCodexCredential("", "CoDeX") { t.Fatal("type fallback") }
	if isCodexCredential("xai", "codex") { t.Fatal("provider must take precedence") }
}
