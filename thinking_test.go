package main

import "testing"

// The plugin reads the body before the host translates it, so the shape is the
// client's protocol and the setting is named differently in each. These cases
// follow CPA's own thinking.ExtractReasoningEffort, including its vocabulary
// and its priority, so what the panel shows is what the host would record.
func TestThinkingLevelAcrossRequestFormats(t *testing.T) {
	cases := []struct{ name, format, model, body, want string }{
		// Codex / OpenAI Responses
		{"codex effort", "openai-response", "gpt-6-astra", `{"reasoning":{"effort":"xhigh"}}`, "xhigh"},
		{"codex none", "openai-response", "m", `{"reasoning":{"effort":"none"}}`, "none"},
		{"codex says nothing", "openai-response", "m", `{"model":"m"}`, ""},
		// A Codex session changes effort mid-conversation with an update item,
		// and the latest one is the one in force.
		{"codex configuration_update wins", "openai-response", "m",
			`{"reasoning":{"effort":"low"},"input":[{"type":"message"},{"type":"configuration_update","reasoning":{"effort":"high"}}]}`, "high"},
		{"codex latest update wins", "openai-response", "m",
			`{"input":[{"type":"configuration_update","reasoning":{"effort":"low"}},{"type":"configuration_update","reasoning":{"effort":"xhigh"}}]}`, "xhigh"},

		// Claude Messages
		{"claude budget becomes a level", "claude", "claude-opus-5", `{"thinking":{"type":"enabled","budget_tokens":10000}}`, "high"},
		{"claude small budget", "claude", "m", `{"thinking":{"type":"enabled","budget_tokens":900}}`, "low"},
		{"claude tiny budget", "claude", "m", `{"thinking":{"type":"enabled","budget_tokens":400}}`, "minimal"},
		{"claude large budget", "claude", "m", `{"thinking":{"type":"enabled","budget_tokens":60000}}`, "xhigh"},
		{"claude disabled", "claude", "m", `{"thinking":{"type":"disabled","budget_tokens":10000}}`, "none"},
		{"claude enabled without a budget", "claude", "m", `{"thinking":{"type":"enabled"}}`, "auto"},
		{"claude budget zero", "claude", "m", `{"thinking":{"type":"enabled","budget_tokens":0}}`, "none"},
		{"claude budget auto", "claude", "m", `{"thinking":{"type":"enabled","budget_tokens":-1}}`, "auto"},
		{"claude adaptive effort", "claude", "m", `{"thinking":{"type":"adaptive"},"output_config":{"effort":"max"}}`, "max"},
		{"claude adaptive without effort", "claude", "m", `{"thinking":{"type":"adaptive"}}`, ""},

		// OpenAI chat completions
		{"openai reasoning_effort", "openai", "m", `{"reasoning_effort":"medium"}`, "medium"},
		{"openai falls back to responses", "openai", "m", `{"reasoning":{"effort":"low"}}`, "low"},

		// Gemini
		{"gemini level", "gemini", "m", `{"generationConfig":{"thinkingConfig":{"thinkingLevel":"high"}}}`, "high"},
		{"gemini snake case level", "gemini", "m", `{"generationConfig":{"thinkingConfig":{"thinking_level":"low"}}}`, "low"},
		{"gemini budget", "gemini", "m", `{"generationConfig":{"thinkingConfig":{"thinkingBudget":2000}}}`, "medium"},
		{"gemini level beats budget", "gemini", "m", `{"generationConfig":{"thinkingConfig":{"thinkingLevel":"minimal","thinkingBudget":60000}}}`, "minimal"},
		{"antigravity nests under request", "antigravity", "m", `{"request":{"generationConfig":{"thinkingConfig":{"thinkingLevel":"xhigh"}}}}`, "xhigh"},

		// The suffix outranks the body, which is the host's own priority.
		{"suffix level beats the body", "openai-response", "gpt-6-astra(high)", `{"reasoning":{"effort":"low"}}`, "high"},
		{"suffix budget beats the body", "claude", "claude-opus-5(16384)", `{"thinking":{"budget_tokens":100}}`, "high"},
		{"suffix none", "openai-response", "m(none)", `{"reasoning":{"effort":"high"}}`, "none"},
		{"suffix auto", "openai-response", "m(auto)", `{}`, "auto"},
		{"unknown suffix falls through to the body", "openai-response", "m(banana)", `{"reasoning":{"effort":"low"}}`, "low"},
		{"a bare parenthesis is not a suffix", "openai-response", "m(high", `{"reasoning":{"effort":"low"}}`, "low"},

		// An unfamiliar source format still tries each shape.
		{"unknown format, claude body", "something-else", "m", `{"thinking":{"type":"enabled","budget_tokens":10000}}`, "high"},
		{"unknown format, nothing to find", "something-else", "m", `{"model":"m"}`, ""},

		// Never guesses from a body it cannot read.
		{"broken json", "claude", "m", `{"thinking":`, ""},
		{"empty body", "claude", "m", ``, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := requestThinkingLevel([]byte(tc.body), tc.format, tc.model); got != tc.want {
				t.Fatalf("got %q want %q", got, tc.want)
			}
		})
	}
}

// The thresholds are the host's, so a budget lands in the same level the host
// would report for it.
func TestBudgetLandsInTheHostsLevels(t *testing.T) {
	cases := []struct {
		budget int
		want   string
	}{
		{-2, ""}, {-1, "auto"}, {0, "none"},
		{1, "minimal"}, {512, "minimal"},
		{513, "low"}, {1024, "low"},
		{1025, "medium"}, {8192, "medium"},
		{8193, "high"}, {24576, "high"},
		{24577, "xhigh"}, {200000, "xhigh"},
	}
	for _, tc := range cases {
		if got := budgetConfig(tc.budget).label(); got != tc.want {
			t.Fatalf("budget %d -> %q want %q", tc.budget, got, tc.want)
		}
	}
}
