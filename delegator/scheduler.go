package delegator

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"

	"github.com/sirus20x6/adamomaton-delegator/delegator/budget"
)

// Scheduler wraps Temporal's ScheduleClient for recurring delegations. It
// is intentionally a thin layer — Temporal already does cron, pause,
// backfill, and run history; this type just gives callers a CRUD surface
// keyed by user-friendly schedule IDs and validated request payloads.
type Scheduler struct {
	client       client.Client
	taskQueue    string
	workflowName string
	logger       *logrus.Logger
}

// NewScheduler constructs a Scheduler. taskQueue must match what the
// worker registered DelegationWorkflow on. workflowName identifies the
// workflow to schedule (defaults to "DelegationWorkflow" if empty).
func NewScheduler(c client.Client, taskQueue, workflowName string, logger *logrus.Logger) *Scheduler {
	if logger == nil {
		logger = logrus.New()
	}
	if workflowName == "" {
		workflowName = "DelegationWorkflow"
	}
	return &Scheduler{
		client:       c,
		taskQueue:    taskQueue,
		workflowName: workflowName,
		logger:       logger,
	}
}

// ScheduleSpec is the input to Create. Mirrors the user-facing MCP tool
// arguments verbatim so the wire shape is the schedule shape.
type ScheduleSpec struct {
	ID          string
	Cron        string // standard 5-field cron in the operator's timezone
	Note        string // free-text, surfaced in list_recurring
	Prompt      string
	Difficulty  Difficulty
	Priority    budget.Priority
	AgentHint   string
	WorkingDir  string
	Model       string
	TimeoutSecs int
}

// ScheduleSummary is the per-row response for List/Describe.
type ScheduleSummary struct {
	ID         string    `json:"id"`
	Cron       string    `json:"cron"`
	Note       string    `json:"note,omitempty"`
	Paused     bool      `json:"paused"`
	NextRun    time.Time `json:"next_run,omitempty"`
	LastRun    time.Time `json:"last_run,omitempty"`
	Prompt     string    `json:"prompt"`
	Difficulty string    `json:"difficulty,omitempty"`
	Priority   string    `json:"priority,omitempty"`
	AgentHint  string    `json:"agent_hint,omitempty"`
}

// ErrScheduleExists is returned when Create is called with an ID that
// is already in use. Callers can check via errors.Is.
var ErrScheduleExists = errors.New("schedule already exists")

// Create registers a new recurring delegation. ID must be globally unique
// in the Temporal namespace (we sanity-check and bail with ErrScheduleExists
// on collision). Cron is parsed by Temporal — we don't pre-validate.
func (s *Scheduler) Create(ctx context.Context, spec ScheduleSpec) (*ScheduleSummary, error) {
	if spec.ID == "" {
		return nil, errors.New("schedule ID is required")
	}
	if spec.Cron == "" {
		return nil, errors.New("cron expression is required")
	}
	if spec.Prompt == "" {
		return nil, errors.New("prompt is required")
	}
	if spec.Difficulty != "" && !ValidDifficulties[spec.Difficulty] {
		return nil, fmt.Errorf("invalid difficulty %q", spec.Difficulty)
	}
	if spec.Priority != "" && !budget.ValidPriorities[spec.Priority] {
		return nil, fmt.Errorf("invalid priority %q", spec.Priority)
	}

	scheduleClient := s.client.ScheduleClient()
	input := scheduleInputForSpec(spec)

	handle, err := scheduleClient.Create(ctx, client.ScheduleOptions{
		ID: spec.ID,
		Spec: client.ScheduleSpec{
			CronExpressions: []string{spec.Cron},
		},
		Action: &client.ScheduleWorkflowAction{
			ID:        "delegation-" + spec.ID,
			Workflow:  s.workflowName,
			TaskQueue: s.taskQueue,
			Args:      []interface{}{input},
		},
		Note: spec.Note,
	})
	if err != nil {
		if isAlreadyExistsErr(err) {
			return nil, fmt.Errorf("%w: %s", ErrScheduleExists, spec.ID)
		}
		return nil, fmt.Errorf("create schedule: %w", err)
	}

	desc, derr := handle.Describe(ctx)
	if derr != nil {
		s.logger.WithError(derr).WithField("schedule_id", spec.ID).Warn("describe right after create failed")
		return baseSummaryFromSpec(spec), nil
	}
	return summariseSchedule(spec.ID, desc, spec), nil
}

// List returns all schedules visible in the current namespace. The
// Temporal SDK paginates internally — we walk the iterator until done
// or until ctx is cancelled.
func (s *Scheduler) List(ctx context.Context) ([]*ScheduleSummary, error) {
	iter, err := s.client.ScheduleClient().List(ctx, client.ScheduleListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list schedules: %w", err)
	}
	out := []*ScheduleSummary{}
	for iter.HasNext() {
		entry, err := iter.Next()
		if err != nil {
			return out, fmt.Errorf("list iterate: %w", err)
		}
		out = append(out, summariseListEntry(entry))
	}
	return out, nil
}

// Cancel deletes the schedule. Idempotent — deleting a schedule that
// doesn't exist returns nil, since the user's intent (it shouldn't exist)
// is already satisfied.
func (s *Scheduler) Cancel(ctx context.Context, id string) error {
	handle := s.client.ScheduleClient().GetHandle(ctx, id)
	if err := handle.Delete(ctx); err != nil {
		if isNotFoundErr(err) {
			return nil
		}
		return fmt.Errorf("delete schedule: %w", err)
	}
	return nil
}

// scheduleInputForSpec builds the activities.DelegationInput payload for
// the workflow. Defined as map[string]any rather than the typed struct
// to avoid a circular import — workflows imports activities; this
// package would have to import workflows to use the typed struct, which
// would import this back. The map round-trips fine through the workflow
// arg encoder.
func scheduleInputForSpec(spec ScheduleSpec) map[string]any {
	return map[string]any{
		"prompt":          spec.Prompt,
		"difficulty":      string(spec.Difficulty),
		"priority":        string(spec.Priority),
		"agent_hint":      spec.AgentHint,
		"working_dir":     spec.WorkingDir,
		"model":           spec.Model,
		"timeout_seconds": spec.TimeoutSecs,
	}
}

func summariseSchedule(id string, desc *client.ScheduleDescription, spec ScheduleSpec) *ScheduleSummary {
	out := baseSummaryFromSpec(spec)
	out.ID = id
	if desc != nil {
		if desc.Schedule.State != nil {
			out.Paused = desc.Schedule.State.Paused
		}
		if len(desc.Info.NextActionTimes) > 0 {
			out.NextRun = desc.Info.NextActionTimes[0]
		}
		if len(desc.Info.RecentActions) > 0 {
			out.LastRun = desc.Info.RecentActions[len(desc.Info.RecentActions)-1].ActualTime
		}
	}
	return out
}

func summariseListEntry(e *client.ScheduleListEntry) *ScheduleSummary {
	out := &ScheduleSummary{
		ID:     e.ID,
		Note:   e.Note,
		Paused: e.Paused,
	}
	if e.Spec != nil && len(e.Spec.CronExpressions) > 0 {
		out.Cron = e.Spec.CronExpressions[0]
	}
	if len(e.NextActionTimes) > 0 {
		out.NextRun = e.NextActionTimes[0]
	}
	if len(e.RecentActions) > 0 {
		out.LastRun = e.RecentActions[len(e.RecentActions)-1].ActualTime
	}
	return out
}

func baseSummaryFromSpec(spec ScheduleSpec) *ScheduleSummary {
	return &ScheduleSummary{
		ID:         spec.ID,
		Cron:       spec.Cron,
		Note:       spec.Note,
		Prompt:     spec.Prompt,
		Difficulty: string(spec.Difficulty),
		Priority:   string(spec.Priority),
		AgentHint:  spec.AgentHint,
	}
}

// isAlreadyExistsErr prefers the typed serviceerror.AlreadyExists
// returned by the Temporal SDK so we're not pattern-matching on
// English-language error messages. The string fallback catches the
// (rare) cases where the SDK wraps the error in a way errors.As
// can't unwrap — for example when an intermediate layer renders the
// status to text before passing it up.
func isAlreadyExistsErr(err error) bool {
	if err == nil {
		return false
	}
	var ae *serviceerror.AlreadyExists
	if errors.As(err, &ae) {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "already exists") || strings.Contains(msg, "alreadyexists")
}

// isNotFoundErr prefers the typed serviceerror.NotFound for the same
// reason as isAlreadyExistsErr; the string fallback is a safety net.
func isNotFoundErr(err error) bool {
	if err == nil {
		return false
	}
	var nf *serviceerror.NotFound
	if errors.As(err, &nf) {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "not found") || strings.Contains(msg, "notfound")
}
