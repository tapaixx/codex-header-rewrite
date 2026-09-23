package main

import "time"

// The degradation judgement lives here, and nowhere else: pooling, injection,
// interception, retry, eviction and the history verdicts all ask this file.
//
// Three rules, because the three paths see different evidence:
//
//   - judgeInjectedTurn  live requests, from whether the upstream wrote a
//     state back over one the plugin had injected.
//   - judgeProbeLatency  the automatic probe, from how long its own minimal
//     request took.
//   - degradedJudge      the blob itself, which is all the retry and a manual
//     pool have to go on. Currently judgePaused: no verdict, nothing kept out.
//     The wire-length rule (team ≤ 332 characters, personal ≤ 292) stopped
//     matching the upstream and is kept as judgeByLength.
//
// None of them reads the response body.

// degradedVerdict is the answer about one observed state.
type degradedVerdict struct {
	// Eligible says whether the state may enter the pool. A rule that judged
	// it degraded keeps it out; so does a rule that could not judge it at all
	// while a judgement is required (no plan claim under the length rule).
	Eligible bool
	// Judged is whether a rule actually produced Degraded. Nil verdicts are
	// how an unjudged turn and an unknown plan both read in history.
	Judged   bool
	Degraded bool
	// Suspect is the probe rule's middle band: not clean enough to pool, not
	// slow enough to call degraded. Judged stays false -- there is no verdict,
	// only a reason to keep the state out of the pool.
	Suspect bool
	// Elapsed is how long the request took, when that is what the rule read.
	Elapsed time.Duration
	// MaxChars is the length limit the rule applied, when it applied one.
	MaxChars int
	// Rule names what answered: "length" or "paused". History shows it so a
	// row explains why it carries no verdict.
	Rule string
}

const (
	degradedRuleLength = "length"
	degradedRulePaused = "paused"
	// degradedRuleInjected is the live rule, read from whether the upstream
	// wrote a state back over one the plugin had injected.
	degradedRuleInjected = "injected"
	// degradedRuleLatency is the probe rule, read from how long the probe's
	// own request took.
	degradedRuleLatency = "latency"

	teamStateMaxChars     = 332
	personalStateMaxChars = 292

	// probeCleanLatency and probeSuspectLatency divide a probe's round trip
	// into three: quick enough to trust, slow enough to doubt, slower still.
	probeCleanLatency   = 10 * time.Second
	probeSuspectLatency = 20 * time.Second
)

// degradedJudge is the rule in force. Swap it here to change the judgement.
var degradedJudge = judgePaused

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

// judgeInjectedTurn is the rule for live requests, and it reads the exchange
// rather than the blob. A turn already carrying a state the plugin injected
// has no reason to be handed another: the upstream writes one back when it has
// dropped that chain, which is the degraded case. So an injected turn that
// comes back with no state in its headers is the healthy one, and an injected
// turn that comes back with a state is not. With nothing injected the exchange
// says nothing either way, and the state waits for a manual pool.
func judgeInjectedTurn(injected, returned bool) degradedVerdict {
	if !injected {
		return degradedVerdict{Rule: degradedRuleInjected}
	}
	return degradedVerdict{Judged: true, Degraded: returned, Eligible: !returned, Rule: degradedRuleInjected}
}

// judgeProbeLatency is the rule for the automatic probe, which sends the same
// minimal request every time and so can be judged on how long it takes: a
// prompt answer comes from a healthy turn, a slow one does not. The middle
// band is suspect rather than degraded -- it keeps the state out of the pool
// without claiming to know.
func judgeProbeLatency(elapsed time.Duration) degradedVerdict {
	verdict := degradedVerdict{Rule: degradedRuleLatency, Elapsed: elapsed}
	switch {
	case elapsed <= probeCleanLatency:
		verdict.Judged, verdict.Eligible = true, true
	case elapsed <= probeSuspectLatency:
		verdict.Suspect = true
	default:
		verdict.Judged, verdict.Degraded = true, true
	}
	return verdict
}
