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

// The blob-level rule is paused: it pools everything and judges nothing. It
// is what the retry and a manual pool fall back on.
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

// The live rule reads the exchange, not the blob: a turn that went out
// carrying an injected state and came back without one is the healthy one.
func TestInjectedTurnRule(t *testing.T) {
	cases := []struct {
		injected, returned bool
		judged, degraded   bool
	}{
		{true, false, true, false},  // injected, upstream wrote none: healthy
		{true, true, true, true},    // injected, upstream wrote one: degraded
		{false, true, false, false}, // nothing injected: nothing to read
		{false, false, false, false},
	}
	for _, tc := range cases {
		v := judgeInjectedTurn(tc.injected, tc.returned)
		if v.Judged != tc.judged || v.Degraded != tc.degraded || v.Rule != degradedRuleInjected {
			t.Fatalf("injected=%v returned=%v: %+v", tc.injected, tc.returned, v)
		}
		if v.Eligible == tc.degraded && tc.judged {
			t.Fatalf("a degraded turn is not poolable: %+v", v)
		}
	}
}

// The multi-check reads the probe's second request with the live rule; a
// failed check says nothing.
func TestProbeVerificationRule(t *testing.T) {
	if v := judgeProbeVerification(false, false); !v.Judged || v.Degraded || !v.Eligible || v.Rule != degradedRuleVerify {
		t.Fatalf("no state written back: %+v", v)
	}
	if v := judgeProbeVerification(true, false); !v.Judged || !v.Degraded || v.Eligible {
		t.Fatalf("a state written back: %+v", v)
	}
	if v := judgeProbeVerification(false, true); v.Judged || v.Eligible || v.Rule != degradedRuleVerify {
		t.Fatalf("a failed check: %+v", v)
	}
}

// data_path is the only field the plugin takes from the server config.
func TestRegistrationDeclaresItsConfigFields(t *testing.T) {
	names := []string{}
	for _, field := range pluginRegistration().Metadata.ConfigFields {
		names = append(names, field.Name+":"+field.Type)
	}
	if strings.Join(names, ",") != "data_path:string" {
		t.Fatalf("config fields = %v", names)
	}
	if cfg := parsePluginConfig([]byte("enabled: true\ndata_path: \"x.db\"\n")); cfg.DataPath != "x.db" {
		t.Fatalf("cfg=%+v", cfg)
	}
}

// The cookie cooldown is optional, and bounded when set.
func TestCookieCooldownBounds(t *testing.T) {
	for _, tc := range []struct {
		seconds int
		ok      bool
	}{{0, true}, {60, true}, {maxCookieCooldownSeconds, true}, {-1, false}, {maxCookieCooldownSeconds + 1, false}} {
		_, err := validateRule(headerRule{AuthIndex: "i", CookieCooldownSeconds: tc.seconds})
		if (err == nil) != tc.ok {
			t.Fatalf("%d seconds: err=%v", tc.seconds, err)
		}
	}
}

// Probe header overrides are canonical, and the plugin's own headers are
// refused.
func TestCleanProbeHeaders(t *testing.T) {
	out, err := cleanProbeHeaders(map[string]string{" x-client-version ": "1.2", "User-Agent": "probe/1"})
	if err != nil || out["X-Client-Version"] != "1.2" || out["User-Agent"] != "probe/1" {
		t.Fatalf("out=%v err=%v", out, err)
	}
	for _, bad := range []map[string]string{
		{"Authorization": "Bearer x"}, {"cookie": "a=1"}, {"X-Codex-Turn-State": "s"},
		{"bad header": "v"}, {"X-Ok": "line\r\nbreak"},
	} {
		if _, err := cleanProbeHeaders(bad); err == nil {
			t.Fatalf("%v should be refused", bad)
		}
	}
}
