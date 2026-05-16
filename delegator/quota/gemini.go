package quota

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
)

// Gemini OAuth constants — borrowed from gemini-cli-core's oauth2.js. They
// are public client identifiers; baking them in matches the TS source.
const (
	geminiClientID     = "681255809395-oo8ft2oprdrnp9e3aqf6av3hmdib135j.apps.googleusercontent.com"
	geminiClientSecret = "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"
	geminiQuotaURL     = "https://cloudcode-pa.googleapis.com/v1internal:retrieveUserQuota"
	geminiTokenURL     = "https://oauth2.googleapis.com/token"
)

// GeminiConfig overrides paths and HTTP transport for tests.
type GeminiConfig struct {
	HomeDir     string
	CCSaverPath string
	SkipCCSaver bool
	// HTTPClient is used for OAuth token refresh + quota API calls. Tests
	// inject httptest.Server-backed clients here.
	HTTPClient *http.Client
	// QuotaURL overrides geminiQuotaURL (test injection).
	QuotaURL string
	// TokenURL overrides geminiTokenURL (test injection).
	TokenURL string
	Logger   *logrus.Logger
}

func (c GeminiConfig) homeDir() (string, error) {
	if c.HomeDir != "" {
		return c.HomeDir, nil
	}
	return os.UserHomeDir()
}

func (c GeminiConfig) logger() *logrus.Logger {
	if c.Logger != nil {
		return c.Logger
	}
	return logrus.StandardLogger()
}

func (c GeminiConfig) httpClient() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	return &http.Client{Timeout: 15 * time.Second}
}

func (c GeminiConfig) quotaURL() string {
	if c.QuotaURL != "" {
		return c.QuotaURL
	}
	return geminiQuotaURL
}

func (c GeminiConfig) tokenURL() string {
	if c.TokenURL != "" {
		return c.TokenURL
	}
	return geminiTokenURL
}

// geminiSessionMessage is the loose decoded shape of one entry — either a
// JSONL line or a member of `messages` in the legacy single-doc format.
type geminiSessionMessage struct {
	Tokens *geminiTokens `json:"tokens,omitempty"`
	Model  string        `json:"model,omitempty"`
}

type geminiTokens struct {
	Input    int64 `json:"input"`
	Output   int64 `json:"output"`
	Thoughts int64 `json:"thoughts"`
	Cached   int64 `json:"cached"`
}

// geminiLegacySession matches the older single-doc format with a `messages`
// array.
type geminiLegacySession struct {
	Messages []geminiSessionMessage `json:"messages"`
}

func listGeminiSessionFiles(homeDir string, days int) ([]string, error) {
	geminiDir := filepath.Join(homeDir, ".gemini", "tmp")
	cutoff := time.Now().Add(-time.Duration(days) * 24 * time.Hour)

	hashes, err := os.ReadDir(geminiDir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}

	var files []string
	for _, h := range hashes {
		chatsDir := filepath.Join(geminiDir, h.Name(), "chats")
		entries, err := os.ReadDir(chatsDir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			name := e.Name()
			isSession := strings.HasPrefix(name, "session-") &&
				(strings.HasSuffix(name, ".jsonl") || strings.HasSuffix(name, ".json"))
			if !isSession {
				continue
			}
			fp := filepath.Join(chatsDir, name)
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

func accumulateGeminiMessage(msg *geminiSessionMessage, result *GeminiUsage, modelSet map[string]struct{}) {
	if msg.Tokens != nil {
		result.TotalInputTokens += msg.Tokens.Input
		result.TotalOutputTokens += msg.Tokens.Output
		result.TotalThoughtTokens += msg.Tokens.Thoughts
		result.TotalCachedTokens += msg.Tokens.Cached
	}
	if msg.Model != "" {
		modelSet[msg.Model] = struct{}{}
	}
}

// GetGeminiUsage scans gemini-cli session files (both JSONL and legacy JSON
// formats) and augments with CCSAVER totals. Mirrors getGeminiUsage.
func GetGeminiUsage(days int, cfg GeminiConfig) (*GeminiUsage, error) {
	if days <= 0 {
		days = 1
	}
	home, err := cfg.homeDir()
	if err != nil {
		return nil, err
	}
	logger := cfg.logger()

	result := &GeminiUsage{Models: []string{}}
	modelSet := make(map[string]struct{})

	files, err := listGeminiSessionFiles(home, days)
	if err != nil {
		logger.WithError(err).Warn("gemini: session listing failed")
	}
	result.Sessions = int64(len(files))

	for _, file := range files {
		if err := scanGeminiSessionFile(file, result, modelSet); err != nil {
			logger.WithFields(logrus.Fields{
				"file":  file,
				"error": err.Error(),
			}).Warn("gemini: failed to read session")
		}
	}

	// Augment with CCSAVER. Per the TS comment, session files and CCSAVER
	// overlap for proxied gemini-cli, so take the per-metric maximum.
	if !cfg.SkipCCSaver {
		ccsCfg := CCSaverConfig{Path: cfg.CCSaverPath, Logger: logger}
		if cs, err := OpenCCSaver(ccsCfg); err == nil {
			ccsaver := cs.GetGeminiUsage(days)
			cs.Close()
			if ccsaver != nil {
				if ccsaver.Input > result.TotalInputTokens {
					result.TotalInputTokens = ccsaver.Input
				}
				if ccsaver.Output > result.TotalOutputTokens {
					result.TotalOutputTokens = ccsaver.Output
				}
				if result.Sessions == 0 {
					result.Sessions = ccsaver.Calls
				}
				for _, m := range ccsaver.Models {
					modelSet[m] = struct{}{}
				}
			}
		} else if !errors.Is(err, fs.ErrNotExist) {
			logger.WithError(err).Debug("gemini: ccsaver not readable")
		}
	}

	for m := range modelSet {
		result.Models = append(result.Models, m)
	}
	return result, nil
}

func scanGeminiSessionFile(path string, result *GeminiUsage, modelSet map[string]struct{}) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	if strings.HasSuffix(path, ".jsonl") {
		scanner := bufio.NewScanner(f)
		scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" {
				continue
			}
			var msg geminiSessionMessage
			if err := json.Unmarshal([]byte(line), &msg); err != nil {
				continue
			}
			// Match TS guard: only count entries that have a tokens object.
			if msg.Tokens == nil {
				continue
			}
			accumulateGeminiMessage(&msg, result, modelSet)
		}
		return scanner.Err()
	}

	// Legacy single-doc JSON.
	data, err := io.ReadAll(f)
	if err != nil {
		return err
	}
	var session geminiLegacySession
	if err := json.Unmarshal(data, &session); err != nil {
		return err
	}
	for i := range session.Messages {
		accumulateGeminiMessage(&session.Messages[i], result, modelSet)
	}
	return nil
}

// --- Gemini OAuth Quota API ---

type geminiOAuthCreds struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token,omitempty"`
	// ExpiryDate is in milliseconds since epoch (TS Date.now() convention).
	ExpiryDate *int64 `json:"expiry_date,omitempty"`
}

type geminiSettings struct {
	AuthType string `json:"authType"`
}

type geminiQuotaBucket struct {
	QuotaInfo *struct {
		RemainingFraction *float64 `json:"remainingFraction,omitempty"`
		ResetTime         string   `json:"resetTime,omitempty"`
		ModelID           string   `json:"modelId,omitempty"`
	} `json:"quotaInfo,omitempty"`
	ModelID           string   `json:"modelId,omitempty"`
	RemainingFraction *float64 `json:"remainingFraction,omitempty"`
	ResetTime         string   `json:"resetTime,omitempty"`
}

type geminiQuotaResponse struct {
	Quotas    []geminiQuotaBucket `json:"quotas,omitempty"`
	UserQuota []geminiQuotaBucket `json:"userQuota,omitempty"`
}

// GetGeminiLiveQuota fetches the live user quota from Cloud Code's OAuth API.
// Returns nil + non-nil error on hard failures; nil + nil for "auth type
// unsupported" or "creds not present" — matching the TS undefined return.
func GetGeminiLiveQuota(ctx context.Context, cfg GeminiConfig) (*GeminiQuotaInfo, error) {
	logger := cfg.logger()
	home, err := cfg.homeDir()
	if err != nil {
		return nil, err
	}

	// Auth type guard — non-OAuth deployments cannot hit this endpoint.
	settingsPath := filepath.Join(home, ".gemini", "settings.json")
	if data, err := os.ReadFile(settingsPath); err == nil {
		var s geminiSettings
		if err := json.Unmarshal(data, &s); err == nil {
			if s.AuthType == "api-key" || s.AuthType == "vertex-ai" {
				logger.WithField("authType", s.AuthType).Info("gemini: auth type not supported for quota API")
				return nil, nil
			}
		}
	}

	credsPath := filepath.Join(home, ".gemini", "oauth_creds.json")
	credsRaw, err := os.ReadFile(credsPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read gemini creds: %w", err)
	}
	var creds geminiOAuthCreds
	if err := json.Unmarshal(credsRaw, &creds); err != nil {
		return nil, fmt.Errorf("parse gemini creds: %w", err)
	}

	token := creds.AccessToken
	// Refresh if we know the token is expired.
	if creds.ExpiryDate != nil && *creds.ExpiryDate < time.Now().UnixMilli() {
		refreshed, rerr := refreshGeminiToken(ctx, &creds, credsPath, cfg)
		if rerr != nil {
			logger.WithError(rerr).Warn("gemini: token refresh failed")
			return nil, nil
		}
		if refreshed == "" {
			logger.Warn("gemini: token expired and refresh returned empty")
			return nil, nil
		}
		token = refreshed
	}

	// First quota call.
	data, status, err := geminiQuotaCall(ctx, cfg, token)
	if err != nil {
		return nil, err
	}
	if status == http.StatusUnauthorized {
		// Refresh once and retry.
		refreshed, rerr := refreshGeminiToken(ctx, &creds, credsPath, cfg)
		if rerr != nil || refreshed == "" {
			return nil, nil
		}
		data, status, err = geminiQuotaCall(ctx, cfg, refreshed)
		if err != nil {
			return nil, err
		}
		if status >= 400 {
			return nil, nil
		}
	}
	if status >= 400 {
		logger.WithField("status", status).Warn("gemini: quota API failed")
		return nil, nil
	}
	return parseGeminiQuota(data), nil
}

// geminiQuotaCall posts an empty body to the quota URL with a Bearer token,
// returning the parsed response, the HTTP status, and any transport error.
func geminiQuotaCall(ctx context.Context, cfg GeminiConfig, token string) (*geminiQuotaResponse, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.quotaURL(), bytes.NewReader([]byte("{}")))
	if err != nil {
		return nil, 0, fmt.Errorf("build quota request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := cfg.httpClient().Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("quota fetch: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 4*1024*1024))
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("read quota body: %w", err)
	}
	if resp.StatusCode >= 400 {
		return nil, resp.StatusCode, nil
	}
	var qr geminiQuotaResponse
	if err := json.Unmarshal(body, &qr); err != nil {
		return nil, resp.StatusCode, fmt.Errorf("parse quota body: %w", err)
	}
	return &qr, resp.StatusCode, nil
}

// refreshGeminiToken posts a refresh_token grant to the OAuth token endpoint
// and rewrites the creds file in place. Returns the new access token.
// On failure returns ("", err) — callers map err==nil + ""=="" to "no creds
// available" and bail out.
func refreshGeminiToken(ctx context.Context, creds *geminiOAuthCreds, credsPath string, cfg GeminiConfig) (string, error) {
	if creds.RefreshToken == "" {
		return "", nil
	}
	form := url.Values{}
	form.Set("client_id", geminiClientID)
	form.Set("client_secret", geminiClientSecret)
	form.Set("refresh_token", creds.RefreshToken)
	form.Set("grant_type", "refresh_token")

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.tokenURL(), strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("build token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := cfg.httpClient().Do(req)
	if err != nil {
		return "", fmt.Errorf("token refresh: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1*1024*1024))
	if err != nil {
		return "", fmt.Errorf("read token body: %w", err)
	}
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("token refresh status %d: %s", resp.StatusCode, string(body))
	}

	var tr struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &tr); err != nil {
		return "", fmt.Errorf("parse token body: %w", err)
	}
	if tr.AccessToken == "" {
		return "", nil
	}

	creds.AccessToken = tr.AccessToken
	if tr.ExpiresIn > 0 {
		exp := time.Now().UnixMilli() + tr.ExpiresIn*1000
		creds.ExpiryDate = &exp
	}
	// Best-effort: rewrite creds file with the new token. JSON marshal
	// errors are ignored to mirror the TS try/catch.
	if updated, err := json.MarshalIndent(creds, "", "  "); err == nil {
		_ = os.WriteFile(credsPath, updated, 0o600)
	}
	return tr.AccessToken, nil
}

// parseGeminiQuota walks the quota buckets and computes the lowest remaining
// fraction with its corresponding reset time.
func parseGeminiQuota(data *geminiQuotaResponse) *GeminiQuotaInfo {
	if data == nil {
		return nil
	}
	buckets := data.Quotas
	if len(buckets) == 0 {
		buckets = data.UserQuota
	}

	models := make([]GeminiQuotaModel, 0, len(buckets))
	lowestRemaining := 1.0
	earliestReset := ""

	for _, b := range buckets {
		modelID := ""
		var remaining float64 = 1.0
		reset := ""

		if b.QuotaInfo != nil {
			modelID = b.QuotaInfo.ModelID
			if b.QuotaInfo.RemainingFraction != nil {
				remaining = *b.QuotaInfo.RemainingFraction
			}
			reset = b.QuotaInfo.ResetTime
		}
		if modelID == "" {
			modelID = b.ModelID
		}
		if modelID == "" {
			modelID = "unknown"
		}
		// Outer fields override inner only when inner was absent — TS
		// uses ?? (nullish coalescing) which keeps inner's 0 over outer's
		// undefined. We approximate that by preferring inner if set above.
		if b.QuotaInfo == nil && b.RemainingFraction != nil {
			remaining = *b.RemainingFraction
		}
		if reset == "" {
			reset = b.ResetTime
		}

		models = append(models, GeminiQuotaModel{
			ModelID:           modelID,
			RemainingFraction: remaining,
			ResetTime:         reset,
		})
		if remaining < lowestRemaining {
			lowestRemaining = remaining
			earliestReset = reset
		}
	}

	return &GeminiQuotaInfo{
		Models:          models,
		LowestRemaining: lowestRemaining,
		ResetTime:       earliestReset,
	}
}

// --- Self-reported rate limit cache ---
//
// Mirrors the persistent JSON cache from the TS source. The cache lives at
// ~/.cache/delegator/rate-limits.json so the TS dashboard and Go consumers
// can share it. We only implement the Gemini side here per scope.

type rateLimitCache struct {
	Gemini *struct {
		UtilizationDaily *float64                            `json:"utilizationDaily,omitempty"`
		ResetTimeDaily   string                              `json:"resetTimeDaily,omitempty"`
		Models           map[string]GeminiRateLimitModelInfo `json:"models,omitempty"`
		UpdatedAt        int64                               `json:"updatedAt"`
	} `json:"gemini,omitempty"`
}

const staleMs = 3_600_000 // 1 hour, matches TS STALE_MS

var rateLimitMu sync.Mutex

func rateLimitsCachePath(homeDir string) string {
	return filepath.Join(homeDir, ".cache", "delegator", "rate-limits.json")
}

func loadRateLimitCache(homeDir string) rateLimitCache {
	rateLimitMu.Lock()
	defer rateLimitMu.Unlock()
	path := rateLimitsCachePath(homeDir)
	data, err := os.ReadFile(path)
	if err != nil {
		return rateLimitCache{}
	}
	var c rateLimitCache
	if err := json.Unmarshal(data, &c); err != nil {
		return rateLimitCache{}
	}
	return c
}

func saveRateLimitCache(homeDir string, c rateLimitCache) error {
	rateLimitMu.Lock()
	defer rateLimitMu.Unlock()
	dir := filepath.Join(homeDir, ".cache", "delegator")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(rateLimitsCachePath(homeDir), data, 0o600)
}

// ReportGeminiRateLimits persists a manual rate-limits report from the
// gemini-cli wrapper. Mirrors the TS reportGeminiRateLimits.
func ReportGeminiRateLimits(report GeminiRateLimitsReport, cfg GeminiConfig) error {
	home, err := cfg.homeDir()
	if err != nil {
		return err
	}
	c := loadRateLimitCache(home)
	if c.Gemini == nil {
		c.Gemini = &struct {
			UtilizationDaily *float64                            `json:"utilizationDaily,omitempty"`
			ResetTimeDaily   string                              `json:"resetTimeDaily,omitempty"`
			Models           map[string]GeminiRateLimitModelInfo `json:"models,omitempty"`
			UpdatedAt        int64                               `json:"updatedAt"`
		}{}
	}
	c.Gemini.UtilizationDaily = report.UtilizationDaily
	c.Gemini.ResetTimeDaily = report.ResetTimeDaily
	c.Gemini.Models = report.Models
	c.Gemini.UpdatedAt = time.Now().UnixMilli()
	cfg.logger().WithFields(logrus.Fields{
		"utilizationDaily": report.UtilizationDaily,
		"resetTimeDaily":   report.ResetTimeDaily,
	}).Info("gemini rate limits updated")
	return saveRateLimitCache(home, c)
}

// getCachedGeminiRateLimits returns the cached self-reported daily
// rate-limit data if present and fresher than staleMs.
func getCachedGeminiRateLimits(homeDir string) *struct {
	UtilizationDaily *float64
	ResetTimeDaily   string
} {
	c := loadRateLimitCache(homeDir)
	if c.Gemini == nil {
		return nil
	}
	if time.Now().UnixMilli()-c.Gemini.UpdatedAt > staleMs {
		return nil
	}
	return &struct {
		UtilizationDaily *float64
		ResetTimeDaily   string
	}{
		UtilizationDaily: c.Gemini.UtilizationDaily,
		ResetTimeDaily:   c.Gemini.ResetTimeDaily,
	}
}
