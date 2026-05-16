package budget

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sirus20x6/adamomaton-core/pgutil"
)

func newTestServer(t *testing.T) *Server {
	t.Helper()
	if os.Getenv("GOGENTS_SKIP_DOCKER_TESTS") != "" {
		t.Skip("GOGENTS_SKIP_DOCKER_TESTS set")
	}
	logger := logrus.New()
	logger.SetLevel(logrus.WarnLevel)

	store, err := NewStore(pgutil.TestDSN(t), logger)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	config := &ServiceConfig{
		DailyResetHour: 0,
		WeeklyResetDay: 1,
		Timezone:       "UTC",
		Providers: []ProviderConfig{
			{
				Provider:      ProviderClaude,
				Tier:          TierCloud,
				Strength:      0.95,
				DailyLimit:    200000,
				WeeklyLimit:   1000000,
				DefaultModel:  "claude-sonnet",
				Models:        map[string]string{"critical": "claude-opus", "high": "claude-sonnet"},
				CostPerMToken: 3.00,
			},
			{
				Provider:      ProviderLocal,
				Tier:          TierLocal,
				Strength:      0.45,
				DailyLimit:    0,
				WeeklyLimit:   0,
				DefaultModel:  "qwen-coder",
				CostPerMToken: 0.0,
			},
		},
	}

	tracker, err := NewTracker(store, config, logger)
	require.NoError(t, err)
	t.Cleanup(func() { tracker.Stop() })

	providerConfigs := make(map[Provider]*ProviderConfig)
	for i := range config.Providers {
		pc := &config.Providers[i]
		providerConfigs[pc.Provider] = pc
	}

	router := NewRouter(tracker, providerConfigs, logger)
	return NewServer(tracker, router, logger)
}

func TestServer_Health(t *testing.T) {
	srv := newTestServer(t)

	req := httptest.NewRequest("GET", "/health", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	var resp APIResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.True(t, resp.Success)
}

func TestServer_Route(t *testing.T) {
	srv := newTestServer(t)

	body, _ := json.Marshal(RouteRequest{
		TaskComplexity:  ComplexityHigh,
		EstimatedTokens: 5000,
	})

	req := httptest.NewRequest("POST", "/api/v1/budget/route", bytes.NewReader(body))
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	var resp APIResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.True(t, resp.Success)
	assert.NotNil(t, resp.Data)
}

func TestServer_Route_InvalidComplexity(t *testing.T) {
	srv := newTestServer(t)

	body, _ := json.Marshal(map[string]interface{}{
		"task_complexity":  "extreme",
		"estimated_tokens": 5000,
	})

	req := httptest.NewRequest("POST", "/api/v1/budget/route", bytes.NewReader(body))
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestServer_Report(t *testing.T) {
	srv := newTestServer(t)

	body, _ := json.Marshal(ReportRequest{
		Provider:    ProviderClaude,
		Model:       "claude-sonnet",
		TotalTokens: 5000,
	})

	req := httptest.NewRequest("POST", "/api/v1/budget/report", bytes.NewReader(body))
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	var resp APIResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.True(t, resp.Success)
}

func TestServer_Report_InvalidProvider(t *testing.T) {
	srv := newTestServer(t)

	body, _ := json.Marshal(map[string]interface{}{
		"provider":     "unknown",
		"total_tokens": 5000,
	})

	req := httptest.NewRequest("POST", "/api/v1/budget/report", bytes.NewReader(body))
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestServer_Report_ZeroTokens(t *testing.T) {
	srv := newTestServer(t)

	body, _ := json.Marshal(ReportRequest{
		Provider:    ProviderClaude,
		TotalTokens: 0,
	})

	req := httptest.NewRequest("POST", "/api/v1/budget/report", bytes.NewReader(body))
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestServer_Status(t *testing.T) {
	srv := newTestServer(t)

	req := httptest.NewRequest("GET", "/api/v1/budget/status", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	var resp APIResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.True(t, resp.Success)
}

func TestServer_ProviderStatus(t *testing.T) {
	srv := newTestServer(t)

	req := httptest.NewRequest("GET", "/api/v1/budget/status/claude", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
}

func TestServer_ProviderStatus_Invalid(t *testing.T) {
	srv := newTestServer(t)

	req := httptest.NewRequest("GET", "/api/v1/budget/status/invalid", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestServer_History(t *testing.T) {
	srv := newTestServer(t)

	req := httptest.NewRequest("GET", "/api/v1/budget/history", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	var resp APIResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.True(t, resp.Success)
}

func TestServer_Reset(t *testing.T) {
	srv := newTestServer(t)

	// First report some usage
	body, _ := json.Marshal(ReportRequest{
		Provider:    ProviderClaude,
		TotalTokens: 50000,
	})
	req := httptest.NewRequest("POST", "/api/v1/budget/report", bytes.NewReader(body))
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)

	// Reset
	req = httptest.NewRequest("POST", "/api/v1/budget/reset/claude", nil)
	w = httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)

	// Verify status is reset
	req = httptest.NewRequest("GET", "/api/v1/budget/status/claude", nil)
	w = httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)

	var resp APIResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.True(t, resp.Success)
}

func TestServer_DisallowsUnknownFields(t *testing.T) {
	srv := newTestServer(t)

	body, _ := json.Marshal(map[string]interface{}{
		"task_complexity":  "high",
		"estimated_tokens": 1000,
		"unknown_field":    "should-be-rejected",
	})
	req := httptest.NewRequest("POST", "/api/v1/budget/route", bytes.NewReader(body))
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestServer_HistoryRejectsBadSince(t *testing.T) {
	srv := newTestServer(t)

	req := httptest.NewRequest("GET", "/api/v1/budget/history?since=not-a-timestamp", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestServer_HistoryRejectsBadLimit(t *testing.T) {
	srv := newTestServer(t)

	req := httptest.NewRequest("GET", "/api/v1/budget/history?limit=banana", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)
	assert.Equal(t, http.StatusBadRequest, w.Code)

	req = httptest.NewRequest("GET", "/api/v1/budget/history?limit=99999", nil)
	w = httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestServer_HistoryRejectsBadProvider(t *testing.T) {
	srv := newTestServer(t)

	req := httptest.NewRequest("GET", "/api/v1/budget/history?provider=banana", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestServer_FullCycle(t *testing.T) {
	srv := newTestServer(t)

	// 1. Route a high-complexity task
	routeBody, _ := json.Marshal(RouteRequest{
		TaskComplexity:  ComplexityHigh,
		EstimatedTokens: 5000,
	})
	req := httptest.NewRequest("POST", "/api/v1/budget/route", bytes.NewReader(routeBody))
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)

	var routeResp APIResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &routeResp))
	assert.True(t, routeResp.Success)

	// Extract provider from response
	routeData, ok := routeResp.Data.(map[string]interface{})
	require.True(t, ok)
	provider := routeData["provider"].(string)

	// 2. Report usage
	boolTrue := true
	reportBody, _ := json.Marshal(ReportRequest{
		Provider:         Provider(provider),
		Model:            "test-model",
		TotalTokens:      4200,
		PromptTokens:     2000,
		CompletionTokens: 2200,
		Success:          &boolTrue,
	})
	req = httptest.NewRequest("POST", "/api/v1/budget/report", bytes.NewReader(reportBody))
	w = httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)

	// 3. Check status reflects the usage
	req = httptest.NewRequest("GET", "/api/v1/budget/status/"+provider, nil)
	w = httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)

	var statusResp APIResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &statusResp))
	assert.True(t, statusResp.Success)

	statusData, ok := statusResp.Data.(map[string]interface{})
	require.True(t, ok)
	dailyUsed, ok := statusData["daily_used"].(float64)
	require.True(t, ok)
	assert.Equal(t, float64(4200), dailyUsed)
}
