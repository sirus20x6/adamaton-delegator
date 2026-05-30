package main

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sirupsen/logrus"
)

// Project tools. Thin REST clients of the apiserver projects API (the same
// evo.projects registry the dashboard uses). They let an orchestrator discover
// registered projects and their host, register new ones, and browse files —
// which is what feeds the project_id into the kanban_* tools. Files/tree for a
// project on a remote host are served transparently (the apiserver proxies to
// that host's deploy-agent). Reuses the apiserver base URL from KANBAN_API_URL
// (default http://localhost:9123) via the shared client.

type projectGetArgs struct {
	ProjectID string `json:"project_id" jsonschema:"the project ID"`
}

type projectRegisterArgs struct {
	Path        string `json:"path" jsonschema:"absolute host path to register (a git repo, worktree, submodule, or folder)"`
	Host        string `json:"host,omitempty" jsonschema:"host the path lives on (see project_hosts); defaults to the apiserver's local host"`
	DisplayName string `json:"display_name,omitempty" jsonschema:"optional display name; defaults to the folder basename"`
}

type projectTreeArgs struct {
	ProjectID string `json:"project_id" jsonschema:"the project ID"`
	Path      string `json:"path,omitempty" jsonschema:"path relative to the project root; empty = the root"`
	Depth     int    `json:"depth,omitempty" jsonschema:"directory depth to return (default 1, max 3)"`
}

type projectFileArgs struct {
	ProjectID string `json:"project_id" jsonschema:"the project ID"`
	Path      string `json:"path" jsonschema:"path to the file, relative to the project root"`
}

func registerProjectTools(server *mcp.Server, logger *logrus.Logger) {
	client := newKanbanClient() // generic apiserver client (/api/v1 + path)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "project_list",
		Description: "List registered projects (the dashboard's project registry). Returns Project[] with id, path, display_name, type, host, git_remote. Use a project's id with the kanban_* and other project_* tools.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
		raw, err := client.do(ctx, http.MethodGet, "/projects", nil)
		if err != nil {
			return errResult(err.Error()), nil, nil
		}
		return jsonResult(raw), nil, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "project_get",
		Description: "Get a single registered project by id. Returns the Project (id, path, display_name, type, host, git_remote).",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args projectGetArgs) (*mcp.CallToolResult, any, error) {
		if args.ProjectID == "" {
			return errResult("project_id is required"), nil, nil
		}
		raw, err := client.do(ctx, http.MethodGet, "/projects/"+url.PathEscape(args.ProjectID), nil)
		if err != nil {
			return errResult(err.Error()), nil, nil
		}
		return jsonResult(raw), nil, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "project_hosts",
		Description: "List the hosts a project can be registered on (the local host plus every host with a deploy-agent, deduped by physical machine). Returns string[].",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
		raw, err := client.do(ctx, http.MethodGet, "/projects/hosts", nil)
		if err != nil {
			return errResult(err.Error()), nil, nil
		}
		return jsonResult(raw), nil, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "project_register",
		Description: "Register a folder as a project. path is an absolute host path; host (see project_hosts) selects which machine it lives on and defaults to local. The folder type and git remote are auto-detected. Returns the created Project.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args projectRegisterArgs) (*mcp.CallToolResult, any, error) {
		if args.Path == "" {
			return errResult("path is required"), nil, nil
		}
		body := map[string]any{"path": args.Path}
		if args.Host != "" {
			body["host"] = args.Host
		}
		if args.DisplayName != "" {
			body["display_name"] = args.DisplayName
		}
		raw, err := client.do(ctx, http.MethodPost, "/projects", body)
		if err != nil {
			return errResult(err.Error()), nil, nil
		}
		return jsonResult(raw), nil, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "project_tree",
		Description: "List a directory in a project (lazy, one level by default). Works across hosts — a project on a remote host is served via that host's deploy-agent. Returns FileNode[] {name, path, is_dir, size}.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args projectTreeArgs) (*mcp.CallToolResult, any, error) {
		if args.ProjectID == "" {
			return errResult("project_id is required"), nil, nil
		}
		q := url.Values{}
		q.Set("path", args.Path)
		if args.Depth > 0 {
			q.Set("depth", strconv.Itoa(args.Depth))
		}
		raw, err := client.do(ctx, http.MethodGet,
			fmt.Sprintf("/projects/%s/tree?%s", url.PathEscape(args.ProjectID), q.Encode()), nil)
		if err != nil {
			return errResult(err.Error()), nil, nil
		}
		return jsonResult(raw), nil, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "project_file",
		Description: "Read a file from a project (up to 1 MiB; binary returned base64). Works across hosts. Returns {path, contents, encoding, size, truncated}.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args projectFileArgs) (*mcp.CallToolResult, any, error) {
		if args.ProjectID == "" {
			return errResult("project_id is required"), nil, nil
		}
		if args.Path == "" {
			return errResult("path is required"), nil, nil
		}
		q := url.Values{}
		q.Set("path", args.Path)
		raw, err := client.do(ctx, http.MethodGet,
			fmt.Sprintf("/projects/%s/file?%s", url.PathEscape(args.ProjectID), q.Encode()), nil)
		if err != nil {
			return errResult(err.Error()), nil, nil
		}
		return jsonResult(raw), nil, nil
	})

	_ = logger
}
