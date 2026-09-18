//go:build localtest

package main

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func resetState(t *testing.T) persistence {
	t.Helper()
	shutdownPlugin()
	p, err := openPersistence(filepath.Join(t.TempDir(), "state.json")); if err != nil { t.Fatal(err) }
	state.mu.Lock()
	state.store = p
	state.writer = newQueuedPersistence(p)
	state.rules = map[string]headerRule{}
	state.pending = map[string]*pendingRequest{}
	state.credentials = map[string]credentialSnapshot{}
	state.mu.Unlock()
	t.Cleanup(shutdownPlugin)
	return p
}

func TestRetryAttemptsBelongToEachCredential(t *testing.T) {
	p := resetState(t)
	state.mu.Lock()
	state.credentials["idx-a"] = credentialSnapshot{AuthIndex: "idx-a", AuthID: "a", Provider: "codex", Name: "a.json"}
	state.credentials["idx-b"] = credentialSnapshot{AuthIndex: "idx-b", AuthID: "b", Provider: "codex", Name: "b.json"}
	state.rules["idx-a"] = headerRule{AuthIndex: "idx-a", Enabled: true, Set: map[string]string{"X-Team": "A"}}
	state.rules["idx-b"] = headerRule{AuthIndex: "idx-b", Enabled: true, Set: map[string]string{"X-Team": "B"}}
	state.mu.Unlock()
	r1, err := interceptAfter(requestInterceptRequest{RequestID: "req", Model: "gpt", Headers: http.Header{"Authorization": {"Bearer real"}}, Metadata: map[string]any{"selected_auth_index": "idx-a", "selected_auth_id": "a"}})
	if err != nil || r1.Headers.Get("X-Team") != "A" { t.Fatalf("r1=%#v err=%v", r1, err) }
	r2, err := interceptAfter(requestInterceptRequest{RequestID: "req", Model: "gpt", Metadata: map[string]any{"selected_auth_index": "idx-b", "selected_auth_id": "b"}})
	if err != nil || r2.Headers.Get("X-Team") != "B" { t.Fatalf("r2=%#v err=%v", r2, err) }
	observeResponse(responseInterceptRequest{RequestID: "req", StatusCode: 200, ResponseHeaders: http.Header{"Set-Cookie": {"secret"}, "X-Upstream": {"ok"}}})
	completeRequest(requestCompletion{RequestID: "req", Outcome: "succeeded", StatusCode: 200, StartedAt: time.Now().Add(-time.Second), CompletedAt: time.Now()})
	state.mu.Lock(); writer := state.writer; state.writer = nil; state.store = nil; state.mu.Unlock()
	if err := writer.Close(); err != nil { t.Fatal(err) }
	pa, _ := p.History("idx-a", 1); pb, _ := p.History("idx-b", 1)
	if len(pa.Items) != 1 || pa.Items[0].Outcome != "switched" || pa.Items[0].StatusCode != 0 { t.Fatalf("A=%#v", pa.Items) }
	if len(pb.Items) != 1 || pb.Items[0].Outcome != "succeeded" || pb.Items[0].StatusCode != 200 { t.Fatalf("B=%#v", pb.Items) }
	if pa.Items[0].BeforeHeaders.Get("Authorization") != "Bearer [REDACTED]" { t.Fatalf("request secret not redacted %#v", pa.Items[0].BeforeHeaders) }
	if pb.Items[0].ResponseHeaders.Get("Set-Cookie") != "[REDACTED]" { t.Fatalf("response secret not redacted %#v", pb.Items[0].ResponseHeaders) }
}

func TestLiveStreamRecordsTheModelTheUpstreamServed(t *testing.T) {
	p := resetState(t)
	state.mu.Lock()
	state.credentials["idx-a"] = credentialSnapshot{AuthIndex: "idx-a", AuthID: "a", Provider: "codex", Name: "a.json"}
	state.mu.Unlock()
	if _, err := interceptAfter(requestInterceptRequest{RequestID: "req", Model: "gpt-5.6-luna", RequestedModel: "gpt-5", Stream: true, Metadata: map[string]any{"selected_auth_index": "idx-a"}}); err != nil {
		t.Fatal(err)
	}
	observeStreamHeaders(streamChunkInterceptRequest{RequestID: "req", ChunkIndex: streamChunkHeaderInitIndex, ResponseHeaders: http.Header{"X-Upstream": {"ok"}}})
	observeStreamHeaders(streamChunkInterceptRequest{RequestID: "req", ChunkIndex: 0, Body: []byte("event: response.created\ndata: {\"response\":{\"mod")})
	observeStreamHeaders(streamChunkInterceptRequest{RequestID: "req", ChunkIndex: 1, Body: []byte("el\":\"gpt-5.6-luna-mini\"}}\n\n")})
	completeRequest(requestCompletion{RequestID: "req", Outcome: "succeeded", StatusCode: 200, CompletedAt: time.Now()})
	state.mu.Lock(); writer := state.writer; state.writer = nil; state.store = nil; state.mu.Unlock()
	if err := writer.Close(); err != nil { t.Fatal(err) }
	page, _ := p.History("idx-a", 1)
	if len(page.Items) != 1 { t.Fatalf("history=%#v", page.Items) }
	rec := page.Items[0]
	if rec.UpstreamModel != "gpt-5.6-luna-mini" { t.Fatalf("upstream model=%q", rec.UpstreamModel) }
	if rec.ModelMismatch == nil || !*rec.ModelMismatch { t.Fatalf("mismatch=%v", rec.ModelMismatch) }
	if rec.Origin != originLive { t.Fatalf("origin=%q", rec.Origin) }
	raw, _ := json.Marshal(rec)
	if strings.Contains(string(raw), "response.created") { t.Fatalf("stream body reached persistence: %s", raw) }
}

func TestNonStreamingResponseBodyIsReadForTheModelOnly(t *testing.T) {
	p := resetState(t)
	state.mu.Lock()
	state.credentials["idx-a"] = credentialSnapshot{AuthIndex: "idx-a", Provider: "codex", Name: "a.json"}
	state.mu.Unlock()
	if _, err := interceptAfter(requestInterceptRequest{RequestID: "req2", Model: "gpt-5.6-luna", Metadata: map[string]any{"selected_auth_index": "idx-a"}}); err != nil {
		t.Fatal(err)
	}
	observeResponse(responseInterceptRequest{RequestID: "req2", StatusCode: 200, Body: []byte(`{"model":"gpt-5.6-luna","output":[{"text":"private answer"}]}`)})
	completeRequest(requestCompletion{RequestID: "req2", Outcome: "succeeded", StatusCode: 200, CompletedAt: time.Now()})
	state.mu.Lock(); writer := state.writer; state.writer = nil; state.store = nil; state.mu.Unlock()
	if err := writer.Close(); err != nil { t.Fatal(err) }
	page, _ := p.History("idx-a", 1)
	if len(page.Items) != 1 { t.Fatalf("history=%#v", page.Items) }
	rec := page.Items[0]
	if rec.UpstreamModel != "gpt-5.6-luna" || rec.ModelMismatch == nil || *rec.ModelMismatch {
		t.Fatalf("rec=%#v mismatch=%v", rec, rec.ModelMismatch)
	}
	raw, _ := json.Marshal(rec)
	if strings.Contains(string(raw), "private answer") { t.Fatalf("response body reached persistence: %s", raw) }
}

func TestNonCodexIgnored(t *testing.T) {
	resetState(t)
	state.mu.Lock(); state.credentials["idx-x"] = credentialSnapshot{AuthIndex: "idx-x", Provider: "xai", Type: "codex"}; state.mu.Unlock()
	resp, err := interceptAfter(requestInterceptRequest{RequestID: "x", Metadata: map[string]any{"selected_auth_index": "idx-x"}})
	if err != nil || len(resp.Headers) != 0 { t.Fatalf("resp=%#v err=%v", resp, err) }
	state.mu.Lock(); pr := state.pending["x"]; state.mu.Unlock()
	if pr == nil || pr.current != nil { t.Fatalf("non-codex current=%#v", pr) }
}
