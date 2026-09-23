package main

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
