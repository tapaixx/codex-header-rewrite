package main

import (
	"strings"
	"testing"
)

// useLengthJudgement puts the former length rule in force for one test, for
// the tests that exercise interception, eviction and the retry -- all of
// which need a verdict to act on.
func useLengthJudgement(t *testing.T) {
	t.Helper()
	previous := degradedJudge
	degradedJudge = judgeByLength
	t.Cleanup(func() { degradedJudge = previous })
}

// Paused, the judgement pools everything and judges nothing.
func TestPausedJudgementPoolsEverythingAndJudgesNothing(t *testing.T) {
	for _, blob := range []string{"short", strings.Repeat("x", 400)} {
		for _, plan := range []string{"team", "pro", ""} {
			v := judgeDegraded(blob, plan)
			if !v.Eligible || v.Judged || v.Rule != degradedRulePaused {
				t.Fatalf("%d chars, plan %q: %+v", len(blob), plan, v)
			}
		}
	}
}

// The length rule is kept intact behind the switch.
func TestLengthJudgementStillWorksWhenSwitchedOn(t *testing.T) {
	useLengthJudgement(t)
	cases := []struct {
		chars    int
		plan     string
		eligible bool
		judged   bool
	}{
		{332, "team", true, true}, {333, "team", false, true},
		{292, "pro", true, true}, {293, "plus", false, true},
		{100, "", false, false},
	}
	for _, tc := range cases {
		v := judgeDegraded(strings.Repeat("x", tc.chars), tc.plan)
		if v.Eligible != tc.eligible || v.Judged != tc.judged || v.Rule != degradedRuleLength {
			t.Fatalf("%d chars, plan %q: %+v", tc.chars, tc.plan, v)
		}
	}
}

// Live states pool themselves only under a rule that judged them.
func TestLivePoolingFollowsTheJudgement(t *testing.T) {
	if autoPoolsLive(judgePaused("x", "team")) {
		t.Fatal("paused, a live state waits for a manual pool")
	}
	if !autoPoolsLive(judgeByLength(strings.Repeat("x", 300), "team")) {
		t.Fatal("under a working rule an eligible live state pools itself")
	}
	if autoPoolsLive(judgeByLength(strings.Repeat("x", 400), "team")) {
		t.Fatal("a degraded state never pools")
	}
}

// The probe and the retry ask for low effort; without the field the upstream
// runs its default, which is medium.
func TestBackgroundRequestsAskForLowEffort(t *testing.T) {
	body, err := probeRequestBody("gpt-6-astra")
	if err != nil {
		t.Fatal(err)
	}
	if got := requestThinkingLevel(body, "openai-response", "gpt-6-astra"); got != "low" {
		t.Fatalf("background request effort = %q, want low", got)
	}
}

// The response rule reads names, not prose. The marker is a stand-in.
func TestResponseMarkersMatchNamesNotText(t *testing.T) {
	markers := []string{"fixture_marker"}
	cases := []struct {
		name string
		body string
		hit  bool
	}{
		{"sse event line", "event: response.fixture_marker_part.added\ndata: {}\n\n", true},
		{"sse data type", "data: {\"type\":\"response.fixture_marker_text.done\"}\n\n", true},
		{"nested field name", "data: {\"type\":\"response.completed\",\"response\":{\"fixture_marker\":[]}}\n\n", true},
		{"crlf framing", "event: x\r\ndata: {\"type\":\"response.Fixture_Marker\"}\r\n\r\n", true},
		{"whole json body", `{"output":[{"type":"fixture_marker_item"}]}`, true},
		{"text that mentions it", "data: {\"type\":\"response.output_text.delta\",\"delta\":\"fixture_marker\"}\n\n", false},
		{"clean", "data: {\"type\":\"response.completed\"}\n\ndata: [DONE]\n\n", false},
		{"empty", "", false},
	}
	for _, tc := range cases {
		if got := responseHasMarker([]byte(tc.body), markers); got != tc.hit {
			t.Fatalf("%s: hit=%v, want %v", tc.name, got, tc.hit)
		}
	}
}

// No markers, no response judgement: the probe falls back to the state rule.
func TestProbeJudgementFallsBackWithoutMarkers(t *testing.T) {
	body := []byte("data: {\"type\":\"response.fixture_marker\"}\n\n")
	if v := judgeProbeResponse("blob", "team", body, nil); v.Rule != degradedRulePaused || !v.Eligible {
		t.Fatalf("without markers the paused rule answers: %+v", v)
	}
	if v := judgeProbeResponse("blob", "team", body, []string{"fixture_marker"}); v.Rule != degradedRuleResponse || v.Eligible || !v.Degraded {
		t.Fatalf("a hit is degraded: %+v", v)
	}
	if v := judgeProbeResponse("blob", "team", []byte("data: {}\n\n"), []string{"fixture_marker"}); v.Rule != degradedRuleResponse || !v.Eligible || !v.Judged || v.Degraded {
		t.Fatalf("a clean response is judged non-degraded: %+v", v)
	}
}

// The server config names the markers; the data path still reads as before.
func TestPluginConfigReadsProbeMarkers(t *testing.T) {
	cfg := parsePluginConfig([]byte("enabled: true\ndata_path: \"x.db\"\nprobe_degraded_markers: [\"one\", two ,'three']\n"))
	if cfg.DataPath != "x.db" || strings.Join(cfg.ProbeMarkers, "|") != "one|two|three" {
		t.Fatalf("cfg=%+v", cfg)
	}
	if cfg := parsePluginConfig([]byte("data_path: y.db\n")); len(cfg.ProbeMarkers) != 0 {
		t.Fatalf("no key, no markers: %+v", cfg)
	}
}
