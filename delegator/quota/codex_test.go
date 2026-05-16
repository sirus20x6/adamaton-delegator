package quota

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// codexFixturePath returns the dated session-file path for `now`, creating
// dirs along the way.
func codexFixturePath(t *testing.T, home string, now time.Time, name string) string {
	t.Helper()
	dir := filepath.Join(home, ".codex", "sessions",
		now.Format("2006"), now.Format("01"), now.Format("02"))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	return filepath.Join(dir, name)
}

func TestGetCodexUsage_HappyPath(t *testing.T) {
	home := t.TempDir()
	now := time.Now()
	path := codexFixturePath(t, home, now, "session.jsonl")

	resetsAt := float64(now.Add(2 * time.Hour).Unix())
	lines := []map[string]any{
		{
			"type":      "turn_context",
			"timestamp": now.Add(-1 * time.Minute).UTC().Format(time.RFC3339Nano),
			"payload":   map[string]any{"model": "gpt-5-codex"},
		},
		{
			"type":      "event_msg",
			"timestamp": now.UTC().Format(time.RFC3339Nano),
			"payload": map[string]any{
				"type": "token_count",
				"info": map[string]any{
					"last_token_usage": map[string]any{
						"input_tokens":            500,
						"output_tokens":           200,
						"reasoning_output_tokens": 50,
					},
				},
				"rate_limits": map[string]any{
					"primary": map[string]any{
						"used_percent":   42.5,
						"window_minutes": 300,
						"resets_at":      resetsAt,
					},
					"secondary": map[string]any{
						"used_percent":   10.0,
						"window_minutes": 10080,
					},
				},
			},
		},
	}

	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	for _, l := range lines {
		data, _ := json.Marshal(l)
		f.Write(append(data, '\n'))
	}
	f.Close()

	cfg := CodexConfig{HomeDir: home}
	usage, err := GetCodexUsage(1, cfg)
	if err != nil {
		t.Fatalf("GetCodexUsage: %v", err)
	}
	if usage.Sessions != 1 {
		t.Errorf("Sessions = %d, want 1", usage.Sessions)
	}
	if usage.Model != "gpt-5-codex" {
		t.Errorf("Model = %q, want gpt-5-codex", usage.Model)
	}
	if usage.TotalInputTokens != 500 {
		t.Errorf("TotalInputTokens = %d, want 500", usage.TotalInputTokens)
	}
	if usage.TotalOutputTokens != 200 {
		t.Errorf("TotalOutputTokens = %d, want 200", usage.TotalOutputTokens)
	}
	if usage.TotalReasoningTokens != 50 {
		t.Errorf("TotalReasoningTokens = %d, want 50", usage.TotalReasoningTokens)
	}
	if usage.Utilization5h == nil || *usage.Utilization5h != 0.425 {
		got := "<nil>"
		if usage.Utilization5h != nil {
			got = formatFloat(*usage.Utilization5h)
		}
		t.Errorf("Utilization5h = %s, want 0.425", got)
	}
	if usage.ResetTime5h == "" {
		t.Errorf("ResetTime5h was empty")
	}
}

func TestGetCodexUsage_EmptyDir(t *testing.T) {
	home := t.TempDir()
	cfg := CodexConfig{HomeDir: home}
	usage, err := GetCodexUsage(1, cfg)
	if err != nil {
		t.Fatalf("GetCodexUsage: %v", err)
	}
	if usage.Sessions != 0 {
		t.Errorf("Sessions = %d, want 0", usage.Sessions)
	}
	if usage.Model != "" {
		t.Errorf("Model = %q, want empty", usage.Model)
	}
}

func TestGetCodexUsage_MalformedLines(t *testing.T) {
	home := t.TempDir()
	now := time.Now()
	path := codexFixturePath(t, home, now, "mixed.jsonl")

	content := `not valid json` + "\n" +
		`{"type":"turn_context","timestamp":"` + now.UTC().Format(time.RFC3339Nano) + `","payload":{"model":"gpt-5"}}` + "\n" +
		`{"type":"event_msg","timestamp":"` + now.UTC().Format(time.RFC3339Nano) + `","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":7,"output_tokens":3,"reasoning_output_tokens":1}}}}` + "\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	cfg := CodexConfig{HomeDir: home}
	usage, err := GetCodexUsage(1, cfg)
	if err != nil {
		t.Fatalf("GetCodexUsage: %v", err)
	}
	if usage.TotalInputTokens != 7 {
		t.Errorf("TotalInputTokens = %d, want 7 (skipped malformed)", usage.TotalInputTokens)
	}
	if usage.Model != "gpt-5" {
		t.Errorf("Model = %q, want gpt-5", usage.Model)
	}
}

func TestGetCodexUsage_ResetsInSeconds(t *testing.T) {
	home := t.TempDir()
	now := time.Now()
	path := codexFixturePath(t, home, now, "session.jsonl")

	line := map[string]any{
		"type":      "event_msg",
		"timestamp": now.UTC().Format(time.RFC3339Nano),
		"payload": map[string]any{
			"type": "token_count",
			"rate_limits": map[string]any{
				"primary": map[string]any{
					"used_percent":      80.0,
					"window_minutes":    300,
					"resets_in_seconds": 3600.0,
				},
			},
		},
	}
	f, _ := os.Create(path)
	data, _ := json.Marshal(line)
	f.Write(append(data, '\n'))
	f.Close()

	cfg := CodexConfig{HomeDir: home}
	usage, err := GetCodexUsage(1, cfg)
	if err != nil {
		t.Fatalf("GetCodexUsage: %v", err)
	}
	if usage.Utilization5h == nil || *usage.Utilization5h != 0.8 {
		t.Errorf("Utilization5h missing or wrong: %v", usage.Utilization5h)
	}
	if usage.ResetTime5h == "" {
		t.Errorf("ResetTime5h should be derived from resets_in_seconds")
	}
}

func formatFloat(f float64) string {
	return string(jsonNumber(f))
}

func jsonNumber(f float64) []byte {
	b, _ := json.Marshal(f)
	return b
}
