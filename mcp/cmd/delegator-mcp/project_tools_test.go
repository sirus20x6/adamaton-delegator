package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
)

// The project_* tools are thin wrappers over kanbanClient.do against the
// apiserver's /api/v1/projects routes. These tests exercise the path/query
// construction the tool handlers build and the cross-host file-access response
// shape, using a fake apiserver that records what it received.

// recordingServer captures the last request method, path, and query so a test
// can assert the tool built the expected REST call.
type recordingServer struct {
	srv    *httptest.Server
	method string
	path   string
	query  url.Values
}

func newRecordingServer(t *testing.T, status int, body string) *recordingServer {
	t.Helper()
	rs := &recordingServer{}
	rs.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rs.method = r.Method
		rs.path = r.URL.Path
		rs.query = r.URL.Query()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(rs.srv.Close)
	return rs
}

// TestProjectList_Path verifies project_list hits GET /projects.
func TestProjectList_Path(t *testing.T) {
	rs := newRecordingServer(t, http.StatusOK, `[{"id":"p1"}]`)
	c := newTestKanbanClient(rs.srv.URL)

	raw, err := c.do(context.Background(), http.MethodGet, "/projects", nil)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	if rs.method != http.MethodGet || rs.path != "/api/v1/projects" {
		t.Fatalf("unexpected request: %s %s", rs.method, rs.path)
	}
	if !strings.Contains(string(raw), "p1") {
		t.Fatalf("unexpected body: %s", raw)
	}
}

// TestProjectGet_PathEscaping verifies an id with characters needing escaping is
// path-escaped (mirrors project_get's url.PathEscape).
func TestProjectGet_PathEscaping(t *testing.T) {
	rs := newRecordingServer(t, http.StatusOK, `{"id":"p/1"}`)
	c := newTestKanbanClient(rs.srv.URL)

	id := "p/1 weird"
	_, err := c.do(context.Background(), http.MethodGet, "/projects/"+url.PathEscape(id), nil)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	// The server decodes the path; it must round-trip to the original id.
	want := "/api/v1/projects/" + id
	if rs.path != want {
		t.Fatalf("path escaping wrong: got %q want %q", rs.path, want)
	}
}

// TestProjectTree_QueryParams verifies the tree call sets path + depth query
// params exactly as the project_tree handler does.
func TestProjectTree_QueryParams(t *testing.T) {
	rs := newRecordingServer(t, http.StatusOK, `[]`)
	c := newTestKanbanClient(rs.srv.URL)

	q := url.Values{}
	q.Set("path", "sub/dir")
	q.Set("depth", strconv.Itoa(2))
	_, err := c.do(context.Background(), http.MethodGet,
		"/projects/"+url.PathEscape("p1")+"/tree?"+q.Encode(), nil)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	if rs.path != "/api/v1/projects/p1/tree" {
		t.Fatalf("unexpected path: %s", rs.path)
	}
	if rs.query.Get("path") != "sub/dir" || rs.query.Get("depth") != "2" {
		t.Fatalf("unexpected query: %v", rs.query)
	}
}

// TestProjectFile_CrossHostAccess verifies the file read on a remote-hosted
// project returns the apiserver's proxied payload verbatim. From the client's
// perspective a cross-host read is an ordinary GET — the apiserver hides the
// deploy-agent proxy — so the test asserts the payload (including a host marker)
// flows through unchanged.
func TestProjectFile_CrossHostAccess(t *testing.T) {
	payload := `{"path":"main.go","contents":"package main","encoding":"utf-8","size":12,"truncated":false,"host":"pi5"}`
	rs := newRecordingServer(t, http.StatusOK, payload)
	c := newTestKanbanClient(rs.srv.URL)

	q := url.Values{}
	q.Set("path", "main.go")
	raw, err := c.do(context.Background(), http.MethodGet,
		"/projects/"+url.PathEscape("remote-proj")+"/file?"+q.Encode(), nil)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	if rs.path != "/api/v1/projects/remote-proj/file" || rs.query.Get("path") != "main.go" {
		t.Fatalf("unexpected request: %s ?%s", rs.path, rs.query.Encode())
	}
	var file map[string]any
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if file["host"] != "pi5" || file["contents"] != "package main" {
		t.Fatalf("cross-host payload not passed through: %s", raw)
	}
}

// TestProjectFile_CrossHostError verifies a failed remote read (e.g. the
// deploy-agent on the target host is unreachable) surfaces the apiserver's
// error to the caller. The apiserver returns 502 for a dead downstream host;
// 5xx on a GET is retried, so the handler with maxRetries attempts still ends
// in a clear error rather than a silent empty result.
func TestProjectFile_CrossHostError(t *testing.T) {
	rs := newRecordingServer(t, http.StatusBadGateway, `{"error":"host pi5-speaker unreachable"}`)
	c := newTestKanbanClient(rs.srv.URL)

	q := url.Values{}
	q.Set("path", "main.go")
	_, err := c.do(context.Background(), http.MethodGet,
		"/projects/"+url.PathEscape("remote-proj")+"/file?"+q.Encode(), nil)
	if err == nil {
		t.Fatal("expected error from unreachable host")
	}
	if !strings.Contains(err.Error(), "unreachable") {
		t.Fatalf("downstream error not surfaced: %v", err)
	}
}

// TestProjectRegister_PostBody verifies project_register posts the optional
// host/display_name only when set (mirroring the handler's conditional body).
func TestProjectRegister_PostBody(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"p-new"}`))
	}))
	defer srv.Close()
	c := newTestKanbanClient(srv.URL)

	body := map[string]any{"path": "/abs/path", "host": "pi5"}
	_, err := c.do(context.Background(), http.MethodPost, "/projects", body)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	if gotBody["path"] != "/abs/path" || gotBody["host"] != "pi5" {
		t.Fatalf("register body not threaded: %+v", gotBody)
	}
	if _, ok := gotBody["display_name"]; ok {
		t.Fatalf("display_name should be absent when unset: %+v", gotBody)
	}
}
