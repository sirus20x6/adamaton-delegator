// /thearray/gogents/internal/llm/client.go
package llm

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/sony/gobreaker"

	"github.com/sirus20x6/adamaton-core/metrics"
	"github.com/sirus20x6/adamaton-core/types"
)

// defaultTimeout is used when a caller passes a non-positive timeout. A zero
// timeout on http.Client means "no timeout" which is almost never what we
// want here — fall back to a sane upper bound instead of silently hanging.
const defaultTimeout = 60 * time.Second

// errorBodyLimit caps how much of an upstream error body we read into memory
// before truncating. Matches the bound used by the Gitea client.
const errorBodyLimit = 64 * 1024

// maxLLMResponseSize bounds a successful (200 OK) response body before we
// hand it to json.Decode. Without this, a misconfigured or malicious upstream
// could stream gigabytes into RAM and OOM the worker. 8 MiB is generous for
// chat-completion responses (typical responses are <100 KB) while still
// catching pathological cases.
const maxLLMResponseSize = 8 << 20 // 8 MiB

// ErrResponseTooLarge is returned when an upstream sends a 200 OK body
// exceeding maxLLMResponseSize. It is wrapped with %w so callers can
// errors.Is against it.
var ErrResponseTooLarge = errors.New("vLLM response body exceeds size limit")

// maxRetryAttempts is an upper bound on user-configured RetryAttempts to
// avoid pathological retry storms.
const maxRetryAttempts = 5

// Circuit breaker tuning. With 12 agents fanning out, a dead vLLM with
// 3-minute timeouts and 3 retries would otherwise stall a workflow for
// ~108 minutes before giving up. The breaker collapses repeated failures
// into a fast-fail.
//
//   - defaultBreakerMaxRequests caps half-open probes; the breaker remains
//     open until that many succeed in a row.
//   - defaultBreakerInterval is the closed-state window for resetting the
//     consecutive-failure counter; failures spread over more than this
//     period don't accumulate.
//   - defaultBreakerTimeout is the open-state cooldown before the breaker
//     attempts a half-open probe.
//   - defaultBreakerTripThreshold is the number of consecutive failures
//     that flips the breaker open.
const (
	defaultBreakerMaxRequests   uint32 = 3
	defaultBreakerInterval             = 60 * time.Second
	defaultBreakerTimeout              = 30 * time.Second
	defaultBreakerTripThreshold uint32 = 5
)

// authHeaderRegex matches whole `authorization: ...` lines that some
// upstreams echo back when reflecting the original request. We consume the
// rest of the line so the redacted output never carries the bearer token.
var authHeaderRegex = regexp.MustCompile(`(?i)authorization:[^\r\n]*`)

// sanitizeBody redacts authorization headers and truncates very long bodies
// before they are interpolated into error messages or log fields.
func sanitizeBody(body string) string {
	body = authHeaderRegex.ReplaceAllString(body, "authorization: [redacted]")
	if len(body) > 4096 {
		body = body[:4096] + "...[truncated]"
	}
	return body
}

type VLLMClient struct {
	baseURL       string
	httpClient    *http.Client
	logger        *logrus.Logger
	modelName     string // sent in VLLMRequest.Model when non-empty
	retryAttempts int    // 0 = no retries

	// breaker fast-fails when the upstream is repeatedly failing. Wrapping
	// individual HTTP attempts (NOT the retry loop) means each retry counts
	// as a separate breaker observation. 4xx responses do NOT count as
	// breaker failures because they signal the upstream is alive and the
	// request was bad — see breakerIsSuccessful.
	breaker *gobreaker.CircuitBreaker
}

type ChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type VLLMRequest struct {
	Model       string        `json:"model"`
	Messages    []ChatMessage `json:"messages"`
	MaxTokens   int           `json:"max_tokens,omitempty"`
	Temperature float64       `json:"temperature,omitempty"`
	TopP        float64       `json:"top_p,omitempty"`
	Stream      bool          `json:"stream"`
}

type VLLMResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
}

func NewVLLMClient(baseURL string, timeout time.Duration, logger *logrus.Logger) *VLLMClient {
	if baseURL == "" {
		baseURL = "http://localhost:8000"
	}
	// Strip trailing slash so we don't construct double-slashed URLs like
	// "http://host//v1/chat/completions".
	baseURL = strings.TrimSuffix(baseURL, "/")

	if timeout <= 0 {
		timeout = defaultTimeout
	}

	c := &VLLMClient{
		baseURL: baseURL,
		httpClient: &http.Client{
			Timeout:   timeout,
			Transport: newTunedVLLMTransport(),
		},
		logger: logger,
	}
	c.breaker = newDefaultBreaker(logger, defaultBreakerInterval, defaultBreakerTimeout)
	return c
}

// newTunedVLLMTransport mirrors the tuning rationale documented on
// internal/gitea.newTunedGiteaTransport: the default transport's
// MaxIdleConnsPerHost=2 forces the 12-agent fan-out into ~10 fresh TCP+TLS
// handshakes per round, which we measured at ~120ms each on the production
// vLLM endpoint. With MaxIdleConnsPerHost=32 plus a TLS session cache,
// keep-alives + HTTP/2 multiplexing collapse that to a single warm conn.
//
// The vLLM client only ever talks to the configured Endpoint (a trusted
// upstream), so we do NOT install the SSRF-aware DialContext that lives in
// internal/executor — that one applies to workflow-author-driven requests.
func newTunedVLLMTransport() *http.Transport {
	tlsConfig := &tls.Config{
		MinVersion:         tls.VersionTLS12,
		ClientSessionCache: tls.NewLRUClientSessionCache(64),
	}
	return &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   32,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		TLSClientConfig:       tlsConfig,
	}
}

// breakerIsSuccessful tells the breaker which errors NOT to count as
// failures. 4xx responses come back wrapped in a non-retryable "vLLM
// returned status N" error. Those are caller-side bugs (bad payload, bad
// model name, etc.) — the upstream is alive, so they must NOT trip the
// breaker. Everything else (5xx wrapped in retryableHTTPError, transport
// errors, decode errors, gobreaker.ErrOpenState propagation) is treated
// as a real failure.
func breakerIsSuccessful(err error) bool {
	if err == nil {
		return true
	}
	if errors.Is(err, gobreaker.ErrOpenState) || errors.Is(err, gobreaker.ErrTooManyRequests) {
		// These errors come from the breaker itself when callers race past
		// it; they would never reach IsSuccessful in normal operation, but
		// guard against double-counting just in case.
		return false
	}
	// 5xx and 429 are retryable. 5xx tells us the upstream is sick and
	// SHOULD trip the breaker. 429 tells us the upstream is alive and
	// rate-limiting us — that is NOT a sign of upstream failure, so we
	// treat 429 as "successful" from the breaker's point of view to
	// avoid flapping the breaker open under sustained backpressure.
	var rerr *retryableHTTPError
	if errors.As(err, &rerr) {
		if rerr.status == http.StatusTooManyRequests {
			return true
		}
		return false
	}
	// Plain "vLLM returned status N: ..." errors carry the 4xx case. We
	// detect them by message prefix because we don't wrap them in a typed
	// error (4xx is terminal so we never need to introspect them later).
	if strings.HasPrefix(err.Error(), "vLLM returned status ") {
		// 4xx — caller error, don't trip breaker.
		return true
	}
	// Transport, marshal, decode, ctx errors.
	return false
}

// newDefaultBreaker returns a *gobreaker.CircuitBreaker preconfigured for
// the vLLM upstream. interval/timeout are parameters because tests need to
// dial them down to milliseconds; production callers should pass the
// defaultBreakerInterval / defaultBreakerTimeout constants.
func newDefaultBreaker(logger *logrus.Logger, interval, timeout time.Duration) *gobreaker.CircuitBreaker {
	settings := gobreaker.Settings{
		Name:        "vllm",
		MaxRequests: defaultBreakerMaxRequests,
		Interval:    interval,
		Timeout:     timeout,
		ReadyToTrip: func(counts gobreaker.Counts) bool {
			return counts.ConsecutiveFailures >= defaultBreakerTripThreshold
		},
		OnStateChange: func(name string, from, to gobreaker.State) {
			metrics.CircuitBreakerStateChanges.WithLabelValues(name, from.String(), to.String()).Inc()
			if logger == nil {
				return
			}
			logger.WithFields(logrus.Fields{
				"breaker": name,
				"from":    from.String(),
				"to":      to.String(),
			}).Warn("vLLM circuit breaker state change")
		},
		IsSuccessful: breakerIsSuccessful,
	}
	return gobreaker.NewCircuitBreaker(settings)
}

// NewVLLMClientFromConfig wires the LLMConfig knobs that NewVLLMClient cannot
// take directly: model name (used in VLLMRequest.Model instead of the
// hardcoded "auto") and retry attempts (5xx + transport errors only).
//
// cfg.UseChatAPI is intentionally NOT consumed here: the client only ever
// targets /v1/chat/completions today, so the field is honored implicitly.
// The external config flag is preserved as an API contract — see
// internal/apiserver/llm_endpoints.go which surfaces it. When a non-chat
// endpoint is added, reintroduce a stored field and branch on it.
func NewVLLMClientFromConfig(cfg types.LLMConfig, logger *logrus.Logger) *VLLMClient {
	c := NewVLLMClient(cfg.Endpoint, cfg.Timeout, logger)
	c.modelName = cfg.ModelName
	if cfg.RetryAttempts > 0 {
		c.retryAttempts = cfg.RetryAttempts
		if c.retryAttempts > maxRetryAttempts {
			c.retryAttempts = maxRetryAttempts
		}
	}
	return c
}

func (c *VLLMClient) ExecuteAgentAnalysis(ctx context.Context, agentType types.AgentType, diff string, config types.AgentConfig) (types.LLMCheckResult, error) {
	startTime := time.Now()

	// Build system prompt for the agent
	systemPrompt := c.buildAgentSystemPrompt(agentType)
	userPrompt := fmt.Sprintf("Analyze this code diff:\n\n```diff\n%s\n```", diff)

	messages := []ChatMessage{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: userPrompt},
	}

	model := c.modelName
	if model == "" {
		model = "auto" // vLLM will use the loaded model
	}

	request := VLLMRequest{
		Model:       model,
		Messages:    messages,
		MaxTokens:   config.MaxTokens,
		Temperature: config.Temperature,
		TopP:        0.95,
		Stream:      false,
	}

	if request.MaxTokens == 0 {
		request.MaxTokens = 512
	}

	jsonData, err := json.Marshal(request)
	if err != nil {
		return types.LLMCheckResult{}, fmt.Errorf("failed to marshal request: %w", err)
	}

	url := fmt.Sprintf("%s/v1/chat/completions", c.baseURL)

	vllmResp, err := c.doWithRetry(ctx, url, jsonData)
	if err != nil {
		return types.LLMCheckResult{}, err
	}

	if len(vllmResp.Choices) == 0 {
		return types.LLMCheckResult{}, fmt.Errorf("no choices returned from vLLM")
	}

	response := vllmResp.Choices[0].Message.Content
	result := c.parseAgentResponse(response, agentType)

	duration := time.Since(startTime)
	// Per-success completion is high-volume (3 agents × N PRs/day) and is
	// already covered by the LLMRequests counter and LLMRequestDuration
	// histogram in internal/metrics (Pass 12). Logging at Debug keeps the
	// signal available for local development without flooding aggregators
	// in production. Failure paths remain at Warn/Error.
	c.logger.WithFields(logrus.Fields{
		"agent":      agentType,
		"verdict":    result.Verdict,
		"confidence": result.Confidence,
		"duration":   duration,
		"tokens":     vllmResp.Usage.TotalTokens,
	}).Debug("vLLM agent analysis completed")

	return result, nil
}

// retryableHTTPError signals a 5xx or 429 response that the retry loop is
// allowed to retry. 4xx (except 429) and decode errors are NOT wrapped in
// this type so they are returned to the caller immediately.
//
// retryAfter, when non-zero, is the wait the server requested via the
// Retry-After header. The retry loop honors this in preference to its own
// jittered backoff so we don't hammer a rate-limited upstream.
type retryableHTTPError struct {
	status     int
	body       string
	retryAfter time.Duration
}

func (e *retryableHTTPError) Error() string {
	return fmt.Sprintf("vLLM returned status %d: %s", e.status, e.body)
}

// parseRetryAfter parses an HTTP Retry-After header value, which may be
// either an integer number of seconds or an HTTP-date. Returns 0 when the
// header is absent or unparseable; the retry loop falls through to its
// jittered backoff in that case.
func parseRetryAfter(h string) time.Duration {
	h = strings.TrimSpace(h)
	if h == "" {
		return 0
	}
	if secs, err := strconv.Atoi(h); err == nil && secs >= 0 {
		// Cap at a sane upper bound so a misbehaving upstream cannot
		// pin us for hours.
		if secs > 300 {
			secs = 300
		}
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(h); err == nil {
		d := time.Until(t)
		if d <= 0 {
			return 0
		}
		if d > 5*time.Minute {
			d = 5 * time.Minute
		}
		return d
	}
	return 0
}

// doWithRetry performs a POST to url with jsonData as body, decoding the
// response into a VLLMResponse. It retries on transport errors and 5xx
// responses with exponential backoff + jitter, up to c.retryAttempts.
//
// We intentionally do NOT retry 4xx — those are caller errors and retrying
// is wasteful. We also do NOT retry once the body has started streaming
// successfully, so this remains idempotent w.r.t. the upstream.
func (c *VLLMClient) doWithRetry(ctx context.Context, url string, jsonData []byte) (*VLLMResponse, error) {
	attempts := c.retryAttempts + 1
	if attempts < 1 {
		attempts = 1
	}

	var lastErr error
	var nextWait time.Duration // server-requested Retry-After from the prior attempt
	for attempt := 0; attempt < attempts; attempt++ {
		if attempt > 0 {
			// Honor a server-requested Retry-After if the previous attempt
			// surfaced one (e.g. 429); otherwise fall back to exponential
			// backoff with jitter, capped at 8s.
			var wait time.Duration
			if nextWait > 0 {
				wait = nextWait
				nextWait = 0
			} else {
				base := time.Duration(1<<uint(attempt-1)) * 200 * time.Millisecond
				if base > 8*time.Second {
					base = 8 * time.Second
				}
				jitter := time.Duration(rand.Int63n(int64(base / 2)))
				wait = base + jitter
			}
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(wait):
			}
			c.logger.WithFields(logrus.Fields{
				"attempt": attempt + 1,
				"wait":    wait,
			}).Debug("Retrying vLLM request")
		}

		resp, err := c.doOnceBreakered(ctx, url, jsonData)
		if err == nil {
			return resp, nil
		}
		lastErr = err

		// Capture Retry-After from the just-failed attempt for the next
		// iteration's wait. Only meaningful for retryableHTTPError; transport
		// errors don't carry one.
		var rerr2 *retryableHTTPError
		if errors.As(err, &rerr2) && rerr2.retryAfter > 0 {
			nextWait = rerr2.retryAfter
		}

		// gobreaker.ErrOpenState / ErrTooManyRequests are terminal: there is
		// no point retrying while the breaker is open. Surface the breaker
		// error directly so callers see a fast-fail. Pass-9 retry config at
		// the activity layer is responsible for retrying the whole request
		// later, by which time the breaker may have moved to half-open.
		if errors.Is(err, gobreaker.ErrOpenState) || errors.Is(err, gobreaker.ErrTooManyRequests) {
			return nil, err
		}

		var rerr *retryableHTTPError
		if !errors.As(err, &rerr) && !isTransportRetryable(err) {
			// Non-retryable (e.g. 4xx, malformed JSON, marshal error).
			return nil, err
		}
	}
	return nil, fmt.Errorf("vLLM request failed after %d attempts: %w", attempts, lastErr)
}

// doOnceBreakered wraps doOnce in c.breaker.Execute so repeated upstream
// failures fast-fail with gobreaker.ErrOpenState instead of grinding
// through the full retry loop × N agents × 3 minutes per call.
//
// We unbox the interface{} return because gobreaker v1's Execute uses
// untyped any. The breaker only sees errors, so the *VLLMResponse is
// boxed/unboxed but never inspected by gobreaker itself.
func (c *VLLMClient) doOnceBreakered(ctx context.Context, url string, jsonData []byte) (*VLLMResponse, error) {
	if c.breaker == nil {
		// Defensive: clients constructed via struct literal in older tests
		// might skip the constructor. Fall through to the raw call so we
		// don't NPE.
		return c.doOnce(ctx, url, jsonData)
	}
	out, err := c.breaker.Execute(func() (interface{}, error) {
		return c.doOnce(ctx, url, jsonData)
	})
	if err != nil {
		return nil, err
	}
	resp, ok := out.(*VLLMResponse)
	if !ok {
		return nil, fmt.Errorf("internal: breaker returned unexpected type %T", out)
	}
	return resp, nil
}

// isTransportRetryable returns true for transport-layer errors we believe a
// retry can recover (timeouts, EOF on read, etc.). We do NOT include
// context.Canceled / context.DeadlineExceeded — those mean the caller asked
// us to stop.
func isTransportRetryable(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	// net/http surfaces transport errors with these substrings most of the
	// time; we keep the check conservative.
	msg := err.Error()
	return strings.Contains(msg, "EOF") ||
		strings.Contains(msg, "connection reset") ||
		strings.Contains(msg, "connection refused") ||
		strings.Contains(msg, "i/o timeout")
}

// doOnce performs a single POST attempt and returns either a decoded
// VLLMResponse, a retryableHTTPError (5xx or 429), or a non-retryable error.
//
// Body bounds: both the success and error paths are capped via io.LimitReader.
// The success cap is maxLLMResponseSize (8 MiB), large enough for any sane
// chat-completion response; the error cap is errorBodyLimit (64 KiB), since
// error bodies are only used for log/diagnostic strings.
func (c *VLLMClient) doOnce(ctx context.Context, url string, jsonData []byte) (*VLLMResponse, error) {
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewBuffer(jsonData))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("vLLM request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		errBody, readErr := io.ReadAll(io.LimitReader(resp.Body, errorBodyLimit))
		if readErr != nil {
			c.logger.WithError(readErr).Warn("Failed to read vLLM error response body")
		}
		bodyStr := sanitizeBody(string(errBody))
		// 429 (rate limit) and 5xx are retryable. vLLM, OpenAI, and Anthropic
		// all use 429 to signal "back off" — treating it as terminal here
		// would burn the request on a transient condition. The Retry-After
		// header (RFC 7231 §7.1.3) is honored when present.
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
			ra := parseRetryAfter(resp.Header.Get("Retry-After"))
			return nil, &retryableHTTPError{status: resp.StatusCode, body: bodyStr, retryAfter: ra}
		}
		return nil, fmt.Errorf("vLLM returned status %d: %s", resp.StatusCode, bodyStr)
	}

	// Bound the success body so a misconfigured upstream cannot stream
	// gigabytes into the json decoder. We read +1 byte over the cap and
	// reject if the limit was reached.
	limited := io.LimitReader(resp.Body, int64(maxLLMResponseSize)+1)
	body, err := io.ReadAll(limited)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}
	if int64(len(body)) > int64(maxLLMResponseSize) {
		return nil, fmt.Errorf("%w: read %d bytes, limit %d", ErrResponseTooLarge, len(body), maxLLMResponseSize)
	}
	var vllmResp VLLMResponse
	if err := json.Unmarshal(body, &vllmResp); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}
	return &vllmResp, nil
}

func (c *VLLMClient) Health(ctx context.Context) error {
	url := fmt.Sprintf("%s/health", c.baseURL)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return err
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("health check returned status %d", resp.StatusCode)
	}

	return nil
}

func (c *VLLMClient) buildAgentSystemPrompt(agentType types.AgentType) string {
	basePrompt := `You are a specialized code review AI agent. Analyze the provided code diff and respond in the exact format specified.

CRITICAL: Your response must start with these exact lines:
VERDICT: [PASS/FAIL/WARNING]
CONFIDENCE: [0.0-1.0]
SEVERITY: [LOW/MEDIUM/HIGH/CRITICAL]
RATIONALE: [One sentence summary]
DETAILS: [Specific issues, one per line]

`

	switch agentType {
	case types.AgentSecurity:
		return basePrompt + `You are a SECURITY EXPERT. Focus on:
- SQL injection, XSS, CSRF vulnerabilities
- Authentication and authorization flaws
- Input validation and sanitization
- Cryptographic issues and key management
- Buffer overflows and memory safety
- Privilege escalation risks
- Data exposure and information leaks`

	case types.AgentPerformance:
		return basePrompt + `You are a PERFORMANCE EXPERT. Focus on:
- Algorithmic complexity (O(n²), etc.)
- Memory allocation and leaks
- Database query optimization
- Caching opportunities
- Resource utilization inefficiencies
- Blocking operations and concurrency
- Network and I/O optimization`

	case types.AgentArchitecture:
		return basePrompt + `You are an ARCHITECTURE EXPERT. Focus on:
- SOLID principles violations
- Design pattern usage and anti-patterns
- Code organization and modularity
- Dependency management and coupling
- Interface design and abstraction
- Separation of concerns
- Scalability and maintainability`

	case types.AgentTesting:
		return basePrompt + `You are a TESTING EXPERT. Focus on:
- Test coverage gaps
- Test quality and effectiveness
- Edge case handling
- Integration test needs
- Mocking and test isolation
- Test maintainability
- Performance test requirements`

	case types.AgentCompliance:
		return basePrompt + `You are a COMPLIANCE EXPERT. Focus on:
- GDPR, HIPAA, PCI DSS compliance
- Data privacy and protection
- Regulatory requirements
- Audit trail and logging
- Data retention policies
- Cross-border data transfer
- Industry-specific regulations`

	case types.AgentAccessibility:
		return basePrompt + `You are an ACCESSIBILITY EXPERT. Focus on:
- WCAG 2.1 compliance
- Screen reader compatibility
- Keyboard navigation
- Color contrast and visual design
- Alt text and semantic markup
- Focus management
- Inclusive design principles`

	default:
		return basePrompt + fmt.Sprintf(`You are a %s EXPERT. Analyze the code for issues related to your specialization.`, agentType)
	}
}

// recognized headers used by parseAgentResponse to detect when DETAILS
// continuation has ended. If a line later in the response starts with one
// of these prefixes (case-insensitive), continuation collection stops.
var responseHeaders = []string{"VERDICT:", "CONFIDENCE:", "SEVERITY:", "RATIONALE:", "DETAILS:"}

func startsWithRecognizedHeader(upper string) bool {
	for _, h := range responseHeaders {
		if strings.HasPrefix(upper, h) {
			return true
		}
	}
	return false
}

func (c *VLLMClient) parseAgentResponse(response string, agentType types.AgentType) types.LLMCheckResult {
	lines := strings.Split(strings.TrimSpace(response), "\n")

	result := types.LLMCheckResult{
		Agent:      string(agentType),
		Verdict:    string(types.VerdictFail),
		Confidence: 0.5,
		Severity:   string(types.SeverityMedium),
		Category:   strings.ToUpper(string(agentType)),
		Details:    make([]string, 0),
	}

	if len(lines) == 0 || (len(lines) == 1 && strings.TrimSpace(lines[0]) == "") {
		result.Rationale = "Empty response from LLM"
		return result
	}

	seenDetails := false
	for _, raw := range lines {
		line := strings.TrimSpace(raw)
		if line == "" {
			// A blank line ends DETAILS continuation collection: any text
			// that follows must be re-introduced by another header.
			seenDetails = false
			continue
		}

		upper := strings.ToUpper(line)
		switch {
		case strings.HasPrefix(upper, "VERDICT:"):
			seenDetails = false
			verdict := strings.ToUpper(strings.TrimSpace(line[len("VERDICT:"):]))
			switch {
			case strings.Contains(verdict, "PASS"):
				result.Verdict = string(types.VerdictPass)
			case strings.Contains(verdict, "WARNING"):
				result.Verdict = string(types.VerdictWarning)
			case strings.Contains(verdict, "FAIL"):
				result.Verdict = string(types.VerdictFail)
			}
		case strings.HasPrefix(upper, "CONFIDENCE:"):
			seenDetails = false
			confidenceStr := strings.TrimSpace(line[len("CONFIDENCE:"):])
			if conf, err := strconv.ParseFloat(confidenceStr, 64); err == nil {
				if conf >= 0.0 && conf <= 1.0 {
					result.Confidence = conf
				} else {
					// Clamp out-of-range to the nearest endpoint instead of
					// silently dropping, then warn so misbehaving models
					// still leave a trace.
					if conf < 0.0 {
						result.Confidence = 0.0
					} else {
						result.Confidence = 1.0
					}
					c.logger.WithField("confidence", conf).Warn("LLM returned out-of-range confidence, clamped to [0,1]")
				}
			}
		case strings.HasPrefix(upper, "SEVERITY:"):
			seenDetails = false
			severity := strings.ToUpper(strings.TrimSpace(line[len("SEVERITY:"):]))
			switch severity {
			case string(types.SeverityLow), string(types.SeverityMedium), string(types.SeverityHigh), string(types.SeverityCritical):
				result.Severity = severity
			}
			// Unknown severity values are silently kept at their default
			// (MEDIUM) — the test suite asserts this behavior.
		case strings.HasPrefix(upper, "RATIONALE:"):
			seenDetails = false
			result.Rationale = strings.TrimSpace(line[len("RATIONALE:"):])
		case strings.HasPrefix(upper, "DETAILS:"):
			seenDetails = true
			detail := strings.TrimSpace(line[len("DETAILS:"):])
			if detail != "" {
				result.Details = append(result.Details, detail)
			}
		default:
			// Continuation of DETAILS: only collect well-formed lines. If
			// another recognized header re-appears we will have already
			// taken one of the cases above.
			if seenDetails && !startsWithRecognizedHeader(upper) {
				result.Details = append(result.Details, line)
			}
		}
	}

	if result.Rationale == "" {
		result.Rationale = "No specific issues identified"
	}

	return result
}
