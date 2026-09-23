package main

import (
	"bytes"
	"encoding/json"
	"strings"
)

// The degradation judgement is the one place that decides whether an observed
// X-Codex-Turn-State is degraded. Everything else -- pooling, injection,
// interception, retry, eviction, the history verdicts -- asks here and never
// looks at the blob itself, so changing the rule means changing this file.
//
// The rule in force is degradedJudge. The wire-length rule that used to run
// (team ≤ 332 characters, personal ≤ 292) stopped matching what the upstream
// does and is kept only as judgeByLength; judgePaused is what runs now: no
// verdict, every state eligible for the pool, nothing intercepted or evicted.

// degradedVerdict is the answer about one observed state.
type degradedVerdict struct {
	// Eligible says whether the state may enter the pool. A rule that judged
	// it degraded keeps it out; so does a rule that could not judge it at all
	// while a judgement is required (no plan claim under the length rule).
	Eligible bool
	// Judged is whether a rule actually produced Degraded. Nil verdicts are
	// how the paused rule and an unknown plan both read in history.
	Judged   bool
	Degraded bool
	// MaxChars is the length limit the rule applied, when it applied one.
	MaxChars int
	// Rule names what answered: "length" or "paused". History shows it so a
	// row explains why it carries no verdict.
	Rule string
}

const (
	degradedRuleLength = "length"
	degradedRulePaused = "paused"
	// degradedRuleResponse is the probe's own judgement, made on its response.
	degradedRuleResponse = "response"

	teamStateMaxChars     = 332
	personalStateMaxChars = 292
)

// degradedJudge is the rule in force. Swap it here to change the judgement.
var degradedJudge = judgePaused

// autoPoolsLive says whether a state that a client's own request brought back
// may enter the pool without an operator. Only a rule that actually judged the
// state can vouch for it, so while the judgement is paused a live state is
// recorded -- its provenance still feeds cross-account detection -- and waits
// in history for a manual pool. The plugin's own requests (the probe, the
// retry) are minimal calls and keep pooling on their own under any rule.
// With a working rule back in degradedJudge this is true for every eligible
// state and live pooling is automatic again.
func autoPoolsLive(v degradedVerdict) bool { return v.Eligible && v.Judged }

func judgeDegraded(blob, plan string) degradedVerdict { return degradedJudge(blob, plan) }

// judgePaused makes no judgement: every state is pooled and none is treated
// as degraded. It is the rule until a reliable signal replaces the length.
func judgePaused(string, string) degradedVerdict {
	return degradedVerdict{Eligible: true, Rule: degradedRulePaused}
}

// judgeByLength is the former rule, by wire-text length per plan: a team plan
// may run to 332 characters, every personal (non-team) plan to 292. A
// credential that claims no plan cannot be judged and is kept out of the pool,
// because guessing either limit would misjudge the other kind of account.
func judgeByLength(blob, plan string) degradedVerdict {
	verdict := degradedVerdict{Rule: degradedRuleLength}
	switch normalizePlanType(plan) {
	case "":
		return verdict
	case "team":
		verdict.MaxChars = teamStateMaxChars
	default:
		verdict.MaxChars = personalStateMaxChars
	}
	verdict.Judged = true
	verdict.Degraded = len(blob) > verdict.MaxChars
	verdict.Eligible = !verdict.Degraded
	return verdict
}

// judgeProbeResponse is the probe's judgement. A probe reads its response to
// the end before it pools anything, so unlike a live request -- whose state
// arrives with the headers, before any of the body -- it can be judged on the
// body. The markers are server configuration (probe_degraded_markers in the
// plugin's config block): they are not in this repository and not in the
// released binary. With none configured the probe falls back to degradedJudge.
//
// A response is degraded when one of its event names, event types or field
// names contains a marker. Text the model wrote is not searched, so an answer
// that merely mentions a marker does not count. A state rule that is back in
// degradedJudge keeps its say: its verdict stands when no marker is found.
func judgeProbeResponse(blob, plan string, body []byte, markers []string) degradedVerdict {
	base := judgeDegraded(blob, plan)
	if len(markers) == 0 {
		return base
	}
	if responseHasMarker(body, markers) {
		return degradedVerdict{Judged: true, Degraded: true, Rule: degradedRuleResponse}
	}
	if base.Judged || !base.Eligible {
		return base
	}
	return degradedVerdict{Eligible: true, Judged: true, Rule: degradedRuleResponse}
}

// responseHasMarker reads a whole JSON body or an SSE stream. Frames are split
// the way the model observer splits them; a frame whose data lines do not join
// into one JSON document is read line by line.
func responseHasMarker(body []byte, markers []string) bool {
	text := bytes.ReplaceAll(body, []byte("\r\n"), []byte("\n"))
	trimmed := bytes.TrimSpace(text)
	if len(trimmed) == 0 {
		return false
	}
	if trimmed[0] == '{' || trimmed[0] == '[' {
		return payloadHasMarker(trimmed, markers)
	}
	for _, frame := range bytes.Split(text, []byte("\n\n")) {
		var data [][]byte
		for _, line := range bytes.Split(frame, []byte("\n")) {
			if value, ok := bytes.CutPrefix(line, []byte("event:")); ok {
				if containsMarker(string(bytes.TrimSpace(value)), markers) {
					return true
				}
			} else if value, ok := bytes.CutPrefix(line, []byte("data:")); ok {
				data = append(data, bytes.TrimSpace(value))
			}
		}
		if len(data) == 0 {
			continue
		}
		if joined := bytes.Join(data, []byte("\n")); json.Valid(joined) {
			if payloadHasMarker(joined, markers) {
				return true
			}
			continue
		}
		for _, line := range data {
			if payloadHasMarker(line, markers) {
				return true
			}
		}
	}
	return false
}

func payloadHasMarker(payload []byte, markers []string) bool {
	var value any
	if json.Unmarshal(payload, &value) != nil {
		return false
	}
	return valueHasMarker(value, markers)
}

func valueHasMarker(value any, markers []string) bool {
	switch v := value.(type) {
	case map[string]any:
		for key, child := range v {
			if containsMarker(key, markers) {
				return true
			}
			if text, ok := child.(string); ok && key == "type" && containsMarker(text, markers) {
				return true
			}
			if valueHasMarker(child, markers) {
				return true
			}
		}
	case []any:
		for _, child := range v {
			if valueHasMarker(child, markers) {
				return true
			}
		}
	}
	return false
}

func containsMarker(text string, markers []string) bool {
	text = strings.ToLower(text)
	for _, marker := range markers {
		if marker = strings.ToLower(marker); marker != "" && strings.Contains(text, marker) {
			return true
		}
	}
	return false
}
