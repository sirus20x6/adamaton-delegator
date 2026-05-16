package quota

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeJSONLLine emits one Claude session line. Helper kept tiny so each test
// fixture is obvious from its caller.
func writeJSONLLine(t *testing.T, w *os.File, msg map[string]any) {
	t.Helper()
	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if _, err := w.Write(append(data, '\n')); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func setupClaudeFixture(t *testing.T, lines []map[string]any) string {
	t.Helper()
	home := t.TempDir()
	projDir := filepath.Join(home, ".claude", "projects", "proj1")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	sessionFile := filepath.Join(projDir, "session.jsonl")
	f, err := os.Create(sessionFile)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	for _, line := range lines {
		writeJSONLLine(t, f, line)
	}
	f.Close()
	// Touch mtime to "now" so it's within the days window.
	now := time.Now()
	if err := os.Chtimes(sessionFile, now, now); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	return home
}

func TestGetClaudeUsage_HappyPath(t *testing.T) {
	home := setupClaudeFixture(t, []map[string]any{
		{
			"message": map[string]any{
				"usage": map[string]any{
					"input_tokens":                100,
					"output_tokens":               50,
					"cache_creation_input_tokens": 25,
					"cache_read_input_tokens":     10,
				},
			},
		},
		{
			"message": map[string]any{
				"usage": map[string]any{
					"input_tokens":  200,
					"output_tokens": 75,
				},
			},
		},
	})

	cfg := ClaudeConfig{HomeDir: home, SkipCCSaver: true}
	usage, err := GetClaudeUsage(1, cfg)
	if err != nil {
		t.Fatalf("GetClaudeUsage: %v", err)
	}
	if usage.TotalInputTokens != 300 {
		t.Errorf("TotalInputTokens = %d, want 300", usage.TotalInputTokens)
	}
	if usage.TotalOutputTokens != 125 {
		t.Errorf("TotalOutputTokens = %d, want 125", usage.TotalOutputTokens)
	}
	if usage.TotalCacheCreationTokens != 25 {
		t.Errorf("TotalCacheCreationTokens = %d, want 25", usage.TotalCacheCreationTokens)
	}
	if usage.TotalCacheReadTokens != 10 {
		t.Errorf("TotalCacheReadTokens = %d, want 10", usage.TotalCacheReadTokens)
	}
	if usage.Sessions != 1 {
		t.Errorf("Sessions = %d, want 1", usage.Sessions)
	}
}

func TestGetClaudeUsage_EmptyDir(t *testing.T) {
	home := t.TempDir()
	cfg := ClaudeConfig{HomeDir: home, SkipCCSaver: true}
	usage, err := GetClaudeUsage(1, cfg)
	if err != nil {
		t.Fatalf("GetClaudeUsage: %v", err)
	}
	if usage.Sessions != 0 {
		t.Errorf("Sessions = %d, want 0", usage.Sessions)
	}
	if usage.TotalInputTokens != 0 {
		t.Errorf("TotalInputTokens = %d, want 0", usage.TotalInputTokens)
	}
}

func TestGetClaudeUsage_MalformedLines(t *testing.T) {
	home := t.TempDir()
	projDir := filepath.Join(home, ".claude", "projects", "proj1")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	sessionFile := filepath.Join(projDir, "mixed.jsonl")
	content := `{"this":"is not valid for our schema but is valid json"}` + "\n" +
		`not valid json at all` + "\n" +
		`{"message":{"usage":{"input_tokens":42,"output_tokens":17}}}` + "\n" +
		`` + "\n" +
		`{"message":{"usage":{"input_tokens":1,"output_tokens":2}}}` + "\n"
	if err := os.WriteFile(sessionFile, []byte(content), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	now := time.Now()
	os.Chtimes(sessionFile, now, now)

	cfg := ClaudeConfig{HomeDir: home, SkipCCSaver: true}
	usage, err := GetClaudeUsage(1, cfg)
	if err != nil {
		t.Fatalf("GetClaudeUsage: %v", err)
	}
	if usage.TotalInputTokens != 43 {
		t.Errorf("TotalInputTokens = %d, want 43 (skipped malformed)", usage.TotalInputTokens)
	}
	if usage.TotalOutputTokens != 19 {
		t.Errorf("TotalOutputTokens = %d, want 19", usage.TotalOutputTokens)
	}
}

func TestGetClaudeUsage_StatsCacheDailyActivity(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	today := time.Now().Format("2006-01-02")
	cache := map[string]any{
		"dailyActivity": []map[string]any{
			{"date": today, "sessionCount": 5, "messageCount": 100, "toolCallCount": 30},
		},
	}
	data, _ := json.Marshal(cache)
	if err := os.WriteFile(filepath.Join(home, ".claude", "stats-cache.json"), data, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	cfg := ClaudeConfig{HomeDir: home, SkipCCSaver: true}
	usage, err := GetClaudeUsage(1, cfg)
	if err != nil {
		t.Fatalf("GetClaudeUsage: %v", err)
	}
	if usage.Sessions != 5 {
		t.Errorf("Sessions = %d, want 5 (from stats cache)", usage.Sessions)
	}
	if usage.MessageCount != 100 {
		t.Errorf("MessageCount = %d, want 100", usage.MessageCount)
	}
	if usage.ToolCallCount != 30 {
		t.Errorf("ToolCallCount = %d, want 30", usage.ToolCallCount)
	}
}
