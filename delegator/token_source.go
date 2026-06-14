package delegator

import (
	"time"

	"github.com/sirus20x6/adamaton-delegator/delegator/budget"
)

// TokenReader supplies a delegation's REAL token usage over a time window,
// summed across the CCSAVER api_type(s) for a provider. quota.CCSaver
// satisfies it. A nil TokenReader on the Orchestrator disables real-token
// accounting — report() falls back to the coarse prompt+stdout estimate.
type TokenReader interface {
	SumTokensInWindow(apiTypes []string, start, end time.Time) (input, output int64, err error)
}

// apiTypesForProvider maps a budget.Provider to the CCSAVER api_type(s) its
// agent's traffic is recorded under. Mirrors the agent->api_type mapping in
// quota/aggregate.go: codex sums openai + openai-codex, gemini sums gemini +
// gemini-code-assist, opencode runs on the local vLLM backend ("vllm"),
// claude is the Anthropic proxy path. Unknown providers return nil →
// report() falls back to the estimate.
func apiTypesForProvider(p budget.Provider) []string {
	switch p {
	case budget.ProviderClaude:
		return []string{"anthropic"}
	case budget.ProviderOpenAI:
		return []string{"openai", "openai-codex"}
	case budget.ProviderGemini:
		return []string{"gemini", "gemini-code-assist"}
	case budget.ProviderLocal:
		return []string{"vllm"}
	}
	return nil
}
