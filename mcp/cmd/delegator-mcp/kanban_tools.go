package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sirupsen/logrus"
)

// Kanban tools. These are thin REST clients of the apiserver kanban API
// (the same evo-schema board/column/card store the dashboard and the
// platform endpoints share). The MCP server doesn't touch postgres for
// kanban — it speaks HTTP to the apiserver so the claim/move/complete
// atomicity (pg_advisory_xact_lock, claim_token matching) lives in one
// place. Base URL comes from KANBAN_API_URL (default http://localhost:9123).

// kanbanClient is a tiny HTTP wrapper around the apiserver kanban API.
type kanbanClient struct {
	base string
	http *http.Client
}

// Per-call deadline and retry tuning. The MCP request ctx (threaded from the
// tool handler) is the outer bound; perCallTimeout caps any single attempt so
// one wedged apiserver call can't hang the whole MCP tool invocation
// indefinitely even if the client's ctx has no deadline. The retry budget is
// intentionally small — these are interactive tool calls, not a background
// job, so we'd rather surface a transient error quickly than stack seconds of
// backoff in front of the operator.
const (
	perCallTimeout = 15 * time.Second
	maxRetries     = 3 // total attempts = maxRetries (1 initial + up to 2 retries)
	baseBackoff    = 100 * time.Millisecond
	maxBackoff     = 2 * time.Second
)

func newKanbanClient() *kanbanClient {
	base := os.Getenv("KANBAN_API_URL")
	if base == "" {
		base = "http://localhost:9123"
	}
	base = strings.TrimRight(base, "/")
	return &kanbanClient{
		base: base,
		http: &http.Client{Timeout: 30 * time.Second},
	}
}

// do issues a request to <base>/api/v1<path>. When body is non-nil it is
// JSON-encoded. On a non-2xx response the decoded body is returned as an
// error so the caller can surface the apiserver's message (incl. 409
// claim conflicts) verbatim. On success the raw JSON body is returned.
//
// The caller's ctx (the MCP request context) is honored and additionally
// bounded by perCallTimeout per attempt. Idempotent requests (GET) are retried
// with bounded exponential backoff + jitter on transient failures — network
// errors and 5xx responses — so a momentary apiserver blip or restart doesn't
// surface as a hard tool failure. Non-idempotent requests (POST: claim, move,
// complete, release, ...) are NOT retried: replaying them could double-apply a
// mutation, so a transient error is returned to the caller as-is.
func (c *kanbanClient) do(ctx context.Context, method, path string, body any) (json.RawMessage, error) {
	var payload []byte
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("marshal request: %w", err)
		}
		payload = b
	}

	retryable := isIdempotent(method)

	var lastErr error
	for attempt := 0; attempt < maxRetries; attempt++ {
		if attempt > 0 {
			// Back off before retrying. Honor ctx cancellation while waiting.
			if err := sleepWithContext(ctx, backoffFor(attempt)); err != nil {
				return nil, err
			}
		}

		raw, retry, err := c.doOnce(ctx, method, path, payload, body != nil)
		if err == nil {
			return raw, nil
		}
		lastErr = err
		// Stop early on a non-retryable outcome (4xx, marshal/build error,
		// ctx cancellation) or when the method isn't safe to retry.
		if !retry || !retryable {
			return nil, err
		}
	}
	return nil, lastErr
}

// doOnce performs a single HTTP attempt. The returned retry flag is true only
// for transient failures (network error or 5xx) that a caller may retry; it is
// false for context cancellation, request-build failures, and 4xx responses
// (including 409 claim conflicts, which are a definitive answer, not a blip).
func (c *kanbanClient) doOnce(ctx context.Context, method, path string, payload []byte, hasBody bool) (json.RawMessage, bool, error) {
	attemptCtx, cancel := context.WithTimeout(ctx, perCallTimeout)
	defer cancel()

	var reader io.Reader
	if hasBody {
		reader = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(attemptCtx, method, c.base+"/api/v1"+path, reader)
	if err != nil {
		return nil, false, fmt.Errorf("build request: %w", err)
	}
	if hasBody {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		// If the caller's ctx (not just our per-attempt deadline) is done,
		// don't treat it as a retryable blip — the caller is giving up.
		if ctx.Err() != nil {
			return nil, false, fmt.Errorf("call %s %s: %w", method, path, ctx.Err())
		}
		// Network/transport error (connection refused during a restart, EOF,
		// per-attempt timeout): retryable.
		return nil, true, fmt.Errorf("call %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		if ctx.Err() != nil {
			return nil, false, fmt.Errorf("read response: %w", ctx.Err())
		}
		return nil, true, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg := strings.TrimSpace(string(raw))
		if msg == "" {
			msg = resp.Status
		}
		retry := resp.StatusCode >= 500 // 5xx is transient; 4xx (incl. 409) is definitive.
		return nil, retry, fmt.Errorf("kanban api %s %s -> %d: %s", method, path, resp.StatusCode, msg)
	}
	return json.RawMessage(raw), false, nil
}

// isIdempotent reports whether a method is safe to retry without risking a
// double-applied mutation. Only GET (and HEAD, for completeness) qualify; the
// kanban/project mutations are all POST and must not be replayed.
func isIdempotent(method string) bool {
	return method == http.MethodGet || method == http.MethodHead
}

// backoffFor returns the exponential backoff for the given attempt (1-based for
// the first retry) with full jitter, clamped to maxBackoff. Full jitter spreads
// retries from concurrent MCP clients so they don't synchronize into a thundering
// herd against a recovering apiserver.
func backoffFor(attempt int) time.Duration {
	d := baseBackoff << (attempt - 1) // attempt 1 -> base, 2 -> 2*base, ...
	if d > maxBackoff || d <= 0 {
		d = maxBackoff
	}
	// Full jitter in [0, d].
	return time.Duration(rand.Int63n(int64(d) + 1))
}

// sleepWithContext sleeps for d or until ctx is done, whichever comes first.
// Returns ctx.Err() if the context was cancelled during the wait.
func sleepWithContext(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// --- tool argument types ---

type kanbanCreateBoardArgs struct {
	ProjectID string `json:"project_id" jsonschema:"the project ID to create the board under"`
	Name      string `json:"name" jsonschema:"the board name"`
}

type kanbanListBoardsArgs struct {
	ProjectID string `json:"project_id" jsonschema:"the project ID whose boards to list"`
}

type kanbanAddCardArgs struct {
	BoardID    string `json:"board_id" jsonschema:"the board ID to add the card to"`
	Title      string `json:"title" jsonschema:"the card title (required)"`
	Body       string `json:"body,omitempty" jsonschema:"the card body / description"`
	ColumnID   string `json:"column_id,omitempty" jsonschema:"target column ID; defaults to the Backlog column"`
	Priority   string `json:"priority,omitempty" jsonschema:"card priority (default 'normal')"`
	Difficulty string `json:"difficulty,omitempty" jsonschema:"card difficulty (default 'medium')"`
}

type kanbanListReadyArgs struct {
	BoardID string `json:"board_id" jsonschema:"the board ID whose ready (unclaimed) cards to list"`
}

type kanbanGetBoardArgs struct {
	BoardID string `json:"board_id" jsonschema:"the board ID to fetch in full (all columns + every card)"`
}

type kanbanClaimCardArgs struct {
	CardID  string `json:"card_id" jsonschema:"the card ID to claim"`
	AgentID string `json:"agent_id" jsonschema:"the agent ID claiming the card; returned claim_token is required for later move/complete/release"`
}

type kanbanMoveCardArgs struct {
	CardID         string `json:"card_id" jsonschema:"the card ID to move"`
	TargetColumnID string `json:"target_column_id" jsonschema:"the column ID to move the card into"`
	ClaimToken     string `json:"claim_token,omitempty" jsonschema:"required if the card is claimed; must match the token from kanban_claim_card"`
	Position       *int   `json:"position,omitempty" jsonschema:"optional position within the target column"`
}

type kanbanCompleteCardArgs struct {
	CardID        string `json:"card_id" jsonschema:"the card ID to complete"`
	ClaimToken    string `json:"claim_token" jsonschema:"the claim token from kanban_claim_card; must match"`
	ResultSummary string `json:"result_summary,omitempty" jsonschema:"short summary of the completed work"`
	ResultTaskID  string `json:"result_task_id,omitempty" jsonschema:"the delegate_task ID that produced the result, if any"`
	ResultPRURL   string `json:"result_pr_url,omitempty" jsonschema:"the PR URL for the completed work, if any"`
}

type kanbanReleaseCardArgs struct {
	CardID     string `json:"card_id" jsonschema:"the card ID to release"`
	ClaimToken string `json:"claim_token" jsonschema:"the claim token from kanban_claim_card; must match"`
}

type kanbanAddCommentArgs struct {
	CardID string `json:"card_id" jsonschema:"the card ID to comment on"`
	Author string `json:"author" jsonschema:"the comment author"`
	Text   string `json:"text" jsonschema:"the comment text"`
}

// --- tool registrations ---

func registerKanbanTools(server *mcp.Server, logger *logrus.Logger) {
	client := newKanbanClient()

	mcp.AddTool(server, &mcp.Tool{
		Name:        "kanban_create_board",
		Description: "Create a kanban board under a project. Seeds 5 columns (Backlog, Ready, In Progress, Review, Done). Returns {board, columns}.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args kanbanCreateBoardArgs) (*mcp.CallToolResult, any, error) {
		if args.ProjectID == "" {
			return errResult("project_id is required"), nil, nil
		}
		if args.Name == "" {
			return errResult("name is required"), nil, nil
		}
		raw, err := client.do(ctx, http.MethodPost,
			fmt.Sprintf("/projects/%s/kanban/boards", url.PathEscape(args.ProjectID)),
			map[string]any{"name": args.Name})
		if err != nil {
			return errResult(err.Error()), nil, nil
		}
		return jsonResult(raw), nil, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "kanban_list_boards",
		Description: "List all kanban boards for a project. Returns Board[].",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args kanbanListBoardsArgs) (*mcp.CallToolResult, any, error) {
		if args.ProjectID == "" {
			return errResult("project_id is required"), nil, nil
		}
		raw, err := client.do(ctx, http.MethodGet,
			fmt.Sprintf("/projects/%s/kanban/boards", url.PathEscape(args.ProjectID)), nil)
		if err != nil {
			return errResult(err.Error()), nil, nil
		}
		return jsonResult(raw), nil, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "kanban_add_card",
		Description: "Add a card to a kanban board. Defaults to the Backlog column, priority 'normal', difficulty 'medium'. Returns the created Card.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args kanbanAddCardArgs) (*mcp.CallToolResult, any, error) {
		if args.BoardID == "" {
			return errResult("board_id is required"), nil, nil
		}
		if args.Title == "" {
			return errResult("title is required"), nil, nil
		}
		body := map[string]any{"title": args.Title}
		if args.Body != "" {
			body["body"] = args.Body
		}
		if args.ColumnID != "" {
			body["column_id"] = args.ColumnID
		}
		if args.Priority != "" {
			body["priority"] = args.Priority
		}
		if args.Difficulty != "" {
			body["difficulty"] = args.Difficulty
		}
		raw, err := client.do(ctx, http.MethodPost,
			fmt.Sprintf("/kanban/boards/%s/cards", url.PathEscape(args.BoardID)), body)
		if err != nil {
			return errResult(err.Error()), nil, nil
		}
		return jsonResult(raw), nil, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "kanban_list_ready_cards",
		Description: "List unclaimed cards in the board's Ready column — the queue of work agents can claim. Returns Card[].",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args kanbanListReadyArgs) (*mcp.CallToolResult, any, error) {
		if args.BoardID == "" {
			return errResult("board_id is required"), nil, nil
		}
		raw, err := client.do(ctx, http.MethodGet,
			fmt.Sprintf("/kanban/boards/%s/ready", url.PathEscape(args.BoardID)), nil)
		if err != nil {
			return errResult(err.Error()), nil, nil
		}
		return jsonResult(raw), nil, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "kanban_get_board",
		Description: "Fetch a board in full: the board plus ALL its columns and every card in each (Backlog, Ready, In Progress, Review, Done). Use this to see cards that kanban_list_ready_cards omits (it returns only the unclaimed Ready queue) — e.g. to dedup before adding cards or to review Backlog/In-Progress/Done. Returns {board, columns:[{...,cards:[...]}]}.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args kanbanGetBoardArgs) (*mcp.CallToolResult, any, error) {
		if args.BoardID == "" {
			return errResult("board_id is required"), nil, nil
		}
		raw, err := client.do(ctx, http.MethodGet,
			fmt.Sprintf("/kanban/boards/%s", url.PathEscape(args.BoardID)), nil)
		if err != nil {
			return errResult(err.Error()), nil, nil
		}
		return jsonResult(raw), nil, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "kanban_claim_card",
		Description: "Atomically claim a ready card for an agent. Returns {card, claim_token} or an error if the card is already claimed (409). The claim_token is required for later move/complete/release.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args kanbanClaimCardArgs) (*mcp.CallToolResult, any, error) {
		if args.CardID == "" {
			return errResult("card_id is required"), nil, nil
		}
		if args.AgentID == "" {
			return errResult("agent_id is required"), nil, nil
		}
		raw, err := client.do(ctx, http.MethodPost,
			fmt.Sprintf("/kanban/cards/%s/claim", url.PathEscape(args.CardID)),
			map[string]any{"agent_id": args.AgentID})
		if err != nil {
			return errResult(err.Error()), nil, nil
		}
		return jsonResult(raw), nil, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "kanban_move_card",
		Description: "Move a card to another column. If the card is claimed, claim_token must match. Returns the updated Card.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args kanbanMoveCardArgs) (*mcp.CallToolResult, any, error) {
		if args.CardID == "" {
			return errResult("card_id is required"), nil, nil
		}
		if args.TargetColumnID == "" {
			return errResult("target_column_id is required"), nil, nil
		}
		body := map[string]any{"target_column_id": args.TargetColumnID}
		if args.ClaimToken != "" {
			body["claim_token"] = args.ClaimToken
		}
		if args.Position != nil {
			body["position"] = *args.Position
		}
		raw, err := client.do(ctx, http.MethodPost,
			fmt.Sprintf("/kanban/cards/%s/move", url.PathEscape(args.CardID)), body)
		if err != nil {
			return errResult(err.Error()), nil, nil
		}
		return jsonResult(raw), nil, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "kanban_complete_card",
		Description: "Mark a claimed card done and move it to the Done column. claim_token must match. Optionally records result_summary, result_task_id, result_pr_url. Returns the updated Card.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args kanbanCompleteCardArgs) (*mcp.CallToolResult, any, error) {
		if args.CardID == "" {
			return errResult("card_id is required"), nil, nil
		}
		if args.ClaimToken == "" {
			return errResult("claim_token is required"), nil, nil
		}
		body := map[string]any{"claim_token": args.ClaimToken}
		if args.ResultSummary != "" {
			body["result_summary"] = args.ResultSummary
		}
		if args.ResultTaskID != "" {
			body["result_task_id"] = args.ResultTaskID
		}
		if args.ResultPRURL != "" {
			body["result_pr_url"] = args.ResultPRURL
		}
		raw, err := client.do(ctx, http.MethodPost,
			fmt.Sprintf("/kanban/cards/%s/complete", url.PathEscape(args.CardID)), body)
		if err != nil {
			return errResult(err.Error()), nil, nil
		}
		return jsonResult(raw), nil, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "kanban_release_card",
		Description: "Release a claimed card back to unclaimed (clears claimed_by/claim_token/claimed_at). claim_token must match. Returns the updated Card.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args kanbanReleaseCardArgs) (*mcp.CallToolResult, any, error) {
		if args.CardID == "" {
			return errResult("card_id is required"), nil, nil
		}
		if args.ClaimToken == "" {
			return errResult("claim_token is required"), nil, nil
		}
		raw, err := client.do(ctx, http.MethodPost,
			fmt.Sprintf("/kanban/cards/%s/release", url.PathEscape(args.CardID)),
			map[string]any{"claim_token": args.ClaimToken})
		if err != nil {
			return errResult(err.Error()), nil, nil
		}
		return jsonResult(raw), nil, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "kanban_add_comment",
		Description: "Add a comment to a card. Returns the created Comment.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args kanbanAddCommentArgs) (*mcp.CallToolResult, any, error) {
		if args.CardID == "" {
			return errResult("card_id is required"), nil, nil
		}
		if args.Author == "" {
			return errResult("author is required"), nil, nil
		}
		if args.Text == "" {
			return errResult("text is required"), nil, nil
		}
		raw, err := client.do(ctx, http.MethodPost,
			fmt.Sprintf("/kanban/cards/%s/comment", url.PathEscape(args.CardID)),
			map[string]any{"author": args.Author, "text": args.Text})
		if err != nil {
			return errResult(err.Error()), nil, nil
		}
		return jsonResult(raw), nil, nil
	})

	_ = logger
}
