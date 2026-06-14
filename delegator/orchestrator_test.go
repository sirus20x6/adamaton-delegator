package delegator

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/sirus20x6/adamaton-core/executor/cli"
	"github.com/sirus20x6/adamaton-delegator/delegator/budget"
)

// fakeBudget is an in-test BudgetClient that records calls and returns
// scripted responses.
type fakeBudget struct {
	mu          sync.Mutex
	routeResp   *budget.RouteResponse
	routeErr    error
	reportCalls []budget.ReportRequest
	lastReq     budget.RouteRequest
}

func (f *fakeBudget) Route(req budget.RouteRequest) (*budget.RouteResponse, error) {
	f.mu.Lock()
	f.lastReq = req
	f.mu.Unlock()
	if f.routeErr != nil {
		return nil, f.routeErr
	}
	return f.routeResp, nil
}

func (f *fakeBudget) lastRouteReq() budget.RouteRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastReq
}

func (f *fakeBudget) Report(req budget.ReportRequest) (*budget.ReportResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reportCalls = append(f.reportCalls, req)
	return &budget.ReportResponse{Recorded: true}, nil
}

// fakeCLI returns a CLIExecutor wired to /bin/sh with a single "fake"
// agent that emits a fixed JSONL line.
func fakeCLI(t *testing.T, agentName, response string) *cli.CLIExecutor {
	t.Helper()
	return &cli.CLIExecutor{
		Specs: map[string]*cli.AgentSpec{
			agentName: {
				Binary: "/bin/sh",
				BuildArgs: func(_ cli.CLIInput, _ *cli.AgentSpec) []string {
					return []string{"-c", "printf '%s\\n' '" + response + "'"}
				},
				BuildEnv: func(_ cli.CLIInput) map[string]string { return nil },
				ParseOutput: func(stdout, _ string) string {
					return stdout
				},
			},
		},
	}
}

func waitForStatus(t *testing.T, store Store, id string, want TaskStatus, timeout time.Duration) *Task {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if task, ok := store.Get(id); ok && task.Status == want {
			return task
		}
		time.Sleep(10 * time.Millisecond)
	}
	if task, ok := store.Get(id); ok {
		t.Fatalf("task %s did not reach %s within %s; final status %s", id, want, timeout, task.Status)
	} else {
		t.Fatalf("task %s vanished from store", id)
	}
	return nil
}

func newTestOrchestrator(t *testing.T, fb *fakeBudget, cli *cli.CLIExecutor) *Orchestrator {
	t.Helper()
	logger := logrus.New()
	logger.SetLevel(logrus.WarnLevel)
	o := New(fb, cli, logger)
	// Overwrite the agent map so "fake" maps to a real provider — keeps
	// the routing path exercising the real reverse-lookup.
	o.ProviderToAgent = map[budget.Provider]string{
		budget.ProviderOpenAI: "fake",
		budget.ProviderLocal:  "fake-local",
	}
	return o
}

func TestOrchestrator_Delegate_Success(t *testing.T) {
	cli := fakeCLI(t, "fake", "hello world")
	fb := &fakeBudget{
		routeResp: &budget.RouteResponse{Provider: budget.ProviderOpenAI, Model: "gpt-4o"},
	}
	o := newTestOrchestrator(t, fb, cli)

	task, err := o.Delegate(context.Background(), DelegateRequest{
		Prompt:     "do something",
		Difficulty: DifficultyMedium,
		Priority:   budget.PriorityNormal,
	})
	if err != nil {
		t.Fatalf("Delegate: %v", err)
	}
	if task.Agent != "fake" {
		t.Errorf("expected agent 'fake', got %q", task.Agent)
	}
	if task.Provider != budget.ProviderOpenAI {
		t.Errorf("expected provider openai, got %q", task.Provider)
	}

	final := waitForStatus(t, o.Store, task.ID, StatusCompleted, 3*time.Second)
	if final.Output == "" {
		t.Error("expected non-empty Output on completed task")
	}
	if final.ExitCode != 0 {
		t.Errorf("expected exit 0, got %d", final.ExitCode)
	}

	// Allow one tick for report goroutine to finish.
	time.Sleep(20 * time.Millisecond)
	fb.mu.Lock()
	defer fb.mu.Unlock()
	if len(fb.reportCalls) == 0 {
		t.Error("expected at least one budget report call")
	}
	if fb.reportCalls[0].Provider != budget.ProviderOpenAI {
		t.Errorf("report provider mismatch: got %q", fb.reportCalls[0].Provider)
	}
	if fb.reportCalls[0].Success == nil || !*fb.reportCalls[0].Success {
		t.Error("expected report Success=true")
	}
}

func TestOrchestrator_AgentHint_Override(t *testing.T) {
	cli := fakeCLI(t, "fake", "hi")
	// Even though the budget would route to a different provider, the hint
	// should win. A nil routeResp would cause an error if hint isn't honoured.
	fb := &fakeBudget{routeErr: errors.New("router should not be called")}
	o := newTestOrchestrator(t, fb, cli)

	task, err := o.Delegate(context.Background(), DelegateRequest{
		Prompt:    "x",
		AgentHint: "fake",
	})
	if err != nil {
		t.Fatalf("Delegate with hint: %v", err)
	}
	if task.Agent != "fake" {
		t.Errorf("expected hinted agent, got %q", task.Agent)
	}
	waitForStatus(t, o.Store, task.ID, StatusCompleted, 3*time.Second)
}

func TestOrchestrator_AgentHint_Unknown(t *testing.T) {
	cli := fakeCLI(t, "fake", "hi")
	fb := &fakeBudget{}
	o := newTestOrchestrator(t, fb, cli)

	_, err := o.Delegate(context.Background(), DelegateRequest{
		Prompt:    "x",
		AgentHint: "doesnotexist",
	})
	if err == nil {
		t.Fatal("expected error for unknown agent hint")
	}
}

func TestOrchestrator_RouterError(t *testing.T) {
	cli := fakeCLI(t, "fake", "hi")
	fb := &fakeBudget{routeErr: errors.New("budget exhausted")}
	o := newTestOrchestrator(t, fb, cli)

	_, err := o.Delegate(context.Background(), DelegateRequest{Prompt: "x"})
	if err == nil {
		t.Fatal("expected error when router fails")
	}
}

func TestOrchestrator_RouterReturnsUnmappedProvider(t *testing.T) {
	cli := fakeCLI(t, "fake", "hi")
	fb := &fakeBudget{
		routeResp: &budget.RouteResponse{Provider: budget.ProviderClaude},
	}
	o := newTestOrchestrator(t, fb, cli)
	// ProviderToAgent doesn't map claude → any agent.

	_, err := o.Delegate(context.Background(), DelegateRequest{Prompt: "x"})
	if err == nil {
		t.Fatal("expected error when router returns unmapped provider")
	}
}

func TestOrchestrator_Cancel(t *testing.T) {
	// Slow CLI: sleeps long enough to be cancellable.
	cli := &cli.CLIExecutor{
		Specs: map[string]*cli.AgentSpec{
			"slow": {
				Binary:      "/bin/sh",
				BuildArgs:   func(_ cli.CLIInput, _ *cli.AgentSpec) []string { return []string{"-c", "sleep 5"} },
				BuildEnv:    func(_ cli.CLIInput) map[string]string { return nil },
				ParseOutput: func(s, _ string) string { return s },
			},
		},
	}
	o := New(nil, cli, nil)
	o.ProviderToAgent = map[budget.Provider]string{}

	task, err := o.Delegate(context.Background(), DelegateRequest{
		Prompt:    "x",
		AgentHint: "slow",
	})
	if err != nil {
		t.Fatalf("Delegate: %v", err)
	}

	// Wait for it to enter Running.
	waitForStatus(t, o.Store, task.ID, StatusRunning, 2*time.Second)

	if !o.Cancel(task.ID) {
		t.Fatal("Cancel returned false on a running task")
	}

	// Final status should not be Completed (subprocess was killed).
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		got, _ := o.Store.Get(task.ID)
		if got.Status == StatusCancelled || got.Status == StatusFailed || got.Status == StatusTimedOut {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	got, _ := o.Store.Get(task.ID)
	t.Fatalf("expected cancelled/failed/timed_out; got %s", got.Status)
}

func TestOrchestrator_PromptRequired(t *testing.T) {
	o := New(&fakeBudget{}, fakeCLI(t, "fake", "x"), nil)
	_, err := o.Delegate(context.Background(), DelegateRequest{})
	if err == nil {
		t.Fatal("expected error for empty prompt")
	}
}

func TestDifficulty_ToComplexity(t *testing.T) {
	cases := map[Difficulty]budget.TaskComplexity{
		DifficultyTrivial: budget.ComplexityLow,
		DifficultyEasy:    budget.ComplexityLow,
		DifficultyMedium:  budget.ComplexityMedium,
		DifficultyHard:    budget.ComplexityHigh,
		DifficultyExpert:  budget.ComplexityCritical,
		Difficulty(""):    budget.ComplexityMedium, // default
	}
	for d, want := range cases {
		if got := d.ToComplexity(); got != want {
			t.Errorf("difficulty %q → complexity %q, want %q", d, got, want)
		}
	}
}

func TestTaskStore_Eviction(t *testing.T) {
	s := NewTaskStore(3)
	// Fill with 5 completed tasks; oldest 2 should evict.
	for i := 0; i < 5; i++ {
		s.Put(&Task{
			ID:        "t" + string(rune('0'+i)),
			Status:    StatusCompleted,
			CreatedAt: time.Now().Add(time.Duration(i) * time.Second),
		})
	}
	if got := len(s.List("", "")); got != 3 {
		t.Errorf("expected 3 retained tasks, got %d", got)
	}
	// Oldest two evicted.
	if _, ok := s.Get("t0"); ok {
		t.Error("t0 should be evicted")
	}
	if _, ok := s.Get("t4"); !ok {
		t.Error("t4 should still be present")
	}
}

func TestTaskStore_RunningHeldEvenAtCap(t *testing.T) {
	// At cap with two running tasks and one done, the done one is the only
	// evictable entry and must go first when a fourth task is added.
	s := NewTaskStore(2)
	s.Put(&Task{ID: "running1", Status: StatusRunning, CreatedAt: time.Now()})
	s.Put(&Task{ID: "done1", Status: StatusCompleted, CreatedAt: time.Now().Add(time.Second)})
	s.Put(&Task{ID: "running2", Status: StatusRunning, CreatedAt: time.Now().Add(2 * time.Second)})
	// Cap is 2; we exceeded it. Eviction must drop done1 (only terminal one).
	if _, ok := s.Get("done1"); ok {
		t.Error("done1 should be evicted when running tasks fill the cap")
	}
	if _, ok := s.Get("running1"); !ok {
		t.Error("running1 should be retained")
	}
	if _, ok := s.Get("running2"); !ok {
		t.Error("running2 should be retained")
	}
}

func TestTaskStore_AllInFlightExceedsCap(t *testing.T) {
	// When everything in the store is running and the cap is reached, the
	// evictor must give up rather than violate the "never evict in-flight"
	// rule. The store is allowed to grow past cap until tasks settle.
	s := NewTaskStore(2)
	s.Put(&Task{ID: "running1", Status: StatusRunning, CreatedAt: time.Now()})
	s.Put(&Task{ID: "running2", Status: StatusRunning, CreatedAt: time.Now()})
	s.Put(&Task{ID: "running3", Status: StatusRunning, CreatedAt: time.Now()})
	if got := len(s.List("", "")); got != 3 {
		t.Errorf("expected 3 retained running tasks (cap exceeded but none evictable), got %d", got)
	}
}

func TestOrchestrator_InflightCounter(t *testing.T) {
	o := New(&fakeBudget{}, fakeCLI(t, "fake", "x"), nil)

	// Empty → nil snapshot (preserves the router's pre-load behaviour).
	if snap := o.inflightSnapshot(); snap != nil {
		t.Fatalf("expected nil snapshot when idle, got %v", snap)
	}

	o.incrInflight(budget.ProviderOpenAI)
	o.incrInflight(budget.ProviderOpenAI)
	o.incrInflight(budget.ProviderGemini)
	o.incrInflight("") // empty provider must be ignored

	snap := o.inflightSnapshot()
	if snap[budget.ProviderOpenAI] != 2 || snap[budget.ProviderGemini] != 1 {
		t.Fatalf("unexpected snapshot: %v", snap)
	}
	if _, ok := snap[""]; ok {
		t.Errorf("empty provider must not appear in snapshot")
	}

	// Snapshot is a copy — mutating it must not corrupt internal state.
	snap[budget.ProviderOpenAI] = 99
	if o.inflightSnapshot()[budget.ProviderOpenAI] != 2 {
		t.Errorf("snapshot must be a defensive copy")
	}

	o.decrInflight(budget.ProviderGemini) // 1 → 0, key removed
	o.decrInflight(budget.ProviderOpenAI) // 2 → 1
	final := o.inflightSnapshot()
	if final[budget.ProviderOpenAI] != 1 {
		t.Errorf("expected openai=1 after decr, got %d", final[budget.ProviderOpenAI])
	}
	if _, ok := final[budget.ProviderGemini]; ok {
		t.Errorf("gemini key should be deleted at zero, got %v", final)
	}
}

func TestOrchestrator_PassesProviderLoadToRouter(t *testing.T) {
	cli := fakeCLI(t, "fake", "ok")
	fb := &fakeBudget{routeResp: &budget.RouteResponse{Provider: budget.ProviderOpenAI, Model: "gpt-4o"}}
	o := newTestOrchestrator(t, fb, cli)

	// Simulate an already-outstanding delegation on the openai provider.
	o.incrInflight(budget.ProviderOpenAI)

	task, err := o.Delegate(context.Background(), DelegateRequest{
		Prompt:     "do something",
		Difficulty: DifficultyMedium,
	})
	if err != nil {
		t.Fatalf("Delegate: %v", err)
	}
	// The router must have seen the pre-existing in-flight task as load
	// (the new task's own increment happens after routing, so it's 1).
	if got := fb.lastRouteReq().ProviderLoad[budget.ProviderOpenAI]; got != 1 {
		t.Fatalf("expected ProviderLoad[openai]=1 at route time, got %d (load=%v)",
			got, fb.lastRouteReq().ProviderLoad)
	}

	// Once the task finishes, both increments unwind back to the seeded one.
	waitForStatus(t, o.Store, task.ID, StatusCompleted, 3*time.Second)
	if got := o.inflightSnapshot()[budget.ProviderOpenAI]; got != 1 {
		t.Fatalf("expected openai back to the seeded 1 after completion, got %d", got)
	}
}

func TestOrchestrator_HonorsExplicitTaskID(t *testing.T) {
	fb := &fakeBudget{routeResp: &budget.RouteResponse{Provider: budget.ProviderOpenAI, Model: "gpt-4o"}}
	o := newTestOrchestrator(t, fb, fakeCLI(t, "fake", "ok"))

	task, err := o.Delegate(context.Background(), DelegateRequest{
		Prompt:     "x",
		Difficulty: DifficultyEasy,
		TaskID:     "task-pinned-123",
	})
	if err != nil {
		t.Fatalf("Delegate: %v", err)
	}
	if task.ID != "task-pinned-123" {
		t.Fatalf("expected pinned id, got %q", task.ID)
	}
	// Retrievable by the pinned id (the durable path returns it to the caller).
	waitForStatus(t, o.Store, "task-pinned-123", StatusCompleted, 3*time.Second)
}

func TestOrchestrator_GeneratesTaskIDWhenUnset(t *testing.T) {
	fb := &fakeBudget{routeResp: &budget.RouteResponse{Provider: budget.ProviderOpenAI}}
	o := newTestOrchestrator(t, fb, fakeCLI(t, "fake", "ok"))
	task, err := o.Delegate(context.Background(), DelegateRequest{Prompt: "x"})
	if err != nil {
		t.Fatalf("Delegate: %v", err)
	}
	if task.ID == "" || task.ID == "task-pinned-123" {
		t.Fatalf("expected a freshly generated id, got %q", task.ID)
	}
}

func TestOrchestrator_ChooseAgent(t *testing.T) {
	fb := &fakeBudget{routeResp: &budget.RouteResponse{Provider: budget.ProviderOpenAI}}
	o := newTestOrchestrator(t, fb, fakeCLI(t, "fake", "x"))
	agent, provider, err := o.ChooseAgent(DelegateRequest{Prompt: "x", Difficulty: DifficultyMedium})
	if err != nil {
		t.Fatalf("ChooseAgent: %v", err)
	}
	if agent != "fake" || provider != budget.ProviderOpenAI {
		t.Fatalf("got agent=%q provider=%q", agent, provider)
	}
}

func TestNewTaskID(t *testing.T) {
	a, b := NewTaskID(), NewTaskID()
	if a == "" || a == b {
		t.Fatalf("ids must be non-empty and unique, got %q and %q", a, b)
	}
}
