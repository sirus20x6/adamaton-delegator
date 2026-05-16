package contextmode

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"time"

	"github.com/sirupsen/logrus"
)

// rerankTimeout bounds a single rerank call. BGE-Reranker-v2-m3 on CPU
// runs ~1.76s for 50 pairs (per deepresearch's benches); 30s leaves
// generous headroom for a slow box or a backlog of pending requests
// without ever holding the MCP caller hostage.
const rerankTimeout = 30 * time.Second

// RerankerClient wraps the deepresearch sidecar's /v1/rerank endpoint.
// When BaseURL is empty, every method returns nil + nil — contextmode
// reads that as "no reranker configured" and falls back to the
// pre-Phase-4 LLM-compress stage 3.
type RerankerClient struct {
	BaseURL string
	Model   string
	HTTP    *http.Client
	Logger  *logrus.Logger
}

// NewRerankerClient builds a client. baseURL="" returns a disabled
// client.
func NewRerankerClient(baseURL, model string, logger *logrus.Logger) *RerankerClient {
	return &RerankerClient{
		BaseURL: baseURL,
		Model:   model,
		HTTP:    &http.Client{Timeout: rerankTimeout},
		Logger:  logger,
	}
}

// Enabled reports whether the client has a backing URL.
func (c *RerankerClient) Enabled() bool {
	return c != nil && c.BaseURL != ""
}

// RerankResult is one (index, score) pair from the reranker. Index
// is the position of the document in the input list.
type RerankResult struct {
	Index int
	Score float64
}

type rerankReq struct {
	Model     string   `json:"model,omitempty"`
	Query     string   `json:"query"`
	Documents []string `json:"documents"`
	TopK      int      `json:"top_k,omitempty"`
}

type rerankRespItem struct {
	Index int     `json:"index"`
	Score float64 `json:"score"`
}

type rerankResp struct {
	Results []rerankRespItem `json:"results"`
}

// Rerank scores each document against the query and returns at most
// topK results sorted by descending score. topK <= 0 means "return
// all". Returns nil + nil when the client is disabled.
func (c *RerankerClient) Rerank(ctx context.Context, query string, documents []string, topK int) ([]RerankResult, error) {
	if !c.Enabled() {
		return nil, nil
	}
	if len(documents) == 0 {
		return nil, nil
	}

	body, err := json.Marshal(rerankReq{
		Model:     c.Model,
		Query:     query,
		Documents: documents,
		TopK:      topK,
	})
	if err != nil {
		return nil, fmt.Errorf("reranker: marshal: %w", err)
	}

	reqCtx, cancel := context.WithTimeout(ctx, rerankTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost,
		c.BaseURL+"/v1/rerank", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("reranker: build req: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("reranker: do: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("reranker: HTTP %d", resp.StatusCode)
	}

	var parsed rerankResp
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, fmt.Errorf("reranker: decode: %w", err)
	}

	out := make([]RerankResult, 0, len(parsed.Results))
	for _, r := range parsed.Results {
		if r.Index < 0 || r.Index >= len(documents) {
			return nil, fmt.Errorf("reranker: index %d out of range", r.Index)
		}
		out = append(out, RerankResult{Index: r.Index, Score: r.Score})
	}
	// Server-side ordering is not guaranteed across implementations; sort
	// descending by score so the caller can rely on out[0] being best.
	sort.SliceStable(out, func(i, j int) bool { return out[i].Score > out[j].Score })

	if topK > 0 && len(out) > topK {
		out = out[:topK]
	}
	if len(out) == 0 {
		return nil, errors.New("reranker: empty result")
	}
	return out, nil
}
