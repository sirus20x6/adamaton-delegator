package quota

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestGetGeminiUsage_HappyPath_JSONL(t *testing.T) {
	home := t.TempDir()
	chatsDir := filepath.Join(home, ".gemini", "tmp", "abcdef", "chats")
	if err := os.MkdirAll(chatsDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	path := filepath.Join(chatsDir, "session-001.jsonl")

	lines := []map[string]any{
		// header / control entry — no tokens, must be skipped.
		{"type": "header", "id": "abc"},
		{
			"tokens": map[string]any{
				"input":    100,
				"output":   50,
				"thoughts": 10,
				"cached":   5,
			},
			"model": "gemini-2.5-pro",
		},
		{
			"tokens": map[string]any{
				"input":  200,
				"output": 80,
			},
			"model": "gemini-2.5-pro",
		},
	}
	f, _ := os.Create(path)
	for _, l := range lines {
		data, _ := json.Marshal(l)
		f.Write(append(data, '\n'))
	}
	f.Close()
	now := time.Now()
	os.Chtimes(path, now, now)

	cfg := GeminiConfig{HomeDir: home, SkipCCSaver: true}
	usage, err := GetGeminiUsage(1, cfg)
	if err != nil {
		t.Fatalf("GetGeminiUsage: %v", err)
	}
	if usage.TotalInputTokens != 300 {
		t.Errorf("TotalInputTokens = %d, want 300", usage.TotalInputTokens)
	}
	if usage.TotalOutputTokens != 130 {
		t.Errorf("TotalOutputTokens = %d, want 130", usage.TotalOutputTokens)
	}
	if usage.TotalThoughtTokens != 10 {
		t.Errorf("TotalThoughtTokens = %d, want 10", usage.TotalThoughtTokens)
	}
	if usage.TotalCachedTokens != 5 {
		t.Errorf("TotalCachedTokens = %d, want 5", usage.TotalCachedTokens)
	}
	if usage.Sessions != 1 {
		t.Errorf("Sessions = %d, want 1", usage.Sessions)
	}
	if len(usage.Models) != 1 || usage.Models[0] != "gemini-2.5-pro" {
		t.Errorf("Models = %v, want [gemini-2.5-pro]", usage.Models)
	}
}

func TestGetGeminiUsage_LegacyJSON(t *testing.T) {
	home := t.TempDir()
	chatsDir := filepath.Join(home, ".gemini", "tmp", "abcdef", "chats")
	if err := os.MkdirAll(chatsDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	path := filepath.Join(chatsDir, "session-legacy.json")

	doc := map[string]any{
		"messages": []map[string]any{
			{"tokens": map[string]any{"input": 11, "output": 22}, "model": "gemini-flash"},
			{"tokens": map[string]any{"input": 3, "output": 4}},
		},
	}
	data, _ := json.MarshalIndent(doc, "", "  ")
	os.WriteFile(path, data, 0o644)
	now := time.Now()
	os.Chtimes(path, now, now)

	cfg := GeminiConfig{HomeDir: home, SkipCCSaver: true}
	usage, err := GetGeminiUsage(1, cfg)
	if err != nil {
		t.Fatalf("GetGeminiUsage: %v", err)
	}
	if usage.TotalInputTokens != 14 {
		t.Errorf("TotalInputTokens = %d, want 14", usage.TotalInputTokens)
	}
	if usage.TotalOutputTokens != 26 {
		t.Errorf("TotalOutputTokens = %d, want 26", usage.TotalOutputTokens)
	}
}

func TestGetGeminiUsage_EmptyDir(t *testing.T) {
	home := t.TempDir()
	cfg := GeminiConfig{HomeDir: home, SkipCCSaver: true}
	usage, err := GetGeminiUsage(1, cfg)
	if err != nil {
		t.Fatalf("GetGeminiUsage: %v", err)
	}
	if usage.Sessions != 0 {
		t.Errorf("Sessions = %d, want 0", usage.Sessions)
	}
	if len(usage.Models) != 0 {
		t.Errorf("Models = %v, want empty", usage.Models)
	}
}

func TestGetGeminiUsage_MalformedLines(t *testing.T) {
	home := t.TempDir()
	chatsDir := filepath.Join(home, ".gemini", "tmp", "h1", "chats")
	os.MkdirAll(chatsDir, 0o755)
	path := filepath.Join(chatsDir, "session-bad.jsonl")
	content := `garbage` + "\n" +
		`{"$set":{"foo":"bar"}}` + "\n" + // valid JSON, no tokens — skipped
		`{"tokens":{"input":7,"output":3}}` + "\n"
	os.WriteFile(path, []byte(content), 0o644)
	now := time.Now()
	os.Chtimes(path, now, now)

	cfg := GeminiConfig{HomeDir: home, SkipCCSaver: true}
	usage, err := GetGeminiUsage(1, cfg)
	if err != nil {
		t.Fatalf("GetGeminiUsage: %v", err)
	}
	if usage.TotalInputTokens != 7 {
		t.Errorf("TotalInputTokens = %d, want 7", usage.TotalInputTokens)
	}
}

func TestReportGeminiRateLimits_Roundtrip(t *testing.T) {
	home := t.TempDir()
	util := 0.42
	report := GeminiRateLimitsReport{
		UtilizationDaily: &util,
		ResetTimeDaily:   "2026-05-09T00:00:00Z",
	}
	cfg := GeminiConfig{HomeDir: home}
	if err := ReportGeminiRateLimits(report, cfg); err != nil {
		t.Fatalf("ReportGeminiRateLimits: %v", err)
	}
	cached := getCachedGeminiRateLimits(home)
	if cached == nil {
		t.Fatalf("cached nil after report")
	}
	if cached.UtilizationDaily == nil || *cached.UtilizationDaily != 0.42 {
		t.Errorf("UtilizationDaily = %v, want 0.42", cached.UtilizationDaily)
	}
	if cached.ResetTimeDaily != "2026-05-09T00:00:00Z" {
		t.Errorf("ResetTimeDaily = %q, want set", cached.ResetTimeDaily)
	}

	// On-disk file exists with 0600 perms.
	path := rateLimitsCachePath(home)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("perm = %v, want 0600", info.Mode().Perm())
	}
}

func TestGetGeminiLiveQuota_OAuthSuccess(t *testing.T) {
	home := t.TempDir()
	geminiDir := filepath.Join(home, ".gemini")
	os.MkdirAll(geminiDir, 0o755)

	// Creds file with already-fresh token (no refresh needed).
	exp := time.Now().Add(1 * time.Hour).UnixMilli()
	creds := map[string]any{
		"access_token":  "fresh-token",
		"refresh_token": "rtok",
		"expiry_date":   exp,
	}
	cdata, _ := json.Marshal(creds)
	os.WriteFile(filepath.Join(geminiDir, "oauth_creds.json"), cdata, 0o600)

	// Mock quota server that asserts the bearer token and returns one bucket.
	var seen string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Get("Authorization")
		resp := map[string]any{
			"quotas": []map[string]any{
				{
					"quotaInfo": map[string]any{
						"modelId":           "gemini-2.5-pro",
						"remainingFraction": 0.25,
						"resetTime":         "2026-05-08T23:00:00Z",
					},
				},
			},
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	cfg := GeminiConfig{
		HomeDir:    home,
		QuotaURL:   srv.URL,
		HTTPClient: srv.Client(),
	}
	q, err := GetGeminiLiveQuota(context.Background(), cfg)
	if err != nil {
		t.Fatalf("GetGeminiLiveQuota: %v", err)
	}
	if q == nil {
		t.Fatalf("nil quota")
	}
	if !strings.Contains(seen, "fresh-token") {
		t.Errorf("server did not see expected bearer token: %q", seen)
	}
	if len(q.Models) != 1 || q.Models[0].ModelID != "gemini-2.5-pro" {
		t.Errorf("Models = %v, want one gemini-2.5-pro", q.Models)
	}
	if q.LowestRemaining != 0.25 {
		t.Errorf("LowestRemaining = %v, want 0.25", q.LowestRemaining)
	}
}

func TestGetGeminiLiveQuota_RefreshOnExpired(t *testing.T) {
	home := t.TempDir()
	geminiDir := filepath.Join(home, ".gemini")
	os.MkdirAll(geminiDir, 0o755)

	// Expired token — should trigger refresh BEFORE quota call.
	exp := time.Now().Add(-1 * time.Hour).UnixMilli()
	creds := map[string]any{
		"access_token":  "stale",
		"refresh_token": "rtok",
		"expiry_date":   exp,
	}
	cdata, _ := json.Marshal(creds)
	os.WriteFile(filepath.Join(geminiDir, "oauth_creds.json"), cdata, 0o600)

	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Form.Get("refresh_token") != "rtok" {
			http.Error(w, "bad refresh", http.StatusBadRequest)
			return
		}
		resp := map[string]any{
			"access_token": "refreshed",
			"expires_in":   3600,
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer tokenSrv.Close()

	quotaSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer refreshed" {
			http.Error(w, "wrong token", http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"userQuota": []map[string]any{
				{"modelId": "gemini-x", "remainingFraction": 0.9},
			},
		})
	}))
	defer quotaSrv.Close()

	cfg := GeminiConfig{
		HomeDir:    home,
		TokenURL:   tokenSrv.URL,
		QuotaURL:   quotaSrv.URL,
		HTTPClient: &http.Client{Timeout: 5 * time.Second},
	}
	q, err := GetGeminiLiveQuota(context.Background(), cfg)
	if err != nil {
		t.Fatalf("GetGeminiLiveQuota: %v", err)
	}
	if q == nil {
		t.Fatalf("nil quota")
	}
	if q.LowestRemaining != 0.9 {
		t.Errorf("LowestRemaining = %v, want 0.9", q.LowestRemaining)
	}

	// Creds file should have been rewritten with the fresh token.
	updated, _ := os.ReadFile(filepath.Join(geminiDir, "oauth_creds.json"))
	if !strings.Contains(string(updated), "refreshed") {
		t.Errorf("creds file not rewritten: %s", updated)
	}
}

func TestGetGeminiLiveQuota_AuthTypeUnsupported(t *testing.T) {
	home := t.TempDir()
	geminiDir := filepath.Join(home, ".gemini")
	os.MkdirAll(geminiDir, 0o755)
	settings := map[string]any{"authType": "api-key"}
	sdata, _ := json.Marshal(settings)
	os.WriteFile(filepath.Join(geminiDir, "settings.json"), sdata, 0o644)

	cfg := GeminiConfig{HomeDir: home}
	q, err := GetGeminiLiveQuota(context.Background(), cfg)
	if err != nil {
		t.Fatalf("expected nil err, got %v", err)
	}
	if q != nil {
		t.Errorf("expected nil quota for api-key auth, got %+v", q)
	}
}
