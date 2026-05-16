package quota

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
)

// AggregateConfig optionally overrides the per-agent configs used by the
// fan-out. Defaults to zero-valued configs (which respect $HOME).
type AggregateConfig struct {
	Claude  ClaudeConfig
	Codex   CodexConfig
	Gemini  GeminiConfig
	Logger  *logrus.Logger
	// SkipGeminiLiveQuota disables the live OAuth quota fetch — useful in
	// tests or environments without network access.
	SkipGeminiLiveQuota bool
}

func (c AggregateConfig) logger() *logrus.Logger {
	if c.Logger != nil {
		return c.Logger
	}
	if c.Claude.Logger != nil {
		return c.Claude.Logger
	}
	if c.Codex.Logger != nil {
		return c.Codex.Logger
	}
	if c.Gemini.Logger != nil {
		return c.Gemini.Logger
	}
	return logrus.StandardLogger()
}

// GetAllAgentUsage fans out to each per-agent reader concurrently and returns
// a unified slice. Errors from individual readers are logged and that agent
// is omitted, matching the TS Promise.all+catch pattern.
func GetAllAgentUsage(ctx context.Context, days int, cfg AggregateConfig) ([]AgentUsage, error) {
	if days <= 0 {
		days = 1
	}
	logger := cfg.logger()

	var (
		wg          sync.WaitGroup
		claudeRes   *ClaudeUsage
		codexRes    *CodexUsage
		geminiRes   *GeminiUsage
		geminiQuota *GeminiQuotaInfo
	)

	wg.Add(4)
	go func() {
		defer wg.Done()
		r, err := GetClaudeUsage(days, cfg.Claude)
		if err != nil {
			logger.WithError(err).Error("claude usage error")
			return
		}
		claudeRes = r
	}()
	go func() {
		defer wg.Done()
		r, err := GetCodexUsage(days, cfg.Codex)
		if err != nil {
			logger.WithError(err).Error("codex usage error")
			return
		}
		codexRes = r
	}()
	go func() {
		defer wg.Done()
		r, err := GetGeminiUsage(days, cfg.Gemini)
		if err != nil {
			logger.WithError(err).Error("gemini usage error")
			return
		}
		geminiRes = r
	}()
	go func() {
		defer wg.Done()
		if cfg.SkipGeminiLiveQuota {
			return
		}
		q, err := GetGeminiLiveQuota(ctx, cfg.Gemini)
		if err != nil {
			logger.WithError(err).Error("gemini quota API error")
			return
		}
		geminiQuota = q
	}()
	wg.Wait()

	var summaries []AgentUsage

	if claudeRes != nil {
		summaries = append(summaries, AgentUsage{
			Agent:         "claude",
			APIType:       "anthropic",
			Sessions:      claudeRes.Sessions,
			InputTokens:   claudeRes.TotalInputTokens + claudeRes.TotalCacheCreationTokens + claudeRes.TotalCacheReadTokens,
			OutputTokens:  claudeRes.TotalOutputTokens,
			Model:         "claude-opus-4.7",
			Utilization5h: claudeRes.Utilization5h,
			Utilization7d: claudeRes.Utilization7d,
			ResetTime5h:   claudeRes.ResetTime5h,
			ResetTime7d:   claudeRes.ResetTime7d,
		})
	}

	if codexRes != nil {
		model := codexRes.Model
		if model == "" {
			model = "gpt-5.3-codex"
		}
		summaries = append(summaries, AgentUsage{
			Agent:         "codex",
			APIType:       "openai",
			Sessions:      codexRes.Sessions,
			InputTokens:   codexRes.TotalInputTokens,
			OutputTokens:  codexRes.TotalOutputTokens,
			Model:         model,
			Utilization5h: codexRes.Utilization5h,
			Utilization7d: codexRes.Utilization7d,
			ResetTime5h:   codexRes.ResetTime5h,
			ResetTime7d:   codexRes.ResetTime7d,
		})
	}

	if geminiRes != nil {
		var utilDaily *float64
		var resetDaily string

		switch {
		case geminiQuota != nil:
			u := 1 - geminiQuota.LowestRemaining
			utilDaily = &u
			resetDaily = geminiQuota.ResetTime
			// Persist live quota into the cache for next time.
			if home, err := cfg.Gemini.homeDir(); err == nil {
				_ = saveRateLimitCache(home, rateLimitCache{
					Gemini: &struct {
						UtilizationDaily *float64                            `json:"utilizationDaily,omitempty"`
						ResetTimeDaily   string                              `json:"resetTimeDaily,omitempty"`
						Models           map[string]GeminiRateLimitModelInfo `json:"models,omitempty"`
						UpdatedAt        int64                               `json:"updatedAt"`
					}{
						UtilizationDaily: utilDaily,
						ResetTimeDaily:   resetDaily,
						UpdatedAt:        nowUnixMilli(),
					},
				})
			}
		default:
			if home, err := cfg.Gemini.homeDir(); err == nil {
				if cached := getCachedGeminiRateLimits(home); cached != nil {
					utilDaily = cached.UtilizationDaily
					resetDaily = cached.ResetTimeDaily
				}
			}
		}

		// Sort models for stable output — modelSet iteration order is random.
		sort.Strings(geminiRes.Models)
		modelStr := joinModels(geminiRes.Models)
		if modelStr == "" {
			modelStr = "gemini-3-flash-preview"
		}

		summaries = append(summaries, AgentUsage{
			Agent:         "gemini",
			APIType:       "gemini",
			Sessions:      geminiRes.Sessions,
			InputTokens:   geminiRes.TotalInputTokens + geminiRes.TotalCachedTokens,
			OutputTokens:  geminiRes.TotalOutputTokens,
			Model:         modelStr,
			Utilization5h: utilDaily,
			ResetTime5h:   resetDaily,
		})
	}

	// OpenCode is always present.
	if oc, err := GetOpenCodeUsage(days); err == nil && oc != nil {
		summaries = append(summaries, AgentUsage{
			Agent:        "opencode",
			APIType:      "local",
			Sessions:     oc.Sessions,
			InputTokens:  oc.InputTokens,
			OutputTokens: oc.OutputTokens,
			Model:        oc.Model,
		})
	}

	return summaries, nil
}

// joinModels concatenates models with ", " separator (matches TS .join).
func joinModels(models []string) string {
	out := ""
	for i, m := range models {
		if i > 0 {
			out += ", "
		}
		out += m
	}
	return out
}

// nowUnixMilli is a small helper that exists to stay close to the TS
// Date.now() semantics used in the cache.
func nowUnixMilli() int64 {
	return time.Now().UnixMilli()
}
