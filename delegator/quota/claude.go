package quota

import (
	"bufio"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
)

// ClaudeConfig allows tests to override the home dir and CCSAVER DB path.
// Production callers use DefaultClaudeConfig().
type ClaudeConfig struct {
	// HomeDir overrides the user's home directory. Empty falls back to
	// os.UserHomeDir(), which itself respects $HOME on Unix.
	HomeDir string
	// CCSaverPath is forwarded to OpenCCSaver. Empty means the default path.
	CCSaverPath string
	// SkipCCSaver disables the CCSAVER lookup entirely — useful in tests
	// where the DB doesn't exist and we don't care about rate-limit headers.
	SkipCCSaver bool
	// Logger receives non-fatal warnings.
	Logger *logrus.Logger
}

// DefaultClaudeConfig returns a config that resolves all paths relative to
// the current $HOME (or os.UserHomeDir).
func DefaultClaudeConfig() ClaudeConfig {
	return ClaudeConfig{}
}

func (c ClaudeConfig) homeDir() (string, error) {
	if c.HomeDir != "" {
		return c.HomeDir, nil
	}
	return os.UserHomeDir()
}

func (c ClaudeConfig) logger() *logrus.Logger {
	if c.Logger != nil {
		return c.Logger
	}
	return logrus.StandardLogger()
}

// claudeStatsCache mirrors the bits of ~/.claude/stats-cache.json that the TS
// reads. Only DailyActivity entries are consumed.
type claudeStatsCache struct {
	DailyActivity []claudeDailyActivity `json:"dailyActivity"`
}

type claudeDailyActivity struct {
	Date          string `json:"date"`
	SessionCount  int64  `json:"sessionCount"`
	MessageCount  int64  `json:"messageCount"`
	ToolCallCount int64  `json:"toolCallCount"`
}

// claudeJSONLLine is the loose decoded shape of one line in a Claude session
// JSONL file. Only message.usage is consumed.
type claudeJSONLLine struct {
	Message *claudeMessage `json:"message"`
}

type claudeMessage struct {
	Usage *claudeUsageBlock `json:"usage"`
}

type claudeUsageBlock struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
}

// listClaudeSessionFiles returns JSONL session files modified within the
// last `days` days. Mirrors listClaudeSessionFiles in the TS.
func listClaudeSessionFiles(homeDir string, days int) ([]string, error) {
	projectsDir := filepath.Join(homeDir, ".claude", "projects")
	cutoff := time.Now().Add(-time.Duration(days) * 24 * time.Hour)

	entries, err := os.ReadDir(projectsDir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}

	var files []string
	for _, projEntry := range entries {
		projDir := filepath.Join(projectsDir, projEntry.Name())
		projEntries, err := os.ReadDir(projDir)
		if err != nil {
			continue // skip unreadable project dirs
		}
		for _, e := range projEntries {
			if !strings.HasSuffix(e.Name(), ".jsonl") {
				continue
			}
			fp := filepath.Join(projDir, e.Name())
			info, err := os.Stat(fp)
			if err != nil {
				continue
			}
			if info.ModTime().Before(cutoff) {
				continue
			}
			files = append(files, fp)
		}
	}
	return files, nil
}

// GetClaudeUsage scans Claude's session JSONL files plus stats-cache.json,
// then augments with rate-limit data from CCSAVER. Mirrors getClaudeUsage.
func GetClaudeUsage(days int, cfg ClaudeConfig) (*ClaudeUsage, error) {
	if days <= 0 {
		days = 1
	}
	home, err := cfg.homeDir()
	if err != nil {
		return nil, err
	}
	logger := cfg.logger()
	result := &ClaudeUsage{}

	// Pull daily activity from stats-cache.json (best-effort).
	if cache, err := readClaudeStatsCache(home); err == nil && cache != nil {
		// Match TS: cutoff is a date string YYYY-MM-DD computed from now-days.
		cutoff := time.Now().AddDate(0, 0, -days).Format("2006-01-02")
		for _, day := range cache.DailyActivity {
			if day.Date >= cutoff {
				result.Sessions += day.SessionCount
				result.MessageCount += day.MessageCount
				result.ToolCallCount += day.ToolCallCount
			}
		}
	}

	// Scan session JSONL files for token usage.
	files, err := listClaudeSessionFiles(home, days)
	if err != nil {
		logger.WithError(err).Warn("claude: failed to list session files")
	}
	if result.Sessions == 0 {
		result.Sessions = int64(len(files))
	}

	for _, file := range files {
		if err := scanClaudeSessionFile(file, result); err != nil {
			logger.WithFields(logrus.Fields{
				"file":  file,
				"error": err.Error(),
			}).Warn("claude: failed to read session")
		}
	}

	// Pull rate-limit headers from CCSAVER if available.
	if !cfg.SkipCCSaver {
		ccsCfg := CCSaverConfig{Path: cfg.CCSaverPath, Logger: logger}
		if cs, err := OpenCCSaver(ccsCfg); err == nil {
			rl := cs.GetClaudeRateLimits()
			cs.Close()
			if rl != nil {
				result.Utilization5h = ptrFloat(rl.Utilization5h)
				result.Utilization7d = ptrFloat(rl.Utilization7d)
				result.ResetTime5h = rl.ResetTime5h
				result.ResetTime7d = rl.ResetTime7d
			}
		} else if !errors.Is(err, fs.ErrNotExist) {
			// Missing DB is expected on fresh installs — only warn on real errors.
			logger.WithError(err).Debug("claude: ccsaver not readable")
		}
	}

	return result, nil
}

func readClaudeStatsCache(homeDir string) (*claudeStatsCache, error) {
	path := filepath.Join(homeDir, ".claude", "stats-cache.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cache claudeStatsCache
	if err := json.Unmarshal(data, &cache); err != nil {
		return nil, err
	}
	return &cache, nil
}

// scanClaudeSessionFile parses one JSONL file and accumulates token counts
// into result. Malformed lines are skipped silently — matches TS behavior.
func scanClaudeSessionFile(path string, result *ClaudeUsage) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	// Allow up to 16 MiB per line so very long tool outputs don't trip
	// bufio's 64KB default.
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var entry claudeJSONLLine
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			continue // skip malformed
		}
		if entry.Message == nil || entry.Message.Usage == nil {
			continue
		}
		u := entry.Message.Usage
		result.TotalInputTokens += u.InputTokens
		result.TotalOutputTokens += u.OutputTokens
		result.TotalCacheCreationTokens += u.CacheCreationInputTokens
		result.TotalCacheReadTokens += u.CacheReadInputTokens
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return nil
}
