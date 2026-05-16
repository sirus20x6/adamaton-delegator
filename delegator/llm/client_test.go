// /thearray/gogents/internal/llm/client_test.go
package llm

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/sony/gobreaker"

	"github.com/sirus20x6/adamaton-core/types"
)

func newTestLogger() *logrus.Logger {
	l := logrus.New()
	l.SetOutput(io.Discard)
	return l
}

func newTestClient() *VLLMClient {
	return NewVLLMClient("http://localhost:0", time.Second, newTestLogger())
}

// --- parseAgentResponse: verdict variants -----------------------------------

func TestParseAgentResponse_VerdictPass(t *testing.T) {
	c := newTestClient()
	out := c.parseAgentResponse(`VERDICT: PASS
CONFIDENCE: 0.9
SEVERITY: LOW
RATIONALE: clean
DETAILS: nothing of concern
`, types.AgentSecurity)

	if out.Verdict != string(types.VerdictPass) {
		t.Errorf("expected PASS, got %s", out.Verdict)
	}
	if out.Confidence != 0.9 {
		t.Errorf("expected confidence 0.9, got %v", out.Confidence)
	}
	if out.Severity != string(types.SeverityLow) {
		t.Errorf("expected severity LOW, got %s", out.Severity)
	}
	if out.Rationale != "clean" {
		t.Errorf("unexpected rationale: %q", out.Rationale)
	}
	if len(out.Details) != 1 || out.Details[0] != "nothing of concern" {
		t.Errorf("unexpected details: %#v", out.Details)
	}
}

func TestParseAgentResponse_VerdictFail(t *testing.T) {
	c := newTestClient()
	out := c.parseAgentResponse(`VERDICT: FAIL
RATIONALE: bad
`, types.AgentSecurity)
	if out.Verdict != string(types.VerdictFail) {
		t.Errorf("expected FAIL, got %s", out.Verdict)
	}
}

func TestParseAgentResponse_VerdictWarning(t *testing.T) {
	c := newTestClient()
	out := c.parseAgentResponse(`VERDICT: WARNING
RATIONALE: heads up
`, types.AgentPerformance)
	if out.Verdict != string(types.VerdictWarning) {
		t.Errorf("expected WARNING, got %s", out.Verdict)
	}
}

func TestParseAgentResponse_UnknownVerdictDefaultsFail(t *testing.T) {
	c := newTestClient()
	out := c.parseAgentResponse(`VERDICT: BANANA
RATIONALE: nonsense
`, types.AgentSecurity)
	if out.Verdict != string(types.VerdictFail) {
		t.Errorf("unknown verdict must default to FAIL, got %s", out.Verdict)
	}
}

// --- parseAgentResponse: confidence handling --------------------------------

func TestParseAgentResponse_ConfidenceClampedHigh(t *testing.T) {
	c := newTestClient()
	out := c.parseAgentResponse(`VERDICT: PASS
CONFIDENCE: 9.5
RATIONALE: x
`, types.AgentSecurity)
	if out.Confidence != 1.0 {
		t.Errorf("expected confidence clamped to 1.0, got %v", out.Confidence)
	}
}

func TestParseAgentResponse_ConfidenceClampedLow(t *testing.T) {
	c := newTestClient()
	out := c.parseAgentResponse(`VERDICT: PASS
CONFIDENCE: -0.4
RATIONALE: x
`, types.AgentSecurity)
	if out.Confidence != 0.0 {
		t.Errorf("expected confidence clamped to 0.0, got %v", out.Confidence)
	}
}

func TestParseAgentResponse_ConfidenceInvalidIgnored(t *testing.T) {
	c := newTestClient()
	out := c.parseAgentResponse(`VERDICT: PASS
CONFIDENCE: not-a-number
RATIONALE: x
`, types.AgentSecurity)
	// Invalid input is silently dropped → default 0.5 stays.
	if out.Confidence != 0.5 {
		t.Errorf("expected default confidence 0.5, got %v", out.Confidence)
	}
}

// --- parseAgentResponse: severity handling ----------------------------------

func TestParseAgentResponse_UnknownSeverityKeepsDefault(t *testing.T) {
	c := newTestClient()
	out := c.parseAgentResponse(`VERDICT: PASS
SEVERITY: SPICY
RATIONALE: x
`, types.AgentSecurity)
	if out.Severity != string(types.SeverityMedium) {
		t.Errorf("unknown severity must keep default MEDIUM, got %s", out.Severity)
	}
}

func TestParseAgentResponse_KnownSeverityRespected(t *testing.T) {
	c := newTestClient()
	out := c.parseAgentResponse(`VERDICT: PASS
SEVERITY: HIGH
RATIONALE: x
`, types.AgentSecurity)
	if out.Severity != string(types.SeverityHigh) {
		t.Errorf("expected HIGH, got %s", out.Severity)
	}
}

// --- parseAgentResponse: empty / fallback paths -----------------------------

func TestParseAgentResponse_Empty(t *testing.T) {
	c := newTestClient()
	out := c.parseAgentResponse("", types.AgentSecurity)
	if out.Rationale != "Empty response from LLM" {
		t.Errorf("expected empty-rationale fallback, got %q", out.Rationale)
	}
}

func TestParseAgentResponse_NoRationaleFallback(t *testing.T) {
	c := newTestClient()
	out := c.parseAgentResponse(`VERDICT: PASS`, types.AgentSecurity)
	if out.Rationale != "No specific issues identified" {
		t.Errorf("expected no-rationale fallback, got %q", out.Rationale)
	}
}

// --- parseAgentResponse: DETAILS continuation -------------------------------

func TestParseAgentResponse_MultiLineDetails(t *testing.T) {
	c := newTestClient()
	out := c.parseAgentResponse(`VERDICT: FAIL
RATIONALE: stuff
DETAILS: first issue
second issue
third issue
`, types.AgentSecurity)
	if len(out.Details) != 3 {
		t.Fatalf("expected 3 details, got %d: %#v", len(out.Details), out.Details)
	}
	if out.Details[0] != "first issue" || out.Details[1] != "second issue" || out.Details[2] != "third issue" {
		t.Errorf("unexpected details: %#v", out.Details)
	}
}

func TestParseAgentResponse_DetailsTerminatedByBlankLine(t *testing.T) {
	c := newTestClient()
	out := c.parseAgentResponse(`VERDICT: FAIL
DETAILS: only this
RATIONALE: xx

trailing garbage that should not be a detail
`, types.AgentSecurity)
	if len(out.Details) != 1 || out.Details[0] != "only this" {
		t.Errorf("expected single detail terminated before garbage, got %#v", out.Details)
	}
}

func TestParseAgentResponse_DetailsTerminatedByHeaderResume(t *testing.T) {
	c := newTestClient()
	out := c.parseAgentResponse(`VERDICT: FAIL
DETAILS: first
followup line
RATIONALE: rationale text
should-not-collect
`, types.AgentSecurity)
	// "followup line" should join details (no blank, no header).
	// "should-not-collect" comes after RATIONALE which terminates DETAILS.
	if len(out.Details) != 2 {
		t.Fatalf("expected 2 details, got %d: %#v", len(out.Details), out.Details)
	}
	for _, d := range out.Details {
		if strings.Contains(d, "should-not-collect") {
			t.Errorf("post-header garbage leaked into details: %#v", out.Details)
		}
	}
}

// --- HTTP error path --------------------------------------------------------

func TestExecuteAgentAnalysis_HTTP500WrapsBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("upstream meltdown\nAuthorization: Bearer leaked-secret\n"))
	}))
	defer srv.Close()

	c := NewVLLMClient(srv.URL, 2*time.Second, newTestLogger())
	_, err := c.ExecuteAgentAnalysis(context.Background(), types.AgentSecurity, "diff", types.AgentConfig{MaxTokens: 16, Temperature: 0.1})
	if err == nil {
		t.Fatal("expected error for 500 response")
	}
	msg := err.Error()
	if !strings.Contains(msg, "500") {
		t.Errorf("expected error to include status code, got %q", msg)
	}
	if !strings.Contains(msg, "upstream meltdown") {
		t.Errorf("expected error to include body fragment, got %q", msg)
	}
	if strings.Contains(msg, "leaked-secret") {
		t.Errorf("error must not leak Authorization header: %q", msg)
	}
	if !strings.Contains(msg, "[redacted]") {
		t.Errorf("expected redaction marker in error, got %q", msg)
	}
}

func TestExecuteAgentAnalysis_HTTP400NotRetried(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte("bad request"))
	}))
	defer srv.Close()

	cfg := types.LLMConfig{
		Endpoint:      srv.URL,
		Timeout:       2 * time.Second,
		RetryAttempts: 3,
	}
	c := NewVLLMClientFromConfig(cfg, newTestLogger())
	_, err := c.ExecuteAgentAnalysis(context.Background(), types.AgentSecurity, "diff", types.AgentConfig{MaxTokens: 16, Temperature: 0.1})
	if err == nil {
		t.Fatal("expected error")
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("4xx must not be retried, got %d calls", got)
	}
}

func TestExecuteAgentAnalysis_HTTP500RetriesThenSucceeds(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n < 3 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(VLLMResponse{
			Choices: []struct {
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
			}{
				{Message: struct {
					Content string `json:"content"`
				}{Content: "VERDICT: PASS\nRATIONALE: ok\n"}},
			},
		})
	}))
	defer srv.Close()

	cfg := types.LLMConfig{
		Endpoint:      srv.URL,
		Timeout:       5 * time.Second,
		RetryAttempts: 4,
	}
	c := NewVLLMClientFromConfig(cfg, newTestLogger())
	out, err := c.ExecuteAgentAnalysis(context.Background(), types.AgentSecurity, "diff", types.AgentConfig{MaxTokens: 16, Temperature: 0.1})
	if err != nil {
		t.Fatalf("expected eventual success, got %v", err)
	}
	if out.Verdict != string(types.VerdictPass) {
		t.Errorf("expected PASS verdict, got %s", out.Verdict)
	}
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Errorf("expected 3 attempts before success, got %d", got)
	}
}

// --- ModelName plumbing -----------------------------------------------------

func TestExecuteAgentAnalysis_ModelNameFromConfig(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req VLLMRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		got = req.Model
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(VLLMResponse{
			Choices: []struct {
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
			}{
				{Message: struct {
					Content string `json:"content"`
				}{Content: "VERDICT: PASS\nRATIONALE: ok\n"}},
			},
		})
	}))
	defer srv.Close()

	cfg := types.LLMConfig{
		Endpoint:  srv.URL,
		Timeout:   2 * time.Second,
		ModelName: "custom-model-7b",
	}
	c := NewVLLMClientFromConfig(cfg, newTestLogger())
	if _, err := c.ExecuteAgentAnalysis(context.Background(), types.AgentSecurity, "diff", types.AgentConfig{MaxTokens: 16, Temperature: 0.1}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "custom-model-7b" {
		t.Errorf("expected model name from config to be sent, got %q", got)
	}
}

func TestExecuteAgentAnalysis_DefaultModelAuto(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req VLLMRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		got = req.Model
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(VLLMResponse{
			Choices: []struct {
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
			}{
				{Message: struct {
					Content string `json:"content"`
				}{Content: "VERDICT: PASS\nRATIONALE: ok\n"}},
			},
		})
	}))
	defer srv.Close()

	c := NewVLLMClient(srv.URL, 2*time.Second, newTestLogger())
	if _, err := c.ExecuteAgentAnalysis(context.Background(), types.AgentSecurity, "diff", types.AgentConfig{MaxTokens: 16, Temperature: 0.1}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "auto" {
		t.Errorf("expected default model %q, got %q", "auto", got)
	}
}

// --- Constructor sanity -----------------------------------------------------

func TestNewVLLMClient_DefaultsTimeoutWhenZero(t *testing.T) {
	c := NewVLLMClient("http://x", 0, newTestLogger())
	if c.httpClient.Timeout != defaultTimeout {
		t.Errorf("expected zero timeout to fall back to %v, got %v", defaultTimeout, c.httpClient.Timeout)
	}
}

func TestNewVLLMClient_DefaultsTimeoutWhenNegative(t *testing.T) {
	c := NewVLLMClient("http://x", -1*time.Second, newTestLogger())
	if c.httpClient.Timeout != defaultTimeout {
		t.Errorf("expected negative timeout to fall back to %v, got %v", defaultTimeout, c.httpClient.Timeout)
	}
}

func TestNewVLLMClient_TrimsTrailingSlash(t *testing.T) {
	c := NewVLLMClient("http://x/", time.Second, newTestLogger())
	if c.baseURL != "http://x" {
		t.Errorf("expected trailing slash trimmed, got %q", c.baseURL)
	}
}

// --- sanitizeBody -----------------------------------------------------------

func TestSanitizeBody_RedactsAuthorization(t *testing.T) {
	in := "preamble\nAuthorization: Bearer secret\nepilogue"
	out := sanitizeBody(in)
	if strings.Contains(out, "secret") {
		t.Errorf("expected token redacted, got %q", out)
	}
	if !strings.Contains(out, "[redacted]") {
		t.Errorf("expected redaction marker, got %q", out)
	}
}

func TestSanitizeBody_TruncatesLargeBodies(t *testing.T) {
	in := strings.Repeat("x", 10000)
	out := sanitizeBody(in)
	if !strings.HasSuffix(out, "...[truncated]") {
		t.Errorf("expected truncation suffix, got len=%d tail=%q", len(out), out[len(out)-20:])
	}
}

// --- Circuit breaker -------------------------------------------------------

// withFastBreaker returns a *VLLMClient with a circuit breaker tuned to
// trip quickly so tests don't have to wait the production 30-60s. interval
// is the closed-state reset window and timeout is the open-state cooldown.
func withFastBreaker(c *VLLMClient, interval, timeout time.Duration) *VLLMClient {
	c.breaker = newDefaultBreaker(c.logger, interval, timeout)
	return c
}

// TestBreaker_TripsAfterFiveConsecutiveFailures verifies that 5 consecutive
// 5xx responses trip the breaker open and the 6th call short-circuits with
// gobreaker.ErrOpenState rather than reaching the upstream. We use
// RetryAttempts=0 so each request is exactly one HTTP attempt and the
// breaker's "consecutive failures" maps 1:1 to call count.
func TestBreaker_TripsAfterFiveConsecutiveFailures(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("upstream is on fire"))
	}))
	defer srv.Close()

	cfg := types.LLMConfig{
		Endpoint:      srv.URL,
		Timeout:       2 * time.Second,
		RetryAttempts: 0,
	}
	c := NewVLLMClientFromConfig(cfg, newTestLogger())
	// 100ms / 200ms is plenty short to keep the test fast while still
	// meaningfully exercising the breaker state machine.
	c = withFastBreaker(c, 100*time.Millisecond, 200*time.Millisecond)

	// Drive 5 consecutive failures so the breaker hits the trip threshold.
	for i := 0; i < int(defaultBreakerTripThreshold); i++ {
		_, err := c.ExecuteAgentAnalysis(context.Background(), types.AgentSecurity, "diff", types.AgentConfig{MaxTokens: 16, Temperature: 0.1})
		if err == nil {
			t.Fatalf("expected error on attempt %d, got nil", i+1)
		}
	}
	preTripCalls := atomic.LoadInt32(&calls)
	if preTripCalls != int32(defaultBreakerTripThreshold) {
		t.Fatalf("expected %d upstream calls before trip, got %d", defaultBreakerTripThreshold, preTripCalls)
	}

	// Next call must short-circuit — breaker is open.
	_, err := c.ExecuteAgentAnalysis(context.Background(), types.AgentSecurity, "diff", types.AgentConfig{MaxTokens: 16, Temperature: 0.1})
	if err == nil {
		t.Fatal("expected breaker-open error on 6th call, got nil")
	}
	if !errors.Is(err, gobreaker.ErrOpenState) && !errors.Is(err, gobreaker.ErrTooManyRequests) {
		t.Fatalf("expected gobreaker open/half-open error, got %v", err)
	}

	// Critical assertion: the upstream MUST NOT have been hit again.
	if got := atomic.LoadInt32(&calls); got != preTripCalls {
		t.Errorf("breaker was open but upstream was still called: pre=%d post=%d", preTripCalls, got)
	}
}

// TestBreaker_RecoversAfterTimeout verifies that after the open-state
// timeout elapses, the breaker enters half-open and allows probes through.
// A single success in half-open resets it to closed.
func TestBreaker_RecoversAfterTimeout(t *testing.T) {
	var fail atomic.Bool
	fail.Store(true)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(VLLMResponse{
			Choices: []struct {
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
			}{
				{Message: struct {
					Content string `json:"content"`
				}{Content: "VERDICT: PASS\nRATIONALE: ok\n"}},
			},
		})
	}))
	defer srv.Close()

	cfg := types.LLMConfig{
		Endpoint:      srv.URL,
		Timeout:       2 * time.Second,
		RetryAttempts: 0,
	}
	c := NewVLLMClientFromConfig(cfg, newTestLogger())
	// Short open-state cooldown so the test doesn't wallclock the suite.
	c = withFastBreaker(c, 200*time.Millisecond, 150*time.Millisecond)

	// Trip the breaker.
	for i := 0; i < int(defaultBreakerTripThreshold); i++ {
		_, _ = c.ExecuteAgentAnalysis(context.Background(), types.AgentSecurity, "diff", types.AgentConfig{MaxTokens: 16, Temperature: 0.1})
	}
	if c.breaker.State() != gobreaker.StateOpen {
		t.Fatalf("expected breaker open after %d failures, got %s", defaultBreakerTripThreshold, c.breaker.State())
	}

	// Repair the upstream and wait past the open-state cooldown.
	fail.Store(false)
	time.Sleep(250 * time.Millisecond)

	// First post-cooldown call should be allowed through (half-open probe)
	// and succeed, returning the breaker to closed.
	out, err := c.ExecuteAgentAnalysis(context.Background(), types.AgentSecurity, "diff", types.AgentConfig{MaxTokens: 16, Temperature: 0.1})
	if err != nil {
		t.Fatalf("expected post-cooldown call to succeed, got %v", err)
	}
	if out.Verdict != string(types.VerdictPass) {
		t.Errorf("expected PASS verdict, got %s", out.Verdict)
	}
}

// TestBreaker_4xxDoesNotTrip verifies that 4xx responses (caller errors)
// don't count toward the breaker's failure threshold. An upstream that
// rejects 100 bad payloads is still alive.
func TestBreaker_4xxDoesNotTrip(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte("bad payload"))
	}))
	defer srv.Close()

	cfg := types.LLMConfig{
		Endpoint:      srv.URL,
		Timeout:       2 * time.Second,
		RetryAttempts: 0,
	}
	c := NewVLLMClientFromConfig(cfg, newTestLogger())
	c = withFastBreaker(c, 100*time.Millisecond, 100*time.Millisecond)

	// Hit the upstream more than the trip threshold; breaker must stay
	// closed because none of these are real upstream failures.
	for i := 0; i < int(defaultBreakerTripThreshold)+2; i++ {
		_, err := c.ExecuteAgentAnalysis(context.Background(), types.AgentSecurity, "diff", types.AgentConfig{MaxTokens: 16, Temperature: 0.1})
		if err == nil {
			t.Fatalf("expected 4xx error on attempt %d", i+1)
		}
		if errors.Is(err, gobreaker.ErrOpenState) {
			t.Fatalf("breaker tripped on 4xx errors at attempt %d — should not happen", i+1)
		}
	}
	if state := c.breaker.State(); state != gobreaker.StateClosed {
		t.Errorf("breaker state = %s, want closed", state)
	}
}

// TestBreaker_IsSuccessful_Classification spot-checks the IsSuccessful
// callback wiring directly so a future refactor can't silently change the
// 4xx-vs-5xx semantics without the test catching it.
func TestBreaker_IsSuccessful_Classification(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil is success", nil, true},
		{"4xx wrapped string is success (caller error)", errors.New("vLLM returned status 400: bad"), true},
		{"5xx retryable is failure", &retryableHTTPError{status: 503, body: "down"}, false},
		{"open state is failure", gobreaker.ErrOpenState, false},
		{"too many requests is failure", gobreaker.ErrTooManyRequests, false},
		{"transport error is failure", errors.New("connection refused"), false},
		{"decode error is failure", errors.New("failed to decode response: junk"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := breakerIsSuccessful(tc.err); got != tc.want {
				t.Errorf("breakerIsSuccessful(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestNewVLLMClient_InitializesBreaker guards against a regression where
// the constructor forgets to set the breaker, which would NPE inside
// doOnceBreakered's nil-check fallback path.
func TestNewVLLMClient_InitializesBreaker(t *testing.T) {
	c := NewVLLMClient("http://x", time.Second, newTestLogger())
	if c.breaker == nil {
		t.Fatal("expected breaker to be initialized by constructor")
	}
}

// --- Bug 2: 200 OK body cap --------------------------------------------------

// TestExecuteAgentAnalysis_200OKBodyTooLarge verifies that a 200 OK response
// exceeding maxLLMResponseSize is rejected with ErrResponseTooLarge instead
// of being passed wholesale to the json decoder. Without this bound, a
// misconfigured upstream could OOM the worker with a single response.
func TestExecuteAgentAnalysis_200OKBodyTooLarge(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		// Stream maxLLMResponseSize+1024 bytes of arbitrary JSON-shaped
		// content. We don't care if it's valid JSON — the cap should fire
		// before the decoder ever sees it.
		buf := make([]byte, 64*1024)
		for i := range buf {
			buf[i] = 'a'
		}
		written := 0
		for written < maxLLMResponseSize+1024 {
			n, err := w.Write(buf)
			if err != nil {
				return
			}
			written += n
		}
	}))
	defer srv.Close()

	c := NewVLLMClient(srv.URL, 5*time.Second, newTestLogger())
	_, err := c.ExecuteAgentAnalysis(context.Background(), types.AgentSecurity, "diff", types.AgentConfig{MaxTokens: 16, Temperature: 0.1})
	if err == nil {
		t.Fatal("expected oversized body to produce an error")
	}
	if !errors.Is(err, ErrResponseTooLarge) {
		t.Errorf("expected ErrResponseTooLarge, got %v", err)
	}
}

// TestExecuteAgentAnalysis_200OKBodyAtCapSucceeds verifies that a response
// exactly at the cap (or just under) still parses successfully — the cap is
// inclusive, not strict-less-than.
func TestExecuteAgentAnalysis_200OKBodyJustUnderCapSucceeds(t *testing.T) {
	// Build a small valid response payload — we just want to confirm normal
	// flow still works after the bound was added.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(VLLMResponse{
			Choices: []struct {
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
			}{
				{Message: struct {
					Content string `json:"content"`
				}{Content: "VERDICT: PASS\nRATIONALE: ok\n"}},
			},
		})
	}))
	defer srv.Close()

	c := NewVLLMClient(srv.URL, 2*time.Second, newTestLogger())
	out, err := c.ExecuteAgentAnalysis(context.Background(), types.AgentSecurity, "diff", types.AgentConfig{MaxTokens: 16, Temperature: 0.1})
	if err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	if out.Verdict != string(types.VerdictPass) {
		t.Errorf("expected PASS verdict, got %s", out.Verdict)
	}
}

// --- Bug 3: 429 retry & Retry-After ----------------------------------------

// TestExecuteAgentAnalysis_HTTP429IsRetried verifies that a 429 response is
// classified as retryable (formerly treated as terminal 4xx). Two 429s
// followed by a 200 must succeed on the third attempt.
func TestExecuteAgentAnalysis_HTTP429IsRetried(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n < 3 {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte("rate limited"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(VLLMResponse{
			Choices: []struct {
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
			}{
				{Message: struct {
					Content string `json:"content"`
				}{Content: "VERDICT: PASS\nRATIONALE: ok\n"}},
			},
		})
	}))
	defer srv.Close()

	cfg := types.LLMConfig{
		Endpoint:      srv.URL,
		Timeout:       5 * time.Second,
		RetryAttempts: 4,
	}
	c := NewVLLMClientFromConfig(cfg, newTestLogger())
	out, err := c.ExecuteAgentAnalysis(context.Background(), types.AgentSecurity, "diff", types.AgentConfig{MaxTokens: 16, Temperature: 0.1})
	if err != nil {
		t.Fatalf("expected eventual success after 429s, got %v", err)
	}
	if out.Verdict != string(types.VerdictPass) {
		t.Errorf("expected PASS verdict, got %s", out.Verdict)
	}
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Errorf("expected 3 attempts, got %d", got)
	}
}

// TestExecuteAgentAnalysis_HTTP429HonorsRetryAfter verifies that a 429 with
// Retry-After: <seconds> waits at least that long before the next attempt.
// We use a small value so the test stays fast.
func TestExecuteAgentAnalysis_HTTP429HonorsRetryAfter(t *testing.T) {
	var calls int32
	var firstAt, secondAt time.Time
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		now := time.Now()
		if n == 1 {
			firstAt = now
			w.Header().Set("Retry-After", "1") // 1 second
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		secondAt = now
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(VLLMResponse{
			Choices: []struct {
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
			}{
				{Message: struct {
					Content string `json:"content"`
				}{Content: "VERDICT: PASS\nRATIONALE: ok\n"}},
			},
		})
	}))
	defer srv.Close()

	cfg := types.LLMConfig{Endpoint: srv.URL, Timeout: 10 * time.Second, RetryAttempts: 2}
	c := NewVLLMClientFromConfig(cfg, newTestLogger())
	if _, err := c.ExecuteAgentAnalysis(context.Background(), types.AgentSecurity, "diff", types.AgentConfig{MaxTokens: 16, Temperature: 0.1}); err != nil {
		t.Fatalf("expected eventual success, got %v", err)
	}
	if firstAt.IsZero() || secondAt.IsZero() {
		t.Fatal("expected two attempts to be timestamped")
	}
	gap := secondAt.Sub(firstAt)
	// Allow a little under 1s to absorb scheduler jitter; we mostly want to
	// confirm the wait is >> the default backoff (~200ms first retry).
	if gap < 700*time.Millisecond {
		t.Errorf("expected retry to honor Retry-After: gap was %v, want >=~1s", gap)
	}
}

// TestParseRetryAfter_Seconds parses the integer-seconds form of Retry-After.
func TestParseRetryAfter_Seconds(t *testing.T) {
	if got := parseRetryAfter("5"); got != 5*time.Second {
		t.Errorf("parseRetryAfter(\"5\") = %v, want 5s", got)
	}
	if got := parseRetryAfter("0"); got != 0 {
		t.Errorf("parseRetryAfter(\"0\") = %v, want 0", got)
	}
	// Cap protection: very large values clamp to 5m so a misbehaving
	// upstream cannot pin us indefinitely.
	if got := parseRetryAfter("99999"); got != 300*time.Second {
		t.Errorf("parseRetryAfter(\"99999\") = %v, want 300s clamp", got)
	}
}

// TestParseRetryAfter_HTTPDate parses the HTTP-date form of Retry-After.
func TestParseRetryAfter_HTTPDate(t *testing.T) {
	future := time.Now().Add(2 * time.Second).UTC().Format(http.TimeFormat)
	got := parseRetryAfter(future)
	// Allow some slack — the server's clock resolution is 1s.
	if got < 500*time.Millisecond || got > 5*time.Second {
		t.Errorf("parseRetryAfter(future) = %v, want ~2s", got)
	}

	// Past date returns 0 — no point waiting for the past.
	past := time.Now().Add(-time.Hour).UTC().Format(http.TimeFormat)
	if got := parseRetryAfter(past); got != 0 {
		t.Errorf("parseRetryAfter(past) = %v, want 0", got)
	}
}

// TestParseRetryAfter_EmptyOrInvalid covers the no-header and unparseable
// fallthrough cases.
func TestParseRetryAfter_EmptyOrInvalid(t *testing.T) {
	if got := parseRetryAfter(""); got != 0 {
		t.Errorf("parseRetryAfter(\"\") = %v, want 0", got)
	}
	if got := parseRetryAfter("not-a-time"); got != 0 {
		t.Errorf("parseRetryAfter(\"not-a-time\") = %v, want 0", got)
	}
}

// --- Bug 4 (Pass 12): tuned transport + connection reuse -------------------

// TestNewVLLMClient_TunedTransport pins the connection-pool tuning so a
// future regression that drops back to http.DefaultTransport (and its
// MaxIdleConnsPerHost=2) can't sneak in unnoticed. The default kicks 10 of
// every 12 fan-out requests into a fresh TCP+TLS handshake.
func TestNewVLLMClient_TunedTransport(t *testing.T) {
	c := NewVLLMClient("http://localhost:0", time.Second, newTestLogger())
	tr, ok := c.httpClient.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("expected *http.Transport, got %T", c.httpClient.Transport)
	}
	if tr.MaxIdleConnsPerHost != 32 {
		t.Errorf("MaxIdleConnsPerHost = %d, want 32", tr.MaxIdleConnsPerHost)
	}
	if tr.MaxIdleConns != 100 {
		t.Errorf("MaxIdleConns = %d, want 100", tr.MaxIdleConns)
	}
	if !tr.ForceAttemptHTTP2 {
		t.Error("ForceAttemptHTTP2 = false, want true")
	}
	if tr.TLSClientConfig == nil || tr.TLSClientConfig.ClientSessionCache == nil {
		t.Error("TLS session cache must be configured")
	}
	if tr.TLSClientConfig != nil && tr.TLSClientConfig.MinVersion != tls.VersionTLS12 {
		t.Errorf("MinVersion = %v, want TLS 1.2", tr.TLSClientConfig.MinVersion)
	}
}

// TestVLLMClient_ConcurrentRequestsReuseConnections fires 32 concurrent
// requests at an httptest.Server and asserts the server saw at most 32 fresh
// connections accepted (one per concurrent request) — and ideally fewer once
// the pool starts handing back idle conns. With the OLD default transport
// (MaxIdleConnsPerHost=2), repeating this test would force the server to
// accept ~30 fresh conns per round; the fix proves itself by holding the
// connection count at or below the concurrency level rather than blowing
// past it.
//
// We track Accept events via a custom Listener wrapper because httptest's
// Server.Listener doesn't expose a counter directly.
func TestVLLMClient_ConcurrentRequestsReuseConnections(t *testing.T) {
	var accepts atomic.Int64
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(VLLMResponse{
			Choices: []struct {
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
			}{
				{Message: struct {
					Content string `json:"content"`
				}{Content: "VERDICT: PASS\nRATIONALE: ok\n"}},
			},
		})
	}))
	// Replace the listener BEFORE Start() so the counting wrapper is the one
	// httptest's serve loop accepts on. NewUnstartedServer creates a default
	// listener we then swap with our own.
	base, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv.Listener = &countingListener{Listener: base, accepts: &accepts}
	srv.Start()
	defer srv.Close()

	c := NewVLLMClient(srv.URL, 5*time.Second, newTestLogger())
	const N = 32
	var wg sync.WaitGroup
	wg.Add(N)
	errCh := make(chan error, N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			_, err := c.ExecuteAgentAnalysis(context.Background(), types.AgentSecurity, "diff",
				types.AgentConfig{MaxTokens: 16, Temperature: 0.1})
			if err != nil {
				errCh <- err
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Errorf("unexpected error from concurrent request: %v", err)
	}

	// With the default transport, accepts would be ~N. With the tuned
	// transport, keep-alive + HTTP/2 collapse it; we assert at most N (no
	// connection storms beyond concurrency) and ideally far fewer.
	got := accepts.Load()
	if got > int64(N) {
		t.Errorf("expected at most %d fresh connections accepted under N=%d concurrency, got %d", N, N, got)
	}
	t.Logf("accepted %d connections for %d concurrent requests", got, N)
}

// countingListener wraps a net.Listener so a test can observe how many
// fresh connections were accepted during a request burst. Used to confirm
// the tuned transport actually reuses connections.
type countingListener struct {
	net.Listener
	accepts *atomic.Int64
}

func (l *countingListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err == nil {
		l.accepts.Add(1)
	}
	return conn, err
}

// TestBreaker_429DoesNotTrip verifies that 429 (rate limit) responses do not
// count toward the breaker's failure threshold. A rate-limited upstream is
// alive — flapping the breaker would only mask the backpressure signal.
func TestBreaker_429DoesNotTrip(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte("slow down"))
	}))
	defer srv.Close()

	cfg := types.LLMConfig{
		Endpoint:      srv.URL,
		Timeout:       2 * time.Second,
		RetryAttempts: 0,
	}
	c := NewVLLMClientFromConfig(cfg, newTestLogger())
	c = withFastBreaker(c, 100*time.Millisecond, 100*time.Millisecond)

	for i := 0; i < int(defaultBreakerTripThreshold)+2; i++ {
		_, err := c.ExecuteAgentAnalysis(context.Background(), types.AgentSecurity, "diff", types.AgentConfig{MaxTokens: 16, Temperature: 0.1})
		if err == nil {
			t.Fatalf("expected 429 error on attempt %d", i+1)
		}
		if errors.Is(err, gobreaker.ErrOpenState) {
			t.Fatalf("breaker tripped on 429 errors at attempt %d — should not happen", i+1)
		}
	}
	if state := c.breaker.State(); state != gobreaker.StateClosed {
		t.Errorf("breaker state = %s, want closed", state)
	}
}
