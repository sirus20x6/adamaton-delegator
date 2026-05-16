package main

import (
	"context"
	"os"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sirupsen/logrus"

	"github.com/sirus20x6/adamaton-core/octen"
	"github.com/sirus20x6/adamaton-delegator/delegator"
	"github.com/sirus20x6/adamaton-delegator/delegator/contextmode"
)

// Context-mode tools. The model writes a script (or fetches a URL),
// Go runs it, and big outputs go into pg_search (Tantivy-backed BM25
// inside postgres) with the ngram(3,3) tokenizer on content for
// substring matching. The LLM only sees raw bytes that already fit
// in the threshold OR exact indexed snippets cropped around its
// stated intent.
//
// This sits next to delegate_task, which keeps the full agent-loop
// path intact. The new tools are for "I know what I want, just run
// the analysis and give me the answer" workflows where dropping 47
// Read calls into one execute() is the win.

type executeArgs struct {
	Script     string `json:"script" jsonschema:"the script body to execute (bash by default; set language= for others)"`
	Language   string `json:"language,omitempty" jsonschema:"runtime: bash|python|node|go (default: bash, or detect via shebang)"`
	Intent     string `json:"intent,omitempty" jsonschema:"what you're looking for in the output. When set + output exceeds the size threshold, the result is indexed and cropped via BM25 to just the relevant matches. Skip when you want the whole output."`
	WorkingDir string `json:"working_dir,omitempty" jsonschema:"working directory for the subprocess (defaults to caller's cwd)"`
	TimeoutSec int    `json:"timeout_seconds,omitempty" jsonschema:"hard timeout in seconds (default 60)"`
}

type fetchAndIndexArgs struct {
	URL    string `json:"url" jsonschema:"absolute URL to fetch (http/https only)"`
	Intent string `json:"intent,omitempty" jsonschema:"what to extract. When set + page exceeds the threshold, returns BM25-cropped snippets instead of the full page."`
}

type searchArgs struct {
	Query string `json:"query" jsonschema:"search query (whitespace-separated terms; the ngram(3,3) tokenizer means substring matches work — 'useEff' hits 'useEffect')"`
	TopK  int    `json:"top_k,omitempty" jsonschema:"max snippets to return (default 10)"`
}

func registerContextTools(server *mcp.Server, orch *delegator.Orchestrator, dsn string, logger *logrus.Logger) {
	if dsn == "" {
		logger.Warn("contextmode: postgres DSN not configured; skipping registration")
		return
	}
	idx, err := contextmode.NewIndex(dsn, logger)
	if err != nil {
		logger.WithError(err).Warn("contextmode: failed to open index; skipping registration")
		return
	}
	// Optional dense-retrieval upgrade via the octen sidecar
	// (deepresearch convention: $OCTEN_SIDECAR_URL points at the
	// /v1/embeddings host). When unset, the cascade falls back to the
	// pre-Phase-3 qwen-translate path and never tries to embed.
	if url := os.Getenv("OCTEN_SIDECAR_URL"); url != "" {
		model := os.Getenv("OCTEN_EMBED_MODEL")
		idx.SetOcten(octen.NewClient(url, model, logger))
		logger.WithField("url", url).Info("contextmode: octen dense retrieval enabled")
	}
	// The index owns a pgxpool that lives for the process lifetime; we
	// don't defer Close here because registration returns. The OS reaps
	// connections at exit and pg will reap any leftover server-side
	// state via idle-connection timeout.
	_ = idx

	comp := contextmode.Compressor(contextmode.NoopCompressor{})
	if orch != nil {
		comp = &contextmode.OpencodeCompressor{Orch: orch}
	}
	svc := contextmode.NewService(idx, comp, logger)

	// Optional stage-3 upgrade: BGE-Reranker (deepresearch
	// convention: $BGE_SIDECAR_URL points at the /v1/rerank host).
	// When configured, the cascade reranks the source's chunks
	// against intent instead of asking qwen to paraphrase the raw
	// bytes. Empty → pre-Phase-4 LLM-compress behaviour.
	if url := os.Getenv("BGE_SIDECAR_URL"); url != "" {
		model := os.Getenv("BGE_RERANK_MODEL")
		svc.SetReranker(contextmode.NewRerankerClient(url, model, logger))
		logger.WithField("url", url).Info("contextmode: BGE reranker stage-3 enabled")
	}

	mcp.AddTool(server, &mcp.Tool{
		Name: "execute",
		Description: "Execute a script (bash/python/node/go) in a subprocess and return its output. Compression happens via pg_search (Tantivy-backed BM25 inside postgres): outputs under ~5KB return raw; bigger outputs are indexed and, when an `intent` is provided, return BM25-ranked snippets cropped around matches (exact bytes, no LLM paraphrasing). One execute() that runs `grep -c func **/*.go` replaces 47 individual file Reads. PREFER over native Bash + Read combinations when you want analysis-of-many-files results.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args executeArgs) (*mcp.CallToolResult, any, error) {
		if args.Script == "" {
			return errResult("script is required"), nil, nil
		}
		if err := validateWorkingDir(args.WorkingDir); err != nil {
			return errResult(err.Error()), nil, nil
		}
		res, err := svc.Execute(ctx, contextmode.ExecuteRequest{
			Script:     args.Script,
			Language:   args.Language,
			Intent:     args.Intent,
			WorkingDir: args.WorkingDir,
			TimeoutSec: clampTimeoutCapOnly(args.TimeoutSec),
		})
		if err != nil {
			return errResult(err.Error()), nil, nil
		}
		return jsonResult(res), nil, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name: "fetch_and_index",
		Description: "Fetch a URL, strip nav/footer/ads, convert to clean markdown-ish text, and index it. Returns the full text if it's small; returns BM25-ranked snippets if big and an `intent` is provided. The model gets exact indexed bytes (never paraphrased). PREFER over WebFetch when you want to drill into a specific aspect of a long page (\"how does ctx_execute handle stdin?\") rather than read the whole thing.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args fetchAndIndexArgs) (*mcp.CallToolResult, any, error) {
		if args.URL == "" {
			return errResult("url is required"), nil, nil
		}
		res, err := svc.FetchAndIndex(ctx, contextmode.FetchAndIndexRequest{
			URL:    args.URL,
			Intent: args.Intent,
		})
		if err != nil {
			return errResult(err.Error()), nil, nil
		}
		return jsonResult(res), nil, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name: "search",
		Description: "Query the BM25 index built by previous execute() and fetch_and_index() calls. Returns ranked snippets across ALL indexed sources, with exact bytes cropped around matches. Use this for follow-up exploration after a big script ran without an intent — the source_id reported in the original call is the way to scope, but search() is global so cross-source queries work too.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, args searchArgs) (*mcp.CallToolResult, any, error) {
		if args.Query == "" {
			return errResult("query is required"), nil, nil
		}
		snippets, err := svc.Search(args.Query, args.TopK)
		if err != nil {
			return errResult(err.Error()), nil, nil
		}
		return jsonResult(map[string]any{
			"query":    args.Query,
			"snippets": snippets,
		}), nil, nil
	})
}

