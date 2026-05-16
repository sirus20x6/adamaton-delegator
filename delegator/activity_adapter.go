package delegator

import (
	"context"

	"github.com/sirus20x6/adamomaton-platform/temporal/activities"
	"github.com/sirus20x6/adamomaton-delegator/delegator/budget"
)

// ActivityAdapter wraps an Orchestrator so it satisfies the
// activities.DelegationOrchestrator interface without the activities
// package having to import this one (that direction creates a build
// cycle through internal/executor → activities → internal/delegator).
//
// Construct with NewActivityAdapter(orch) and pass to
// activities.DelegationActivities.Orchestrator.
type ActivityAdapter struct {
	o *Orchestrator
}

// NewActivityAdapter returns an adapter that bridges an Orchestrator to
// the narrow interface the workflow activity consumes.
func NewActivityAdapter(o *Orchestrator) *ActivityAdapter {
	return &ActivityAdapter{o: o}
}

// DelegateSync converts the activity's neutral request into a typed
// DelegateRequest, calls the orchestrator, and flattens the typed Task
// back into the neutral DelegationTaskLike shape the activity returns.
func (a *ActivityAdapter) DelegateSync(ctx context.Context, in activities.DelegateRequestLike) (activities.DelegationTaskLike, error) {
	req := DelegateRequest{
		Prompt:      in.Prompt,
		Difficulty:  Difficulty(in.Difficulty),
		Priority:    budget.Priority(in.Priority),
		AgentHint:   in.AgentHint,
		WorkingDir:  in.WorkingDir,
		Model:       in.Model,
		TimeoutSecs: in.TimeoutSecs,
	}
	t, err := a.o.DelegateSync(ctx, req)
	if err != nil {
		// Even on error we may have a partial task to surface to the
		// activity (e.g. on timeout DelegateSync returns the cancelled
		// task plus ctx.Err()).
		if t == nil {
			return activities.DelegationTaskLike{}, err
		}
		return flattenTask(t), err
	}
	return flattenTask(t), nil
}

func flattenTask(t *Task) activities.DelegationTaskLike {
	return activities.DelegationTaskLike{
		ID:       t.ID,
		Agent:    t.Agent,
		Provider: string(t.Provider),
		Status:   string(t.Status),
		ExitCode: t.ExitCode,
		Output:   t.Output,
		Error:    t.Error,
	}
}
