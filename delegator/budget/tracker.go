package budget

import (
	"fmt"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
)

// Tracker wraps Store with budget business logic and periodic resets.
type Tracker struct {
	store    *Store
	config   *ServiceConfig
	configs  map[Provider]*ProviderConfig
	logger   *logrus.Logger
	location *time.Location

	stopCh   chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup
}

// NewTracker creates a Tracker, initializes all configured providers in the store,
// and starts a background ticker for period resets.
func NewTracker(store *Store, config *ServiceConfig, logger *logrus.Logger) (*Tracker, error) {
	if config == nil {
		return nil, fmt.Errorf("nil ServiceConfig")
	}
	if err := config.Validate(); err != nil {
		return nil, fmt.Errorf("invalid ServiceConfig: %w", err)
	}

	loc, err := time.LoadLocation(config.Timezone)
	if err != nil {
		return nil, fmt.Errorf("load timezone %q: %w", config.Timezone, err)
	}

	t := &Tracker{
		store:    store,
		config:   config,
		configs:  make(map[Provider]*ProviderConfig),
		logger:   logger,
		location: loc,
		stopCh:   make(chan struct{}),
	}

	now := time.Now().In(loc)
	for i := range config.Providers {
		pc := &config.Providers[i]
		t.configs[pc.Provider] = pc

		dailyReset := t.nextDailyReset(now)
		weeklyReset := t.nextWeeklyReset(now)

		if err := store.InitProvider(pc.Provider, pc.DailyLimit, pc.WeeklyLimit, dailyReset, weeklyReset); err != nil {
			return nil, fmt.Errorf("init provider %s: %w", pc.Provider, err)
		}
		logger.WithFields(logrus.Fields{
			"provider":     pc.Provider,
			"daily_limit":  pc.DailyLimit,
			"weekly_limit": pc.WeeklyLimit,
		}).Info("Initialized provider")
	}

	// Start the background ticker only after all error paths are cleared, so
	// we never leak a goroutine when construction fails.
	t.wg.Add(1)
	go t.resetLoop()

	return t, nil
}

// Report records a usage report and returns remaining budget.
func (t *Tracker) Report(req ReportRequest) (*ReportResponse, error) {
	pc, ok := t.configs[req.Provider]
	if !ok {
		return nil, fmt.Errorf("unknown provider: %s", req.Provider)
	}

	success := true
	if req.Success != nil {
		success = *req.Success
	}

	rec := UsageRecord{
		Provider:         req.Provider,
		Model:            req.Model,
		PromptTokens:     req.PromptTokens,
		CompletionTokens: req.CompletionTokens,
		TotalTokens:      req.TotalTokens,
		TaskID:           req.TaskID,
		TaskComplexity:   req.TaskComplexity,
		Success:          success,
		RecordedAt:       time.Now().UTC(),
	}

	if err := t.store.RecordUsage(rec); err != nil {
		return nil, fmt.Errorf("record usage: %w", err)
	}

	status, err := t.store.GetProviderStatus(req.Provider)
	if err != nil {
		return nil, fmt.Errorf("get status: %w", err)
	}

	resp := &ReportResponse{Recorded: true}

	if pc.IsUnlimited() {
		resp.DailyRemaining = -1
		resp.WeeklyRemaining = -1
		return resp, nil
	}

	if pc.DailyLimit > 0 {
		resp.DailyRemaining = pc.DailyLimit - status.DailyUsed
		if resp.DailyRemaining < 0 {
			resp.DailyRemaining = 0
		}
	} else {
		resp.DailyRemaining = -1 // unlimited dimension
	}

	if pc.WeeklyLimit > 0 {
		resp.WeeklyRemaining = pc.WeeklyLimit - status.WeeklyUsed
		if resp.WeeklyRemaining < 0 {
			resp.WeeklyRemaining = 0
		}
	} else {
		resp.WeeklyRemaining = -1 // unlimited dimension
	}

	// Surface a budget warning when either the daily or weekly bucket is
	// >80% utilized. Daily takes precedence in the message because it's the
	// faster-recovering signal, but weekly is checked too so a slow drip
	// week doesn't sneak past the daily 80% threshold unnoticed.
	if pc.DailyLimit > 0 && status.DailyPct > 0.8 {
		resp.BudgetWarning = fmt.Sprintf("%s daily budget at %.0f%%", req.Provider, status.DailyPct*100)
	} else if pc.WeeklyLimit > 0 && status.WeeklyPct > 0.8 {
		resp.BudgetWarning = fmt.Sprintf("%s weekly budget at %.0f%%", req.Provider, status.WeeklyPct*100)
	}

	return resp, nil
}

// Status returns budget status for all providers.
func (t *Tracker) Status() ([]BudgetStatus, error) {
	statuses, err := t.store.GetAllStatuses()
	if err != nil {
		return nil, err
	}

	// Annotate tier from config
	for i := range statuses {
		if pc, ok := t.configs[statuses[i].Provider]; ok {
			statuses[i].Tier = pc.Tier
		}
	}
	return statuses, nil
}

// ProviderStatus returns budget status for a single provider.
func (t *Tracker) ProviderStatus(provider Provider) (*BudgetStatus, error) {
	status, err := t.store.GetProviderStatus(provider)
	if err != nil {
		return nil, err
	}
	if pc, ok := t.configs[provider]; ok {
		status.Tier = pc.Tier
	}
	return status, nil
}

// ResetProvider manually resets a provider's daily and weekly counters.
func (t *Tracker) ResetProvider(provider Provider) error {
	if _, ok := t.configs[provider]; !ok {
		return fmt.Errorf("unknown provider: %s", provider)
	}

	now := time.Now().In(t.location)
	if err := t.store.ResetDaily(provider, t.nextDailyReset(now)); err != nil {
		return err
	}
	return t.store.ResetWeekly(provider, t.nextWeeklyReset(now))
}

// History returns usage records, optionally filtered by provider.
func (t *Tracker) History(provider Provider, since time.Time, limit int) ([]UsageRecord, error) {
	return t.store.QueryHistory(provider, since, limit)
}

// Stop stops the background ticker and waits for it to exit. It is safe to
// call multiple times — repeat invocations are no-ops, so callers may freely
// `defer Stop()` from multiple scopes.
func (t *Tracker) Stop() {
	t.stopOnce.Do(func() {
		close(t.stopCh)
		t.wg.Wait()
	})
}

// --- background reset loop ---

func (t *Tracker) resetLoop() {
	defer t.wg.Done()

	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()

	// Run once immediately on startup
	t.checkResets()

	for {
		select {
		case <-ticker.C:
			t.checkResets()
		case <-t.stopCh:
			return
		}
	}
}

func (t *Tracker) checkResets() {
	now := time.Now().In(t.location)

	statuses, err := t.store.GetAllStatuses()
	if err != nil {
		t.logger.WithError(err).Error("Failed to get statuses for reset check")
		return
	}

	for _, status := range statuses {
		if now.After(status.DailyResetAt) {
			nextReset := t.nextDailyReset(now)
			if err := t.store.ResetDaily(status.Provider, nextReset); err != nil {
				t.logger.WithError(err).WithField("provider", status.Provider).Error("Failed daily reset")
			} else {
				t.logger.WithField("provider", status.Provider).Info("Daily budget reset")
			}
		}

		if now.After(status.WeeklyResetAt) {
			nextReset := t.nextWeeklyReset(now)
			if err := t.store.ResetWeekly(status.Provider, nextReset); err != nil {
				t.logger.WithError(err).WithField("provider", status.Provider).Error("Failed weekly reset")
			} else {
				t.logger.WithField("provider", status.Provider).Info("Weekly budget reset")
			}
		}
	}
}

// nextDailyReset returns the next daily_reset_hour in the configured timezone.
//
// DST note: time.Date does not error when the requested wall clock doesn't
// exist (US spring-forward at 02:00 → 03:00) — it normalizes forward by an
// hour. The reset still fires (Go won't crash) but the LOGGED hour will be
// off by one twice a year. We emit a warning so operators see exactly what
// happened and don't spend an hour debugging "why did the daily reset fire
// at 03:00 today?". The same path runs through the weekly reset (which
// uses the same hour), so we keep the check here at the leaf.
func (t *Tracker) nextDailyReset(now time.Time) time.Time {
	hour := t.config.DailyResetHour
	today := time.Date(now.Year(), now.Month(), now.Day(), hour, 0, 0, 0, t.location)
	if !now.Before(today) {
		today = today.AddDate(0, 0, 1)
	}
	if today.Hour() != hour && t.logger != nil {
		t.logger.WithFields(logrus.Fields{
			"requested_hour": hour,
			"actual_hour":    today.Hour(),
			"date":           today.Format(time.DateOnly),
			"timezone":       t.config.Timezone,
		}).Warn("DST shift: budget reset hour normalized (requested wall clock does not exist this day)")
	}
	return today.UTC()
}

// nextWeeklyReset returns the next weekly_reset_day at daily_reset_hour.
func (t *Tracker) nextWeeklyReset(now time.Time) time.Time {
	targetDay := time.Weekday(t.config.WeeklyResetDay)
	hour := t.config.DailyResetHour

	// Start from today at reset hour
	candidate := time.Date(now.Year(), now.Month(), now.Day(), hour, 0, 0, 0, t.location)

	// Find the next occurrence of targetDay
	daysUntil := int(targetDay) - int(candidate.Weekday())
	if daysUntil < 0 {
		daysUntil += 7
	}
	candidate = candidate.AddDate(0, 0, daysUntil)

	// If candidate is not in the future, advance a week
	if !now.Before(candidate) {
		candidate = candidate.AddDate(0, 0, 7)
	}
	return candidate.UTC()
}
