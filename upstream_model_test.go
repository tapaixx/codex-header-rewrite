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

// A host can hand over a stream with the framing gone: "event: response.created"
// written straight against "data: {...}" with no newline between them. The
// line-based parse sees one event line containing no data at all, so every
// declaration in the run is lost. Payloads are recovered by balancing brackets
// from each data: marker instead.
func TestAStreamWithNoFramingIsStillRead(t *testing.T) {
	const created = `{"type":"response.created","response":{"id":"r","model":"gpt-6-astra","output":[]}}`
	const delta = `{"type":"response.output_text.delta","delta":"hi","obfuscation":"a{b}c\"d"}`
	const completed = `{"type":"response.completed","response":{"id":"r","model":"gpt-6-astra","status":"completed"}}`
	cases := []struct {
		name   string
		chunks []string
	}{
		{"no newline between event and data", []string{
			"event: response.created" + "data: " + created +
				"event: response.output_text.delta" + "data: " + delta +
				"event: response.completed" + "data: " + completed}},
		{"one run-together event per chunk", []string{
			"event: response.createddata: " + created,
			"event: response.completeddata: " + completed}},
		{"newline after the payload only", []string{
			"event: response.createddata: " + created + "\nevent: response.completeddata: " + completed}},
		{"framing gone and [DONE] still present", []string{
			"event: response.completeddata: " + completed + "data: [DONE]"}},
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

// Recovering payloads without framing must not swallow a truncated one: the
// bytes after the last complete value stay buffered for the next chunk.
func TestAnUnframedTruncatedPayloadKeepsBuffering(t *testing.T) {
	const completed = `{"type":"response.completed","response":{"model":"gpt-6-astra"}}`
	const created = `{"type":"response.created","response":{"model":"gpt-5.6-luna"}}`
	var observer modelObserver
	// One whole event, then half of the next, in a single chunk.
	observer.observeCallback([]byte("event: response.createddata: " + created + "event: response.completeddata: " + completed[:30]))
	if got := observer.model(); got != "gpt-5.6-luna" {
		t.Fatalf("the complete event should have been read, got %q", got)
	}
	if len(observer.residual) == 0 {
		t.Fatal("the truncated event should still be buffered")
	}
	observer.observeCallback([]byte(completed[30:]))
	if got := observer.model(); got != "gpt-6-astra" {
		t.Fatalf("model=%q once the truncated event completed", got)
	}
}

// A brace inside a string must not end the scan early, which is exactly what
// the obfuscation field in a real delta frame contains.
func TestBracketsInsideStringsDoNotEndAPayload(t *testing.T) {
	payloads := salvageDataPayloads([]byte(`data: {"a":"}{[]","b":{"c":1}}data: {"d":2}`))
	if len(payloads) != 2 {
		t.Fatalf("got %d payloads", len(payloads))
	}
	if string(payloads[0].payload) != `{"a":"}{[]","b":{"c":1}}` {
		t.Fatalf("first=%s", payloads[0].payload)
	}
	if string(payloads[1].payload) != `{"d":2}` {
		t.Fatalf("second=%s", payloads[1].payload)
	}
}

// The reasoning effort is recorded beside the model, on the same
// terminal-wins rule, and never enters the comparison.
func TestReasoningEffortIsObservedAndNotCompared(t *testing.T) {
	var observer modelObserver
	observer.observeCallback([]byte(`event: response.created
data: {"type":"response.created","response":{"model":"gpt-6-astra","reasoning":{"effort":"low"}}}

`))
	if got := observer.effort(); got != "low" {
		t.Fatalf("effort=%q from the first event", got)
	}
	observer.observeCallback([]byte(`event: response.completed
data: {"type":"response.completed","response":{"model":"gpt-6-astra","reasoning":{"effort":"high"}}}

`))
	if got := observer.effort(); got != "high" {
		t.Fatalf("effort=%q, the terminal event should win", got)
	}
	// Two different efforts are not a conflict; only two models are.
	if observer.conflicted() {
		t.Fatal("a changed effort is not a model conflict")
	}
	// And it plays no part in the mismatch verdict.
	if got := modelMismatch("gpt-6-astra", observer.model()); got == nil || *got {
		t.Fatalf("same model with different efforts is not a mismatch: %v", got)
	}
}

func TestRequestReasoningEffort(t *testing.T) {
	cases := []struct{ body, want string }{
		{`{"model":"gpt-6-astra","reasoning":{"effort":"xhigh","context":"all_turns"}}`, "xhigh"},
		{`{"model":"gpt-6-astra","reasoning":{}}`, ""},
		{`{"model":"gpt-6-astra"}`, ""},
		{`{"reasoning":{"effort":"  medium  "}}`, "medium"},
		{`not json`, ""},
		{``, ""},
		{`[{"reasoning":{"effort":"low"}}]`, ""}, // a request body is an object
	}
	for _, tc := range cases {
		if got := requestReasoningEffort([]byte(tc.body)); got != tc.want {
			t.Fatalf("%s -> %q want %q", tc.body, got, tc.want)
		}
	}
}
