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
	Claude ClaudeConfig
	Codex  CodexConfig
	Gemini GeminiConfig
	Logger *logrus.Logger
	// SkipGeminiLiveQuota disables the live OAuth quota fetch — useful in
	// tests or environments without network access.
	SkipGeminiLiveQuota bool
	// CCSaverPath overrides the CCSAVER DB path used for token totals (and
	// is propagated to the claude/gemini rate-limit lookups). Empty falls
	// back to DefaultCCSaverPath().
	CCSaverPath string
	// SkipCCSaver disables the token-totals query entirely — useful in tests
	// where the DB doesn't exist and we only exercise the rate-limit paths.
	SkipCCSaver bool
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

// GetAllAgentUsage returns a unified per-agent usage slice. Token counts come
// from the CCSAVER interactions table (summed per api_type over the window);
// the workstation proxy records every agent's traffic and ccsaver-mirror
// replicates a 7-day slice to the dashboard host, so this works the same in a
// local dev shell and in the pi5 dashboard container. Rate-limit/utilization
// data still comes from the per-agent readers (claude headers, codex headers,
// gemini live OAuth quota), which run concurrently.
//
// Agent → api_type token mapping:
//
//	claude   -> anthropic
//	codex    -> openai, openai-codex
//	gemini   -> gemini, gemini-code-assist
//	opencode -> vllm
func GetAllAgentUsage(ctx context.Context, days int, cfg AggregateConfig) ([]AgentUsage, error) {
	if days <= 0 {
		days = 1
	}
	logger := cfg.logger()

	// Propagate the CCSAVER path override to the rate-limit lookups so every
	// reader points at the same DB.
	if cfg.CCSaverPath != "" {
		cfg.Claude.CCSaverPath = cfg.CCSaverPath
		cfg.Gemini.CCSaverPath = cfg.CCSaverPath
	}

	// Token totals from CCSAVER — the authoritative source. On any failure we
	// fall through with an empty map (zero tokens + a warning) rather than
	// reviving the per-CLI session-file scan; a DB-only environment is the
	// expected deployment.
	tokenTotals := map[string]AgentTokenTotals{}
	if !cfg.SkipCCSaver {
		if cs, err := OpenCCSaver(CCSaverConfig{Path: cfg.CCSaverPath, Logger: logger}); err == nil {
			tt, qerr := cs.GetTokenTotalsByAPIType(days)
			cs.Close()
			if qerr != nil {
				logger.WithError(qerr).Warn("ccsaver: token totals query failed; agent tokens will be zero")
			} else {
				tokenTotals = tt
			}
		} else {
			logger.WithError(err).Warn("ccsaver: db unreadable; agent tokens will be zero")
		}
	}

	var (
		wg          sync.WaitGroup
		claudeRes   *ClaudeUsage
		codexRes    *CodexUsage
		geminiQuota *GeminiQuotaInfo
	)

	wg.Add(3)
	go func() {
		defer wg.Done()
		// Token output ignored; consumed only for rate-limit headers.
		r, err := GetClaudeUsage(days, cfg.Claude)
		if err != nil {
			logger.WithError(err).Error("claude usage error")
			return
		}
		claudeRes = r
	}()
	go func() {
		defer wg.Done()
		// Token output ignored; consumed only for rate-limit headers.
		r, err := GetCodexUsage(days, cfg.Codex)
		if err != nil {
			logger.WithError(err).Error("codex usage error")
			return
		}
		codexRes = r
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

	// claude — emitted unconditionally so a DB-only host still shows tokens.
	claudeTok := sumTokenTotals(tokenTotals, "anthropic")
	claudeUsage := AgentUsage{
		Agent:        "claude",
		APIType:      "anthropic",
		Sessions:     claudeTok.Calls,
		InputTokens:  claudeTok.InputTokens,
		OutputTokens: claudeTok.OutputTokens,
		Model:        firstNonEmpty(claudeTok.LatestModel, "claude-opus-4.7"),
	}
	if claudeRes != nil {
		claudeUsage.Utilization5h = claudeRes.Utilization5h
		claudeUsage.Utilization7d = claudeRes.Utilization7d
		claudeUsage.ResetTime5h = claudeRes.ResetTime5h
		claudeUsage.ResetTime7d = claudeRes.ResetTime7d
	}
	summaries = append(summaries, claudeUsage)

	// codex — sums both openai + openai-codex; JSON label stays "openai".
	codexTok := sumTokenTotals(tokenTotals, "openai", "openai-codex")
	codexModel := codexTok.LatestModel
	if codexRes != nil {
		codexModel = firstNonEmpty(codexModel, codexRes.Model)
	}
	codexUsage := AgentUsage{
		Agent:        "codex",
		APIType:      "openai",
		Sessions:     codexTok.Calls,
		InputTokens:  codexTok.InputTokens,
		OutputTokens: codexTok.OutputTokens,
		Model:        firstNonEmpty(codexModel, "gpt-5.3-codex"),
	}
	if codexRes != nil {
		codexUsage.Utilization5h = codexRes.Utilization5h
		codexUsage.Utilization7d = codexRes.Utilization7d
		codexUsage.ResetTime5h = codexRes.ResetTime5h
		codexUsage.ResetTime7d = codexRes.ResetTime7d
	}
	summaries = append(summaries, codexUsage)

	// gemini — tokens from ccsaver; daily utilization from the live OAuth
	// quota (or the on-disk cache), preserving the existing behavior.
	geminiTok := sumTokenTotals(tokenTotals, "gemini", "gemini-code-assist")
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
	summaries = append(summaries, AgentUsage{
		Agent:         "gemini",
		APIType:       "gemini",
		Sessions:      geminiTok.Calls,
		InputTokens:   geminiTok.InputTokens,
		OutputTokens:  geminiTok.OutputTokens,
		Model:         firstNonEmpty(joinModels(geminiTok.Models), "gemini-3-flash-preview"),
		Utilization5h: utilDaily,
		ResetTime5h:   resetDaily,
	})

	// opencode — runs on the local vLLM backend, captured as api_type "vllm".
	opencodeTok := sumTokenTotals(tokenTotals, "vllm")
	summaries = append(summaries, AgentUsage{
		Agent:        "opencode",
		APIType:      "local",
		Sessions:     opencodeTok.Calls,
		InputTokens:  opencodeTok.InputTokens,
		OutputTokens: opencodeTok.OutputTokens,
		Model:        firstNonEmpty(opencodeTok.LatestModel, "local"),
	})

	return summaries, nil
}

// sumTokenTotals merges the per-api_type rollups for the given api_types into
// one AgentTokenTotals: summed tokens/calls, a deduped+sorted model union, and
// the LatestModel from whichever contributing api_type logged the most calls.
func sumTokenTotals(byType map[string]AgentTokenTotals, apiTypes ...string) AgentTokenTotals {
	var out AgentTokenTotals
	modelSet := make(map[string]struct{})
	var bestCalls int64 = -1
	for _, at := range apiTypes {
		t, ok := byType[at]
		if !ok {
			continue
		}
		out.InputTokens += t.InputTokens
		out.OutputTokens += t.OutputTokens
		out.Calls += t.Calls
		for _, m := range t.Models {
			if m != "" {
				modelSet[m] = struct{}{}
			}
		}
		if t.LatestModel != "" && t.Calls > bestCalls {
			bestCalls = t.Calls
			out.LatestModel = t.LatestModel
		}
	}
	for m := range modelSet {
		out.Models = append(out.Models, m)
	}
	sort.Strings(out.Models)
	return out
}

// firstNonEmpty returns the first non-empty string argument, or "".
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
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
