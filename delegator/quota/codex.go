package quota

import (
	"bufio"
	"encoding/json"
	"errors"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
)

// CodexConfig overrides the home dir for codex session discovery.
type CodexConfig struct {
	HomeDir string
	Logger  *logrus.Logger
}

func (c CodexConfig) homeDir() (string, error) {
	if c.HomeDir != "" {
		return c.HomeDir, nil
	}
	return os.UserHomeDir()
}

func (c CodexConfig) logger() *logrus.Logger {
	if c.Logger != nil {
		return c.Logger
	}
	return logrus.StandardLogger()
}

// codexJSONLEntry decodes one line of a codex session file. Only `type`,
// `payload`, and `timestamp` are consumed — everything else is dropped.
type codexJSONLEntry struct {
	Type      string          `json:"type"`
	Timestamp string          `json:"timestamp"`
	Payload   json.RawMessage `json:"payload"`
}

// codexTurnContextPayload is the payload for `type: "turn_context"` lines.
type codexTurnContextPayload struct {
	Model string `json:"model"`
}

// codexTokenCountPayload is the payload for `type: "event_msg"` lines whose
// inner type is "token_count".
type codexTokenCountPayload struct {
	Type       string                  `json:"type"`
	Info       *codexTokenCountInfo    `json:"info,omitempty"`
	RateLimits *codexRateLimitsPayload `json:"rate_limits,omitempty"`
}

type codexTokenCountInfo struct {
	LastTokenUsage *codexLastTokenUsage `json:"last_token_usage,omitempty"`
}

type codexLastTokenUsage struct {
	InputTokens           int64 `json:"input_tokens"`
	OutputTokens          int64 `json:"output_tokens"`
	ReasoningOutputTokens int64 `json:"reasoning_output_tokens"`
}

// codexRateLimitsPayload mirrors the TS CodexRateLimits shape.
type codexRateLimitsPayload struct {
	Primary   *codexRateLimitBucket `json:"primary,omitempty"`
	Secondary *codexRateLimitBucket `json:"secondary,omitempty"`
}

// codexRateLimitBucket — used_percent is required; resets_at OR
// resets_in_seconds may be present (codex emits one or the other depending on
// version). Pointers distinguish "missing" from "zero".
type codexRateLimitBucket struct {
	UsedPercent     float64  `json:"used_percent"`
	WindowMinutes   int64    `json:"window_minutes"`
	ResetsAt        *float64 `json:"resets_at,omitempty"`
	ResetsInSeconds *float64 `json:"resets_in_seconds,omitempty"`
}

// listCodexSessionFiles walks ~/.codex/sessions/{Y}/{M}/{D} for the last
// `days` days. Mirrors listCodexSessionFiles.
func listCodexSessionFiles(homeDir string, days int) ([]string, error) {
	sessionsDir := filepath.Join(homeDir, ".codex", "sessions")
	now := time.Now()

	var files []string
	for d := 0; d < days; d++ {
		date := now.AddDate(0, 0, -d)
		y := date.Format("2006")
		m := date.Format("01")
		day := date.Format("02")
		dayDir := filepath.Join(sessionsDir, y, m, day)
		entries, err := os.ReadDir(dayDir)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			// Only log structural errors; missing day-dirs are normal.
			return files, err
		}
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), ".jsonl") {
				files = append(files, filepath.Join(dayDir, e.Name()))
			}
		}
	}
	return files, nil
}

// resolveCodexResetTime converts a bucket's resets_at/resets_in_seconds field
// to an ISO timestamp string. Returns "" if neither field is parseable.
func resolveCodexResetTime(b *codexRateLimitBucket) string {
	if b == nil {
		return ""
	}
	if b.ResetsAt != nil && !math.IsInf(*b.ResetsAt, 0) && !math.IsNaN(*b.ResetsAt) {
		return time.Unix(int64(*b.ResetsAt), 0).UTC().Format("2006-01-02T15:04:05.000Z")
	}
	if b.ResetsInSeconds != nil && !math.IsInf(*b.ResetsInSeconds, 0) && !math.IsNaN(*b.ResetsInSeconds) {
		return time.Now().Add(time.Duration(*b.ResetsInSeconds * float64(time.Second))).UTC().Format("2006-01-02T15:04:05.000Z")
	}
	return ""
}

// GetCodexUsage scans the last `days` of codex JSONL session files and
// returns aggregated usage + the freshest rate-limit bucket. Mirrors
// getCodexUsage from the TS source.
//
// NOTE: As of the CCSAVER token-source switch, GetAllAgentUsage consumes only
// this function's rate-limit fields and Model — token counts now come from the
// CCSAVER interactions table (see CCSaver.GetTokenTotalsByAPIType). The
// token-scanning here is retained for standalone callers and its tests.
func GetCodexUsage(days int, cfg CodexConfig) (*CodexUsage, error) {
	if days <= 0 {
		days = 1
	}
	home, err := cfg.homeDir()
	if err != nil {
		return nil, err
	}
	logger := cfg.logger()

	files, err := listCodexSessionFiles(home, days)
	if err != nil {
		logger.WithError(err).Warn("codex: session listing failed")
	}

	result := &CodexUsage{Sessions: int64(len(files))}

	var latestRateLimits *codexRateLimitsPayload
	// Track the most recent timestamp at which we saw a rate-limits or
	// turn-context entry. -Inf semantics in JS map to math.Inf(-1) here.
	latestRateLimitsTs := math.Inf(-1)
	latestModelTs := math.Inf(-1)

	for _, file := range files {
		if err := scanCodexSessionFile(file, result, &latestRateLimits, &latestRateLimitsTs, &latestModelTs); err != nil {
			logger.WithFields(logrus.Fields{
				"file":  file,
				"error": err.Error(),
			}).Warn("codex: failed to read session")
		}
	}

	if latestRateLimits != nil {
		if latestRateLimits.Primary != nil {
			result.Utilization5h = ptrFloat(latestRateLimits.Primary.UsedPercent / 100.0)
			result.ResetTime5h = resolveCodexResetTime(latestRateLimits.Primary)
		}
		if latestRateLimits.Secondary != nil {
			result.Utilization7d = ptrFloat(latestRateLimits.Secondary.UsedPercent / 100.0)
			result.ResetTime7d = resolveCodexResetTime(latestRateLimits.Secondary)
		}
	}

	return result, nil
}

func scanCodexSessionFile(path string, result *CodexUsage, latestRL **codexRateLimitsPayload, latestRLTs, latestModelTs *float64) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var entry codexJSONLEntry
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			continue
		}

		ts := math.NaN()
		if entry.Timestamp != "" {
			if t, err := time.Parse(time.RFC3339Nano, entry.Timestamp); err == nil {
				// Convert to milliseconds-since-epoch to match the TS Date.parse semantics.
				ts = float64(t.UnixMilli())
			} else if t, err := time.Parse(time.RFC3339, entry.Timestamp); err == nil {
				ts = float64(t.UnixMilli())
			}
		}

		switch entry.Type {
		case "turn_context":
			var p codexTurnContextPayload
			if err := json.Unmarshal(entry.Payload, &p); err != nil || p.Model == "" {
				continue
			}
			if !math.IsNaN(ts) && ts >= *latestModelTs {
				*latestModelTs = ts
				result.Model = p.Model
			} else if result.Model == "" {
				result.Model = p.Model
			}
		case "event_msg":
			var p codexTokenCountPayload
			if err := json.Unmarshal(entry.Payload, &p); err != nil {
				continue
			}
			if p.Type != "token_count" {
				continue
			}
			if p.Info != nil && p.Info.LastTokenUsage != nil {
				u := p.Info.LastTokenUsage
				result.TotalInputTokens += u.InputTokens
				result.TotalOutputTokens += u.OutputTokens
				result.TotalReasoningTokens += u.ReasoningOutputTokens
			}
			if p.RateLimits != nil {
				if !math.IsNaN(ts) && ts >= *latestRLTs {
					*latestRLTs = ts
					*latestRL = p.RateLimits
				} else if *latestRL == nil {
					*latestRL = p.RateLimits
				}
			}
		}
	}
	return scanner.Err()
}
