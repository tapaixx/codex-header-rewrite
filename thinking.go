package main

import (
	"encoding/json"
	"strconv"
	"strings"
)

// How hard a request asked the model to think is said in five different ways,
// and the request hook sees whichever one the client used: this plugin reads
// the body before CPA translates it, so the shape is the client's protocol,
// not Codex's.
//
// Rather than guess at that, this is a port of CPA's own
// thinking.ExtractReasoningEffort, which is the same answer the host records
// for usage. It keeps the host's priority -- a model-name suffix outranks the
// body -- and the host's vocabulary, so a level shown here is the level the
// host would report. A token budget is mapped onto that vocabulary instead of
// being shown raw, which is what makes two clients comparable at a glance.
//
// The thresholds and level names come from internal/thinking/convert.go and
// internal/thinking/suffix.go.
const (
	thinkingBudgetMinimal = 512
	thinkingBudgetLow     = 1024
	thinkingBudgetMedium  = 8192
	thinkingBudgetHigh    = 24576
)

// thinkingLevels is the host's vocabulary. A suffix is only a level if it is
// one of these; anything else falls through to being read as a budget.
var thinkingLevels = map[string]struct{}{
	"minimal": {}, "low": {}, "medium": {}, "high": {}, "xhigh": {}, "max": {},
}

// thinkingConfig mirrors CPA's ThinkingConfig closely enough to carry the same
// distinctions: a level, a budget, off, or automatic.
type thinkingConfig struct {
	level  string
	budget int
	// set distinguishes "the request said nothing" from "the request said
	// none", which are different answers and must not collapse.
	set bool
}

func levelConfig(level string) thinkingConfig {
	level = strings.ToLower(strings.TrimSpace(level))
	if level == "" {
		return thinkingConfig{}
	}
	switch level {
	case "none":
		return thinkingConfig{level: "none", set: true}
	case "auto":
		return thinkingConfig{level: "auto", set: true}
	}
	return thinkingConfig{level: level, set: true}
}

func budgetConfig(budget int) thinkingConfig {
	switch {
	case budget == 0:
		return thinkingConfig{level: "none", set: true}
	case budget == -1:
		return thinkingConfig{level: "auto", set: true}
	case budget < -1:
		return thinkingConfig{}
	}
	return thinkingConfig{budget: budget, set: true}
}

// label renders a config in the host's vocabulary. A budget is converted to
// the level it falls in, so "10000 tokens" and "high" read the same way.
func (c thinkingConfig) label() string {
	if !c.set {
		return ""
	}
	if c.level != "" {
		return c.level
	}
	switch {
	case c.budget <= 0:
		return ""
	case c.budget <= thinkingBudgetMinimal:
		return "minimal"
	case c.budget <= thinkingBudgetLow:
		return "low"
	case c.budget <= thinkingBudgetMedium:
		return "medium"
	case c.budget <= thinkingBudgetHigh:
		return "high"
	default:
		return "xhigh"
	}
}

// requestThinkingLevel reports how hard a request asked the model to think, in
// the host's vocabulary. sourceFormat is the client's protocol, which decides
// where in the body to look; model may still carry a thinking suffix, because
// the executors strip that after this hook has run.
func requestThinkingLevel(body []byte, sourceFormat, model string) string {
	if config := thinkingFromSuffix(model); config.set {
		return config.label()
	}
	return thinkingFromBody(body, sourceFormat).label()
}

// thinkingFromSuffix reads model(value). The host gives a valid suffix
// priority over the body, so this is checked first.
func thinkingFromSuffix(model string) thinkingConfig {
	model = strings.TrimSpace(model)
	open := strings.LastIndex(model, "(")
	if open <= 0 || !strings.HasSuffix(model, ")") {
		return thinkingConfig{}
	}
	raw := strings.TrimSpace(model[open+1 : len(model)-1])
	switch strings.ToLower(raw) {
	case "none":
		return thinkingConfig{level: "none", set: true}
	case "auto":
		return thinkingConfig{level: "auto", set: true}
	}
	if _, ok := thinkingLevels[strings.ToLower(raw)]; ok {
		return levelConfig(raw)
	}
	if budget, err := strconv.Atoi(raw); err == nil {
		return budgetConfig(budget)
	}
	return thinkingConfig{}
}

func thinkingFromBody(body []byte, sourceFormat string) thinkingConfig {
	if len(body) == 0 {
		return thinkingConfig{}
	}
	switch strings.ToLower(strings.TrimSpace(sourceFormat)) {
	case "claude":
		return thinkingFromClaude(body)
	case "gemini", "antigravity":
		return thinkingFromGemini(body)
	case "openai":
		// Chat completions names it at the top level; if it is absent the body
		// may still be a Responses payload, which the host also falls back on.
		if config := thinkingFromOpenAI(body); config.set {
			return config
		}
		return thinkingFromCodex(body)
	case "codex", "xai", "openai-response":
		return thinkingFromCodex(body)
	default:
		// An unfamiliar format is worth one guess at each shape rather than
		// nothing: the fields are distinctive enough not to collide.
		for _, read := range []func([]byte) thinkingConfig{thinkingFromCodex, thinkingFromClaude, thinkingFromOpenAI, thinkingFromGemini} {
			if config := read(body); config.set {
				return config
			}
		}
		return thinkingConfig{}
	}
}

// thinkingFromCodex reads the Responses shape. The latest configuration_update
// in the input array wins over the top-level setting, which is how a Codex
// session changes effort mid-conversation.
func thinkingFromCodex(body []byte) thinkingConfig {
	var payload struct {
		Reasoning struct {
			Effort *string `json:"effort"`
		} `json:"reasoning"`
		Input []struct {
			Type      string `json:"type"`
			Reasoning struct {
				Effort *string `json:"effort"`
			} `json:"reasoning"`
		} `json:"input"`
	}
	if json.Unmarshal(body, &payload) != nil {
		return thinkingConfig{}
	}
	latest := ""
	for _, item := range payload.Input {
		if item.Type != "configuration_update" || item.Reasoning.Effort == nil {
			continue
		}
		if value := strings.ToLower(strings.TrimSpace(*item.Reasoning.Effort)); value != "" {
			latest = value
		}
	}
	if latest != "" {
		return levelConfig(latest)
	}
	if payload.Reasoning.Effort != nil {
		return levelConfig(*payload.Reasoning.Effort)
	}
	return thinkingConfig{}
}

// thinkingFromClaude reads the Messages shape. disabled outranks a budget, and
// adaptive thinking says its level under output_config instead.
func thinkingFromClaude(body []byte) thinkingConfig {
	var payload struct {
		Thinking struct {
			Type         string `json:"type"`
			BudgetTokens *int   `json:"budget_tokens"`
		} `json:"thinking"`
		OutputConfig struct {
			Effort *string `json:"effort"`
		} `json:"output_config"`
	}
	if json.Unmarshal(body, &payload) != nil {
		return thinkingConfig{}
	}
	switch strings.ToLower(strings.TrimSpace(payload.Thinking.Type)) {
	case "disabled":
		return thinkingConfig{level: "none", set: true}
	case "adaptive", "auto":
		if payload.OutputConfig.Effort != nil {
			return levelConfig(*payload.OutputConfig.Effort)
		}
		// Without an explicit effort the upstream default applies, and a
		// default is not something this request asked for.
		return thinkingConfig{}
	}
	if payload.Thinking.BudgetTokens != nil {
		return budgetConfig(*payload.Thinking.BudgetTokens)
	}
	if strings.EqualFold(strings.TrimSpace(payload.Thinking.Type), "enabled") {
		return thinkingConfig{level: "auto", set: true}
	}
	return thinkingConfig{}
}

func thinkingFromOpenAI(body []byte) thinkingConfig {
	var payload struct {
		ReasoningEffort *string `json:"reasoning_effort"`
	}
	if json.Unmarshal(body, &payload) != nil || payload.ReasoningEffort == nil {
		return thinkingConfig{}
	}
	return levelConfig(*payload.ReasoningEffort)
}

// thinkingFromGemini reads generationConfig.thinkingConfig. The level is the
// Gemini 3 form and wins; the budget is the 2.5 form. Both are also sent in
// snake_case by Google's own Python SDK.
func thinkingFromGemini(body []byte) thinkingConfig {
	type thinkingBlock struct {
		ThinkingLevel     *string `json:"thinkingLevel"`
		ThinkingLevelAlt  *string `json:"thinking_level"`
		ThinkingBudget    *int    `json:"thinkingBudget"`
		ThinkingBudgetAlt *int    `json:"thinking_budget"`
	}
	var payload struct {
		GenerationConfig struct {
			ThinkingConfig thinkingBlock `json:"thinkingConfig"`
		} `json:"generationConfig"`
		Request struct {
			GenerationConfig struct {
				ThinkingConfig thinkingBlock `json:"thinkingConfig"`
			} `json:"generationConfig"`
		} `json:"request"`
	}
	if json.Unmarshal(body, &payload) != nil {
		return thinkingConfig{}
	}
	for _, block := range []thinkingBlock{payload.GenerationConfig.ThinkingConfig, payload.Request.GenerationConfig.ThinkingConfig} {
		level := block.ThinkingLevel
		if level == nil {
			level = block.ThinkingLevelAlt
		}
		if level != nil {
			return levelConfig(*level)
		}
		budget := block.ThinkingBudget
		if budget == nil {
			budget = block.ThinkingBudgetAlt
		}
		if budget != nil {
			return budgetConfig(*budget)
		}
	}
	return thinkingConfig{}
}
