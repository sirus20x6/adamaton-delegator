package contextmode

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sirus20x6/adamomaton-core/octen"
)

// fakeOcten stands in for the deepresearch sidecar's /v1/embeddings.
// It returns one deterministic embedding per input string, sized to
// octen.EmbedDim, so callers can verify dim/round-trip without
// depending on a real model. inputs containing "boom" trigger a 500.
//
// This is a contextmode-local copy of core/octen's test fake — both
// dense_search_test.go and any future intra-package tests need a
// running fake to point an *octen.Client at; rather than exposing
// internal test types from core/octen, we keep an identical fake
// here. The two files stay in lockstep with the embed contract.
type fakeOcten struct {
	t           *testing.T
	server      *httptest.Server
	queryInputs [][]string
	docInputs   [][]string
}

func newFakeOcten(t *testing.T) *fakeOcten {
	t.Helper()
	f := &fakeOcten{t: t}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/embeddings", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model     string   `json:"model"`
			Input     []string `json:"input"`
			InputType string   `json:"input_type"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad req: "+err.Error(), http.StatusBadRequest)
			return
		}
		switch req.InputType {
		case "query":
			f.queryInputs = append(f.queryInputs, req.Input)
		case "document":
			f.docInputs = append(f.docInputs, req.Input)
		}
		for _, in := range req.Input {
			if strings.Contains(in, "boom") {
				http.Error(w, "synthetic failure", http.StatusInternalServerError)
				return
			}
		}
		items := make([]map[string]any, len(req.Input))
		for i, in := range req.Input {
			items[i] = map[string]any{
				"object":    "embedding",
				"embedding": deterministicEmbedding(in),
				"index":     i,
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"object": "list",
			"data":   items,
			"model":  req.Model,
		})
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	f.server = httptest.NewServer(mux)
	t.Cleanup(f.server.Close)
	return f
}

// deterministicEmbedding produces a unit-ish vector that's stable per
// input string. Different inputs produce visibly different vectors so
// cosine search can rank them.
func deterministicEmbedding(s string) []float32 {
	out := make([]float32, octen.EmbedDim)
	hashed := uint32(2166136261)
	for _, b := range []byte(s) {
		hashed ^= uint32(b)
		hashed *= 16777619
	}
	for i := range out {
		hashed ^= uint32(i)
		hashed *= 16777619
		out[i] = float32(int32(hashed%2000)-1000) / 1000.0
	}
	return out
}
