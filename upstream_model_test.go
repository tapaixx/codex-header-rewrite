package main

import "testing"

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

func TestFinalFrameWithoutBlankLineIsRead(t *testing.T) {
	var o modelObserver
	o.observeStream([]byte("event: response.completed\ndata: {\"response\":{\"model\":\"gpt-5.6-luna\"}}"))
	if got := o.model(); got != "" {
		t.Fatalf("unterminated frame should wait for more input, got %q", got)
	}
	o.flushStream()
	if got := o.model(); got != "gpt-5.6-luna" {
		t.Fatalf("model=%q", got)
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
