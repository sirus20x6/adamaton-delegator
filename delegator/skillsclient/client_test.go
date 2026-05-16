package skillsclient

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestSearchHappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/skills/search" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		if r.Method != "POST" {
			t.Errorf("unexpected method %q", r.Method)
		}
		body, _ := io.ReadAll(r.Body)
		var got searchPayload
		_ = json.Unmarshal(body, &got)
		if got.Query != "refactor a function" || got.Limit != 3 {
			t.Errorf("payload mismatch: %+v", got)
		}
		_ = json.NewEncoder(w).Encode(searchResponse{Hits: []Hit{
			{SkillID: "s1", SkillName: "extract-method", ChunkKind: "skill_meta", Score: 0.9, Text: "pull a chunk into a fn"},
			{SkillID: "s2", SkillName: "rename-variable", ChunkKind: "skill_meta", Score: 0.7, Text: "give a var a clearer name"},
		}})
	}))
	defer srv.Close()

	c := &HTTPClient{BaseURL: srv.URL, Timeout: time.Second, httpClient: srv.Client()}
	hits, err := c.Search(context.Background(), "refactor a function", 3)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(hits) != 2 || hits[0].SkillName != "extract-method" {
		t.Fatalf("hits wrong: %+v", hits)
	}
}

func TestSearchEmptyQueryReturnsNil(t *testing.T) {
	c := &HTTPClient{BaseURL: "http://nowhere"}
	hits, err := c.Search(context.Background(), "   ", 5)
	if err != nil || hits != nil {
		t.Errorf("expected (nil, nil), got (%v, %v)", hits, err)
	}
}

func TestSearchUpstreamError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"boom"}`, http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	c := &HTTPClient{BaseURL: srv.URL, Timeout: time.Second, httpClient: srv.Client()}
	if _, err := c.Search(context.Background(), "x", 1); err == nil {
		t.Error("expected error on 503")
	}
}

func TestRecordUsage(t *testing.T) {
	gotBody := ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/skills/usages" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	c := &HTTPClient{BaseURL: srv.URL, Timeout: time.Second, httpClient: srv.Client()}
	if err := c.RecordUsage(context.Background(), "skill-1", "task-7"); err != nil {
		t.Fatalf("record: %v", err)
	}
	if !strings.Contains(gotBody, `"skill_id":"skill-1"`) ||
		!strings.Contains(gotBody, `"task_id":"task-7"`) {
		t.Errorf("body wrong: %s", gotBody)
	}
}

func TestRecordUsageSkipsEmptyIDs(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))
	defer srv.Close()
	c := &HTTPClient{BaseURL: srv.URL, Timeout: time.Second, httpClient: srv.Client()}
	if err := c.RecordUsage(context.Background(), "", "t"); err != nil {
		t.Fatalf("err: %v", err)
	}
	if called {
		t.Error("expected no HTTP call when skill_id empty")
	}
}

func TestFormatSkillsBlockHasExpectedShape(t *testing.T) {
	hits := []Hit{
		{SkillID: "a", SkillName: "extract-method", Community: "code-refactoring", Text: "pull into a fn"},
		{SkillID: "a", SkillName: "extract-method", Text: "duplicate"},
		{SkillID: "b", SkillName: "rename-variable", Text: "clearer name"},
	}
	out := FormatSkillsBlock(hits)
	if !strings.HasPrefix(out, "# RELEVANT SKILLS") {
		t.Errorf("expected RELEVANT SKILLS heading: %q", out[:30])
	}
	if !strings.Contains(out, "## extract-method  _[code-refactoring]_") {
		t.Errorf("missing community pill: %s", out)
	}
	if strings.Count(out, "## extract-method") != 1 {
		t.Errorf("duplicate not deduped: %s", out)
	}
	if !strings.Contains(out, "## rename-variable") {
		t.Errorf("second skill missing: %s", out)
	}
	if !strings.HasSuffix(strings.TrimRight(out, "\n"), "---") {
		t.Errorf("expected --- terminator: %q", out[len(out)-30:])
	}
}

func TestFormatSkillsBlockEmpty(t *testing.T) {
	if got := FormatSkillsBlock(nil); got != "" {
		t.Errorf("expected empty string, got %q", got)
	}
}
