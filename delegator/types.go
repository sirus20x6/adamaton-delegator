// Package delegator contains the orchestration glue between the MCP-facing
// delegate_task surface and the underlying CLI executor + budget router.
// The orchestrator decides which agent runs a request based on declared
// difficulty + priority, spawns it via internal/executor.CLIExecutor, and
// records token usage so the budget tracker stays accurate.
package delegator

import (
	"time"

	"github.com/sirus20x6/adamomaton-delegator/delegator/budget"
)

// TaskStatus mirrors the TS delegator's lifecycle.
type TaskStatus string

const (
	StatusPending   TaskStatus = "pending"
	StatusRunning   TaskStatus = "running"
	StatusCompleted TaskStatus = "completed"
	StatusFailed    TaskStatus = "failed"
	StatusCancelled TaskStatus = "cancelled"
	StatusTimedOut  TaskStatus = "timed_out"
)

// Difficulty is the caller-supplied estimate of how hard a task is. The MCP
// tool description tells the model to pick one before calling delegate_task.
// Mapped to budget.TaskComplexity in the orchestrator.
type Difficulty string

const (
	DifficultyTrivial Difficulty = "trivial"
	DifficultyEasy    Difficulty = "easy"
	DifficultyMedium  Difficulty = "medium"
	DifficultyHard    Difficulty = "hard"
	DifficultyExpert  Difficulty = "expert"
)

// ValidDifficulties is the recognized set.
var ValidDifficulties = map[Difficulty]bool{
	DifficultyTrivial: true,
	DifficultyEasy:    true,
	DifficultyMedium:  true,
	DifficultyHard:    true,
	DifficultyExpert:  true,
}

// ToComplexity maps a difficulty to the budget router's complexity enum.
// Two difficulty levels collapse onto each complexity — the router's four
// buckets are fine-grained enough; we only expose the wider scale to the
// model because difficulty is an estimate and a five-bucket grid gives the
// model a clearer signal than four.
func (d Difficulty) ToComplexity() budget.TaskComplexity {
	switch d {
	case DifficultyTrivial, DifficultyEasy:
		return budget.ComplexityLow
	case DifficultyMedium:
		return budget.ComplexityMedium
	case DifficultyHard:
		return budget.ComplexityHigh
	case DifficultyExpert:
		return budget.ComplexityCritical
	}
	return budget.ComplexityMedium
}

// DelegateRequest is the orchestrator's input. The MCP layer decodes the
// delegate_task tool call into this shape.
type DelegateRequest struct {
	Prompt      string
	Difficulty  Difficulty
	Priority    budget.Priority
	AgentHint   string // optional: skip routing, use this agent directly
	WorkingDir  string
	Model       string // optional: override model selection
	TimeoutSecs int    // 0 → orchestrator default
}

// Task is the record kept in the in-memory store.
type Task struct {
	ID          string          `json:"id"`
	Agent       string          `json:"agent"`
	Provider    budget.Provider `json:"provider"`
	Difficulty  Difficulty      `json:"difficulty,omitempty"`
	Priority    budget.Priority `json:"priority,omitempty"`
	Prompt      string          `json:"prompt"`
	WorkingDir  string          `json:"working_dir,omitempty"`
	Model       string          `json:"model,omitempty"`
	Status      TaskStatus      `json:"status"`
	CreatedAt   time.Time       `json:"created_at"`
	StartedAt   time.Time       `json:"started_at,omitempty"`
	CompletedAt time.Time       `json:"completed_at,omitempty"`
	ExitCode    int             `json:"exit_code,omitempty"`
	Output      string          `json:"output,omitempty"`
	Stderr      string          `json:"stderr,omitempty"`
	Error       string          `json:"error,omitempty"`
}
