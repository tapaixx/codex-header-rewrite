package main

// The degradation judgement lives here, and nowhere else: pooling, injection,
// interception, retry, eviction and the history verdicts all ask this file.
//
// Three rules, because the three paths see different evidence:
//
//   - judgeInjectedTurn  live requests, from whether the upstream wrote a
//     state back over one the plugin had injected.
//   - judgeProbeVerification  the automatic probe with multi-check on: the
//     same rule, applied to a second request carrying the probe's own state.
//     With multi-check off the probe pools what it gets, unjudged.
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
	// degradedRuleVerify is the probe's multi-check: the injection rule,
	// applied to a second request the probe sends with its own state.
	degradedRuleVerify = "verify"

	teamStateMaxChars     = 332
	personalStateMaxChars = 292
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

// judgeProbeVerification is the probe's rule when multi-check is on. The probe
// has just obtained a state; it sends one more minimal request carrying that
// state and no cookie, and reads the answer the way the live rule reads a
// client's turn: no state written back means the state held, a state written
// back means it did not. A verification that failed -- no answer, or not a
// 2xx one -- says nothing, and an unverified state is not pooled.
func judgeProbeVerification(returned, failed bool) degradedVerdict {
	if failed {
		return degradedVerdict{Rule: degradedRuleVerify}
	}
	verdict := judgeInjectedTurn(true, returned)
	verdict.Rule = degradedRuleVerify
	return verdict
}
