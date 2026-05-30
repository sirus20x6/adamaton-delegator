package quota

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
)

// resetTokenTotalsCache clears the process-wide cache so each test starts clean.
func resetTokenTotalsCache(t *testing.T) {
	t.Helper()
	tokenTotalsCacheMu.Lock()
	tokenTotalsCache = map[string]tokenTotalsCacheEntry{}
	tokenTotalsCacheMu.Unlock()
}

func quietLogger(t *testing.T) *logrus.Logger {
	t.Helper()
	l := logrus.New()
	l.SetLevel(logrus.PanicLevel)
	return l
}

// findAgent returns the AgentUsage for the named agent, failing if absent.
func findAgent(t *testing.T, usages []AgentUsage, agent string) AgentUsage {
	t.Helper()
	for _, u := range usages {
		if u.Agent == agent {
			return u
		}
	}
	t.Fatalf("agent %q not found in usages", agent)
	return AgentUsage{}
}

// TestGetAllAgentUsage_CacheServesLastKnownOnOutage is the headline test for the
// CCSAVER cached-fallback: a successful read populates the cache, and a later
// read whose DB has vanished serves the last-known token totals instead of
// zeroing every agent.
func TestGetAllAgentUsage_CacheServesLastKnownOnOutage(t *testing.T) {
	resetTokenTotalsCache(t)

	dir := t.TempDir()
	path := filepath.Join(dir, "ccs.db")
	now := time.Now().UTC().Format(time.RFC3339)
	buildCCSaverDB(t, path, []ccsRow{
		{APIType: "anthropic", Model: "claude-opus", InputTokens: 1000, OutputTokens: 500, Timestamp: now},
		{APIType: "vllm", Model: "qwen", InputTokens: 200, OutputTokens: 100, Timestamp: now},
	})

	cfg := AggregateConfig{
		CCSaverPath:         path,
		SkipGeminiLiveQuota: true,
		Logger:              quietLogger(t),
	}

	// First call: live read, cache populated.
	first, err := GetAllAgentUsage(context.Background(), 1, cfg)
	if err != nil {
		t.Fatalf("first GetAllAgentUsage: %v", err)
	}
	claude := findAgent(t, first, "claude")
	if claude.InputTokens != 1000 || claude.OutputTokens != 500 {
		t.Fatalf("first read claude tokens = %d/%d, want 1000/500", claude.InputTokens, claude.OutputTokens)
	}

	// Simulate the CCSAVER outage: remove the DB file.
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove db: %v", err)
	}

	// Second call: live read fails, cached snapshot served.
	second, err := GetAllAgentUsage(context.Background(), 1, cfg)
	if err != nil {
		t.Fatalf("second GetAllAgentUsage: %v", err)
	}
	claude2 := findAgent(t, second, "claude")
	if claude2.InputTokens != 1000 || claude2.OutputTokens != 500 {
		t.Fatalf("cached read claude tokens = %d/%d, want last-known 1000/500", claude2.InputTokens, claude2.OutputTokens)
	}
	opencode2 := findAgent(t, second, "opencode")
	if opencode2.InputTokens != 200 {
		t.Fatalf("cached read opencode tokens = %d, want 200", opencode2.InputTokens)
	}
}

// TestGetAllAgentUsage_CacheExpiresToZero verifies that once the cached snapshot
// ages past the TTL, an outage falls through to zero tokens (the DB-only
// expected steady state) rather than serving stale data forever.
func TestGetAllAgentUsage_CacheExpiresToZero(t *testing.T) {
	resetTokenTotalsCache(t)

	dir := t.TempDir()
	path := filepath.Join(dir, "ccs.db")
	now := time.Now().UTC().Format(time.RFC3339)
	buildCCSaverDB(t, path, []ccsRow{
		{APIType: "anthropic", Model: "claude-opus", InputTokens: 1000, OutputTokens: 500, Timestamp: now},
	})

	cfg := AggregateConfig{
		CCSaverPath:         path,
		SkipGeminiLiveQuota: true,
		Logger:              quietLogger(t),
		// Negative TTL: any cache entry is treated as already-expired, so the
		// fallback never fires.
		CCSaverCacheTTL: -1,
	}

	if _, err := GetAllAgentUsage(context.Background(), 1, cfg); err != nil {
		t.Fatalf("first GetAllAgentUsage: %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove db: %v", err)
	}

	second, err := GetAllAgentUsage(context.Background(), 1, cfg)
	if err != nil {
		t.Fatalf("second GetAllAgentUsage: %v", err)
	}
	claude := findAgent(t, second, "claude")
	if claude.InputTokens != 0 || claude.OutputTokens != 0 {
		t.Fatalf("expired-cache read claude tokens = %d/%d, want 0/0", claude.InputTokens, claude.OutputTokens)
	}
}

// TestTokenTotalsCache_StoreLoadTTL exercises the cache primitives directly:
// store/load round-trips, TTL expiry, per-key isolation, disabled-on-nonpositive
// TTL, and clone isolation.
func TestTokenTotalsCache_StoreLoadTTL(t *testing.T) {
	resetTokenTotalsCache(t)

	path := "/tmp/fake-ccs.db"
	totals := map[string]AgentTokenTotals{
		"anthropic": {InputTokens: 10, OutputTokens: 20, Calls: 2, Models: []string{"claude-opus"}},
	}
	storeTokenTotalsCache(path, 1, totals)

	// Fresh load within TTL returns a clone with the same content.
	got, ok := cachedTokenTotals(path, 1, time.Minute)
	if !ok {
		t.Fatal("expected cache hit within TTL")
	}
	if got["anthropic"].InputTokens != 10 || got["anthropic"].Models[0] != "claude-opus" {
		t.Fatalf("cached content mismatch: %+v", got["anthropic"])
	}

	// Clone isolation: mutating the returned map/slice must not corrupt the cache.
	got["anthropic"] = AgentTokenTotals{InputTokens: 999}
	again, ok := cachedTokenTotals(path, 1, time.Minute)
	if !ok || again["anthropic"].InputTokens != 10 {
		t.Fatalf("cache mutated through returned value: %+v", again["anthropic"])
	}

	// Non-positive TTL disables the cache.
	if _, ok := cachedTokenTotals(path, 1, 0); ok {
		t.Fatal("zero TTL should disable cache")
	}
	if _, ok := cachedTokenTotals(path, 1, -time.Second); ok {
		t.Fatal("negative TTL should disable cache")
	}

	// Expiry: a tiny TTL ages the entry out.
	if _, ok := cachedTokenTotals(path, 1, time.Nanosecond); ok {
		time.Sleep(time.Millisecond)
		if _, ok2 := cachedTokenTotals(path, 1, time.Nanosecond); ok2 {
			t.Fatal("entry should have expired under nanosecond TTL")
		}
	}

	// Per-key isolation: a different days window is a different key.
	if _, ok := cachedTokenTotals(path, 7, time.Minute); ok {
		t.Fatal("days=7 should miss when only days=1 was stored")
	}
}

// TestAggregateConfig_CCSaverCacheTTL covers the default/override resolution.
func TestAggregateConfig_CCSaverCacheTTL(t *testing.T) {
	if got := (AggregateConfig{}).ccSaverCacheTTL(); got != defaultCCSaverCacheTTL {
		t.Fatalf("zero TTL should resolve to default %v, got %v", defaultCCSaverCacheTTL, got)
	}
	if got := (AggregateConfig{CCSaverCacheTTL: 3 * time.Minute}).ccSaverCacheTTL(); got != 3*time.Minute {
		t.Fatalf("explicit TTL not honored, got %v", got)
	}
	if got := (AggregateConfig{CCSaverCacheTTL: -1}).ccSaverCacheTTL(); got != -1 {
		t.Fatalf("negative TTL should pass through, got %v", got)
	}
}
