package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// The body is recorded to explain the request around the conversation, not to
// keep the conversation. What is asked and what is answered are replaced; the
// fields that make a body worth reading back are not touched.
func TestTheConversationIsMaskedAndTheRestSurvives(t *testing.T) {
	request := `{"model":"gpt-6-astra","instructions":"You are Codex.","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"my private prompt"}]}],"tools":[],"reasoning":{"effort":"high"},"stream":true}`
	masked := string(maskRequestBody([]byte(request)))
	if strings.Contains(masked, "my private prompt") {
		t.Fatalf("the prompt survived masking: %s", masked)
	}
	for _, keep := range []string{"gpt-6-astra", "You are Codex.", "\"effort\":\"high\"", "\"stream\":true"} {
		if !strings.Contains(masked, keep) {
			t.Fatalf("masking dropped %q: %s", keep, masked)
		}
	}
	if !strings.Contains(masked, "[MASKED ") {
		t.Fatalf("the masked field should say it was masked: %s", masked)
	}

	response := `{"id":"resp_1","model":"gpt-6-astra","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"the private answer"}]}],"usage":{"input_tokens":12}}`
	maskedResponse := string(maskResponseBody([]byte(response)))
	if strings.Contains(maskedResponse, "the private answer") {
		t.Fatalf("the answer survived masking: %s", maskedResponse)
	}
	for _, keep := range []string{"resp_1", "gpt-6-astra", "input_tokens"} {
		if !strings.Contains(maskedResponse, keep) {
			t.Fatalf("masking dropped %q: %s", keep, maskedResponse)
		}
	}
}

// The field is masked wherever it sits, not only at the top level: a streamed
// terminal event carries it under response.
func TestMaskingReachesNestedAndStreamedBodies(t *testing.T) {
	stream := "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"model\":\"gpt-6-astra\",\"output\":[]}}\n\n" +
		"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"model\":\"gpt-6-astra\",\"output\":[{\"content\":[{\"text\":\"the private answer\"}]}]}}\n\n" +
		"data: [DONE]\n\n"
	masked := string(maskResponseBody([]byte(stream)))
	if strings.Contains(masked, "the private answer") {
		t.Fatalf("a nested answer survived: %s", masked)
	}
	// Framing and the fields the stream is read for are untouched.
	for _, keep := range []string{"event: response.created", "event: response.completed", "gpt-6-astra", "[DONE]"} {
		if !strings.Contains(masked, keep) {
			t.Fatalf("masking damaged the stream, %q is gone: %s", keep, masked)
		}
	}
	// And the masked stream is still readable by the model observer.
	var observer modelObserver
	observer.observeBody([]byte(masked))
	if got := observer.model(); got != "gpt-6-astra" {
		t.Fatalf("model=%q after masking", got)
	}
}

// Masking must never corrupt a body. A payload it cannot parse is left alone
// rather than half-rewritten.
func TestUnparseableBodiesArePassedThrough(t *testing.T) {
	for _, body := range []string{"", "not json at all", "{broken", "<html>nope</html>"} {
		if got := string(maskRequestBody([]byte(body))); got != body {
			t.Fatalf("%q was rewritten to %q", body, got)
		}
	}
}

// The replacement says how large the value was, so a masked body can be told
// apart from one that never carried a conversation.
func TestTheMaskRecordsTheSizeItReplaced(t *testing.T) {
	body := `{"input":[{"a":"bb"}]}`
	masked := maskRequestBody([]byte(body))
	var decoded map[string]any
	if err := json.Unmarshal(masked, &decoded); err != nil {
		t.Fatalf("masked body is not valid JSON: %s", masked)
	}
	text, _ := decoded["input"].(string)
	if !strings.HasPrefix(text, "[MASKED ") || !strings.HasSuffix(text, " bytes]") {
		t.Fatalf("input=%q", text)
	}
	if text == "[MASKED 0 bytes]" {
		t.Fatalf("a non-empty value should report its size: %q", text)
	}
}
