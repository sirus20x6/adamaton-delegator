package delegator

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sirus20x6/adamaton-delegator/delegator/budget"
	"github.com/sirus20x6/adamaton-delegator/delegator/skillsclient"
	"github.com/sirus20x6/adamaton-core/executor/cli"
)

// fakeSkillsClient lets tests script Search results and watch every
// RecordUsage call.
type fakeSkillsClient struct {
	hits      []skillsclient.Hit
	searchErr error

	mu      sync.Mutex
	queries []string
	usages  []struct{ skillID, taskID string }
}

func (f *fakeSkillsClient) Search(_ context.Context, q string, _ int) ([]skillsclient.Hit, error) {
	f.mu.Lock()
	f.queries = append(f.queries, q)
	f.mu.Unlock()
	return f.hits, f.searchErr
}

func (f *fakeSkillsClient) RecordUsage(_ context.Context, skillID, taskID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.usages = append(f.usages, struct{ skillID, taskID string }{skillID, taskID})
	return nil
}

// promptCapturingCLI returns a CLIExecutor that writes the received
// prompt to a shared buffer before producing the fixed response. The
// agent's binary is /bin/sh — keeps the test hermetic.
func promptCapturingCLI(t *testing.T, captured *string, mu *sync.Mutex, response string) *cli.CLIExecutor {
	t.Helper()
	return &cli.CLIExecutor{
		Specs: map[string]*cli.AgentSpec{
			"fake": {
				Binary: "/bin/sh",
				BuildArgs: func(in cli.CLIInput, _ *cli.AgentSpec) []string {
					mu.Lock()
					*captured = in.Prompt
					mu.Unlock()
					return []string{"-c", "printf '%s\\n' '" + response + "'"}
				},
				BuildEnv:    func(_ cli.CLIInput) map[string]string { return nil },
				ParseOutput: func(stdout, _ string) string { return stdout },
			},
		},
	}
}

func TestOrchestrator_SkillsEnrichesPromptAndRecordsUsage(t *testing.T) {
	var captured string
	var mu sync.Mutex
	cli := promptCapturingCLI(t, &captured, &mu, "ok")

	fb := &fakeBudget{routeResp: &budget.RouteResponse{Provider: budget.ProviderOpenAI, Model: "gpt-4o"}}
	o := newTestOrchestrator(t, fb, cli)

	skills := &fakeSkillsClient{
		hits: []skillsclient.Hit{
			{SkillID: "sk-1", SkillName: "extract-method", Community: "code-refactoring",
				Score: 0.9, Text: "pull a chunk of code into a named function"},
			{SkillID: "sk-2", SkillName: "rename-variable",
				Score: 0.7, Text: "improve a variable's name"},
		},
	}
	o.Skills = skills
	o.SkillsTopK = 3

	task, err := o.Delegate(context.Background(), DelegateRequest{
		Prompt:     "refactor the payment processing function",
		Difficulty: DifficultyMedium,
		Priority:   budget.PriorityNormal,
	})
	if err != nil {
		t.Fatalf("Delegate: %v", err)
	}
	waitForStatus(t, o.Store, task.ID, StatusCompleted, 3*time.Second)

	// 1. The CLI received the enriched prompt.
	mu.Lock()
	got := captured
	mu.Unlock()
	if !strings.Contains(got, "# RELEVANT SKILLS") {
		t.Errorf("expected enriched prompt to contain RELEVANT SKILLS, got %q", got)
	}
	if !strings.Contains(got, "extract-method") {
		t.Errorf("expected enriched prompt to mention extract-method, got %q", got)
	}
	if !strings.HasSuffix(strings.TrimSpace(got), "refactor the payment processing function") {
		t.Errorf("expected user prompt to be appended after the skills block, got %q", got)
	}

	// 2. task.Prompt records the *original* prompt, not the enriched one
	//    (budget metering measures the user's intent, not the augmentation).
	final, ok := o.Store.Get(task.ID)
	if !ok {
		t.Fatal("task vanished")
	}
	if final.Prompt != "refactor the payment processing function" {
		t.Errorf("task.Prompt should be the original, got %q", final.Prompt)
	}

	// 3. Skills.Search was called once with the user's prompt.
	skills.mu.Lock()
	queries := append([]string(nil), skills.queries...)
	usages := append([]struct{ skillID, taskID string }(nil), skills.usages...)
	skills.mu.Unlock()
	if len(queries) != 1 || queries[0] != "refactor the payment processing function" {
		t.Errorf("unexpected search queries: %v", queries)
	}

	// 4. RecordUsage was called once per hit after completion. The
	//    usage POSTs run in a background goroutine; poll briefly.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		skills.mu.Lock()
		usages = append([]struct{ skillID, taskID string }(nil), skills.usages...)
		skills.mu.Unlock()
		if len(usages) >= 2 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(usages) != 2 {
		t.Fatalf("expected 2 usage records, got %d: %+v", len(usages), usages)
	}
	want := map[string]bool{"sk-1": false, "sk-2": false}
	for _, u := range usages {
		if u.taskID != task.ID {
			t.Errorf("usage taskID mismatch: %+v vs task.ID=%s", u, task.ID)
		}
		if _, ok := want[u.skillID]; !ok {
			t.Errorf("unexpected skill_id in usage: %s", u.skillID)
		}
		want[u.skillID] = true
	}
	for sk, seen := range want {
		if !seen {
			t.Errorf("missing usage for %s", sk)
		}
	}
}

func TestOrchestrator_SkillsErrorIsNonFatal(t *testing.T) {
	var captured string
	var mu sync.Mutex
	cli := promptCapturingCLI(t, &captured, &mu, "ok")
	fb := &fakeBudget{routeResp: &budget.RouteResponse{Provider: budget.ProviderOpenAI, Model: "gpt-4o"}}
	o := newTestOrchestrator(t, fb, cli)
	o.Skills = &fakeSkillsClient{searchErr: context.DeadlineExceeded}

	task, err := o.Delegate(context.Background(), DelegateRequest{
		Prompt:     "do the thing",
		Difficulty: DifficultyTrivial,
		Priority:   budget.PriorityBackground,
	})
	if err != nil {
		t.Fatalf("Delegate: %v", err)
	}
	waitForStatus(t, o.Store, task.ID, StatusCompleted, 3*time.Second)

	mu.Lock()
	got := captured
	mu.Unlock()
	if strings.Contains(got, "RELEVANT SKILLS") {
		t.Errorf("expected no skills block on search error, got %q", got)
	}
	if got != "do the thing" {
		t.Errorf("expected original prompt on search error, got %q", got)
	}
}
