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
