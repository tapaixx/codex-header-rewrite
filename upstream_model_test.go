package main

import "testing"

func TestCallbackModelsWithoutSSEDelimiters(t *testing.T) {
	for _, prefix := range []string{"", "data: "} {
		var o modelObserver
		o.observeCallback([]byte(prefix + `{"type":"response.created","response":{"model":"early"}}`))
		o.observeCallback([]byte(prefix + `{"type":"response.completed","response":{"model":"final"}}`))
		o.observeCallback([]byte(prefix + `{"type":"response.in_progress","response":{"model":"late"}}`))
		o.flushStream()
		if o.model() != "final" || !o.conflicted() {
			t.Fatalf("prefix %q: model=%q conflict=%v", prefix, o.model(), o.conflicted())
		}
	}
}

func TestClaudeAndCRLFModels(t *testing.T) {
	for _, body := range []string{
		"event: message_start\ndata: {\"message\":{\"model\":\"actual\"}}\n\n",
		"data: {\"response\":{\"model\":\"actual\"}}\r\n\r\ndata: [DONE]\r\n\r\n",
	} {
		var o modelObserver
		// One-byte chunks exercise CRLF split across boundaries.
		for i := range body {
			o.observeStream([]byte(body[i : i+1]))
		}
		o.flushStream()
		if o.model() != "actual" {
			t.Fatalf("model=%q for %q", o.model(), body)
		}
	}
}

func TestCallbackPreservesSplitFrames(t *testing.T) {
	var o modelObserver
	o.observeCallback([]byte("data: {\"response\":{\"mo"))
	o.observeCallback([]byte("del\":\"actual\"}}\n\n"))
	if o.model() != "actual" {
		t.Fatalf("model=%q", o.model())
	}
}

func TestTerminalDeclarationWinsOverEarlierFrames(t *testing.T) {
	var o modelObserver
	o.observeStream([]byte("event: response.created\ndata: {\"response\":{\"model\":\"gpt-5.6-luna\"}}\n\n"))
	o.observeStream([]byte("event: response.completed\ndata: {\"response\":{\"model\":\"gpt-5.6-luna-mini\"}}\n\n"))
	o.flushStream()
	if got := o.model(); got != "gpt-5.6-luna-mini" {
		t.Fatalf("model=%q", got)
	}
	if !o.conflicted() {
		t.Fatal("disagreeing declarations should raise a conflict")
	}
}

func TestFrameSplitAcrossChunksIsStillRead(t *testing.T) {
	var o modelObserver
	o.observeStream([]byte("event: response.completed\ndata: {\"response\":{\"mo"))
	if got := o.model(); got != "" {
		t.Fatalf("partial frame should declare nothing, got %q", got)
	}
	o.observeStream([]byte("del\":\"gpt-5.6-luna\"}}\n\n"))
	if got := o.model(); got != "gpt-5.6-luna" {
		t.Fatalf("model=%q", got)
	}
}

// A complete frame is read at the chunk boundary rather than held for a blank
// line that may never arrive. Waiting for one is what made every live stream
// report no model at all: the host hands over one event per callback and
// strips the separator, so the observer buffered the stream and read none of
// it. flushStream stays safe to call afterwards and adds nothing.
func TestFinalFrameWithoutBlankLineIsRead(t *testing.T) {
	var o modelObserver
	o.observeStream([]byte("event: response.completed\ndata: {\"response\":{\"model\":\"gpt-5.6-luna\"}}"))
	if got := o.model(); got != "gpt-5.6-luna" {
		t.Fatalf("model=%q at the chunk boundary", got)
	}
	if len(o.residual) != 0 {
		t.Fatalf("a frame that was read should not stay buffered: %q", o.residual)
	}
	o.flushStream()
	if got := o.model(); got != "gpt-5.6-luna" || o.conflicted() {
		t.Fatalf("model=%q conflict=%v after flush", got, o.conflicted())
	}
}

func TestDoneSentinelAndDeltaFramesAreIgnored(t *testing.T) {
	var o modelObserver
	o.observeStream([]byte("event: response.output_text.delta\ndata: {\"delta\":\"OK\"}\n\ndata: [DONE]\n\n"))
	o.flushStream()
	if got := o.model(); got != "" {
		t.Fatalf("model=%q", got)
	}
	if o.conflicted() {
		t.Fatal("no declaration should mean no conflict")
	}
}

func TestNonStreamingBodyDeclaresModel(t *testing.T) {
	var o modelObserver
	o.observeBody([]byte(`{"id":"resp_1","model":"gpt-5.6-luna","output":[]}`))
	if got := o.model(); got != "gpt-5.6-luna" {
		t.Fatalf("model=%q", got)
	}
}

func TestMalformedPayloadDeclaresNothing(t *testing.T) {
	var o modelObserver
	o.observeBody([]byte(`{"model":"gpt-5.6-luna"`))
	if got := o.model(); got != "" {
		t.Fatalf("truncated JSON must not declare a model, got %q", got)
	}
}

func TestMismatchIsUnknownWithoutAnUpstreamDeclaration(t *testing.T) {
	if got := modelMismatch("gpt-5.6-luna", ""); got != nil {
		t.Fatalf("expected unknown, got %v", *got)
	}
	if got := modelMismatch("gpt-5.6-luna", "GPT-5.6-Luna"); got == nil || *got {
		t.Fatalf("case difference is not a mismatch: %v", got)
	}
	if got := modelMismatch("gpt-5.6-luna", "gpt-5.6-luna-mini"); got == nil || !*got {
		t.Fatalf("expected mismatch, got %v", got)
	}
	if got := modelMismatch("", "gpt-5.6-luna"); got == nil || !*got {
		t.Fatalf("unknown sent model against a declared upstream model is a mismatch: %v", got)
	}
}

func TestSentModelPrefersTheMappedModel(t *testing.T) {
	if got := sentModel("gpt-5.6-luna", "gpt-5"); got != "gpt-5.6-luna" {
		t.Fatalf("sent=%q", got)
	}
	if got := sentModel("  ", "gpt-5"); got != "gpt-5" {
		t.Fatalf("sent=%q", got)
	}
}

func TestObservedModelIsLengthCapped(t *testing.T) {
	long := make([]rune, upstreamModelMaxLength+50)
	for i := range long {
		long[i] = 'm'
	}
	var o modelObserver
	o.observe(string(long), true)
	if got := len([]rune(o.model())); got != upstreamModelMaxLength {
		t.Fatalf("length=%d", got)
	}
}

// Every shape a host has been seen to hand a stream over in. The one that was
// missed in practice is a single complete event with no blank line after it:
// there is no separator left to find, so the observer buffered the whole
// stream and read none of it. Across 153 real records not one upstream model
// was captured.
func TestEveryStreamDeliveryShapeIsRead(t *testing.T) {
	const (
		created   = `{"type":"response.created","sequence_number":0,"response":{"id":"r","object":"response","model":"gpt-6-astra","status":"in_progress"}}`
		delta     = `{"type":"response.output_text.delta","sequence_number":1,"delta":"hi"}`
		completed = `{"type":"response.completed","sequence_number":9,"response":{"id":"r","object":"response","model":"gpt-6-astra","status":"completed"}}`
	)
	cases := []struct {
		name   string
		chunks []string
	}{
		{"frames terminated by a blank line", []string{
			"event: response.created\ndata: " + created + "\n\n",
			"event: response.output_text.delta\ndata: " + delta + "\n\n",
			"event: response.completed\ndata: " + completed + "\n\n"}},
		{"one event per callback, no blank line", []string{
			"event: response.created\ndata: " + created,
			"event: response.output_text.delta\ndata: " + delta,
			"event: response.completed\ndata: " + completed}},
		{"one event per callback, single newline", []string{
			"event: response.created\ndata: " + created + "\n",
			"event: response.completed\ndata: " + completed + "\n"}},
		{"data line with no event line", []string{
			"data: " + created + "\n\n", "data: " + completed + "\n\n"}},
		{"data line with no newline at all", []string{
			"data: " + created, "data: " + completed}},
		{"bare JSON payloads", []string{created, completed}},
		{"one frame split across two chunks", []string{
			"event: response.created\ndata: " + created[:40], created[40:] + "\n\n"}},
		{"several frames in one chunk", []string{
			"event: response.created\ndata: " + created + "\n\nevent: response.completed\ndata: " + completed + "\n\n"}},
		{"several events packed into one frame", []string{
			"data: " + created + "\ndata: " + completed + "\n\n"}},
		{"CRLF framing", []string{
			"event: response.created\r\ndata: " + created + "\r\n\r\n",
			"event: response.completed\r\ndata: " + completed + "\r\n\r\n"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var observer modelObserver
			for _, chunk := range tc.chunks {
				observer.observeCallback([]byte(chunk))
			}
			observer.flushStream()
			if got := observer.model(); got != "gpt-6-astra" {
				t.Fatalf("model=%q", got)
			}
			if observer.conflicted() {
				t.Fatal("one model was declared, so there is no conflict")
			}
		})
	}
}

// Parsing the tail of a chunk must not eat a frame that is merely truncated:
// its payload is not valid JSON yet, so it has to keep buffering.
func TestATruncatedFrameKeepsBuffering(t *testing.T) {
	const created = `{"type":"response.created","response":{"model":"gpt-6-astra"}}`
	var observer modelObserver
	observer.observeCallback([]byte("event: response.created\ndata: " + created[:30]))
	if got := observer.model(); got != "" {
		t.Fatalf("half a frame declared %q", got)
	}
	if len(observer.residual) == 0 {
		t.Fatal("the half frame should still be buffered")
	}
	observer.observeCallback([]byte(created[30:]))
	if got := observer.model(); got != "gpt-6-astra" {
		t.Fatalf("model=%q after the frame completed", got)
	}
	if observer.conflicted() {
		t.Fatal("the two halves are one declaration, not two")
	}
}
