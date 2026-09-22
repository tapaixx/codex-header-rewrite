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

// Only three kinds of frame say anything the panel uses. Everything else is
// the answer arriving in fragments, and delta is not named output, so masking
// by field name never touched it.
func TestOnlyInformativeFramesSurvive(t *testing.T) {
	stream := strings.Join([]string{
		`event: response.created`,
		`data: {"type":"response.created","response":{"model":"gpt-6-astra","reasoning":{"effort":"low"},"output":[],"tools":[{"type":"function","name":"exec","description":"a very long schema repeated on every single turn"}]}}`,
		``,
		`event: response.in_progress`,
		`data: {"type":"response.in_progress","response":{"model":"gpt-6-astra","instructions":"a very long repeat of everything"}}`,
		``,
		`event: response.output_text.delta`,
		`data: {"type":"response.output_text.delta","delta":"the private answer","obfuscation":"zz"}`,
		``,
		`event: response.output_item.done`,
		`data: {"type":"response.output_item.done","item":{"content":[{"text":"the private answer again"}]}}`,
		``,
		`event: response.completed`,
		`data: {"type":"response.completed","response":{"model":"gpt-6-astra","status":"completed","output":[{"text":"the private answer"}],"usage":{"input_tokens":21190}}}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")
	masked := string(maskResponseBody([]byte(stream)))

	if strings.Contains(masked, "the private answer") {
		t.Fatalf("the answer survived:\n%s", masked)
	}
	// The tool schemas are the same declarations on every turn; knowing they
	// were there says as much as reading them again.
	if strings.Contains(masked, "a very long schema repeated on every single turn") {
		t.Fatalf("the tool schemas survived:\n%s", masked)
	}
	if !strings.Contains(masked, `"tools":"[MASKED `) {
		t.Fatalf("tools should be masked by name, not dropped:\n%s", masked)
	}
	// Kept whole apart from output: the model, the settings, the usage.
	for _, keep := range []string{`"model":"gpt-6-astra"`, `"effort":"low"`, `"status":"completed"`, `"input_tokens":21190`} {
		if !strings.Contains(masked, keep) {
			t.Fatalf("masking dropped %s:\n%s", keep, masked)
		}
	}
	// Replaced outright, with the type left so the sequence still reads.
	for _, gone := range []string{"response.in_progress", "response.output_text.delta", "response.output_item.done"} {
		if !strings.Contains(masked, `"type":"`+gone+`"`) {
			t.Fatalf("the sequence lost %s:\n%s", gone, masked)
		}
	}
	if strings.Count(masked, `"masked":"`) != 3 {
		t.Fatalf("expected three replaced frames:\n%s", masked)
	}
	if strings.Contains(masked, "a very long repeat of everything") {
		t.Fatalf("in_progress merely repeats created and should be gone:\n%s", masked)
	}
	// Framing and the sentinel are untouched, and the result still parses.
	for _, keep := range []string{"event: response.created", "event: response.completed", "data: [DONE]"} {
		if !strings.Contains(masked, keep) {
			t.Fatalf("framing damaged, %q is gone:\n%s", keep, masked)
		}
	}
	var observer modelObserver
	observer.observeBody([]byte(masked))
	if got := observer.model(); got != "gpt-6-astra" {
		t.Fatalf("model=%q after masking", got)
	}
	if got := observer.effort(); got != "low" {
		t.Fatalf("effort=%q after masking", got)
	}
}

// The host sends streams with the framing gone. A line-based pass matched
// nothing in one of those, so every fragment of the answer was stored.
func TestMaskingReachesAnUnframedStream(t *testing.T) {
	stream := `event: response.created` +
		`data: {"type":"response.created","response":{"model":"gpt-6-astra","output":[]}}` +
		`event: response.output_text.delta` +
		`data: {"type":"response.output_text.delta","delta":"the private answer"}` +
		`event: response.completed` +
		`data: {"type":"response.completed","response":{"model":"gpt-6-astra","status":"completed"}}`
	masked := string(maskResponseBody([]byte(stream)))
	if strings.Contains(masked, "the private answer") {
		t.Fatalf("an unframed answer survived:\n%s", masked)
	}
	if !strings.Contains(masked, `"model":"gpt-6-astra"`) || !strings.Contains(masked, `"status":"completed"`) {
		t.Fatalf("the informative frames should survive:\n%s", masked)
	}
	// Key order comes from marshalling a map, so check for the pair rather than
	// for one particular spelling of it.
	if !strings.Contains(masked, `"type":"response.output_text.delta"`) || !strings.Contains(masked, `"masked":`) {
		t.Fatalf("the delta frame should be replaced:\n%s", masked)
	}
	var observer modelObserver
	observer.observeBody([]byte(masked))
	if got := observer.model(); got != "gpt-6-astra" {
		t.Fatalf("model=%q after masking an unframed stream", got)
	}
}

// The two directions mask different things, and only what each is told to.
func TestMaskedFieldsPerDirection(t *testing.T) {
	both := `{"input":["ask"],"output":["answer"],"tools":[{"name":"exec"}],"model":"gpt-6-astra"}`

	request := string(maskRequestBody([]byte(both)))
	if !strings.Contains(request, `"input":"[MASKED `) {
		t.Fatalf("a request masks input: %s", request)
	}
	// A request body's own tools are left alone: the ask was for the response's.
	for _, keep := range []string{`"output":["answer"]`, `"tools":[{"name":"exec"}]`, `"model":"gpt-6-astra"`} {
		if !strings.Contains(request, keep) {
			t.Fatalf("a request should keep %s: %s", keep, request)
		}
	}

	response := string(maskResponseBody([]byte(both)))
	for _, name := range []string{"output", "tools"} {
		if !strings.Contains(response, `"`+name+`":"[MASKED `) {
			t.Fatalf("a response masks %s: %s", name, response)
		}
	}
	for _, keep := range []string{`"input":["ask"]`, `"model":"gpt-6-astra"`} {
		if !strings.Contains(response, keep) {
			t.Fatalf("a response should keep %s: %s", keep, response)
		}
	}
}
