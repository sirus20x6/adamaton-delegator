package budget

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestObserveQuotaUtilization(t *testing.T) {
	QuotaUtilization.Reset()

	status := &BudgetStatus{
		Provider:  ProviderClaude,
		DailyPct:  0.42,
		WeeklyPct: 0.17,
	}
	observeQuotaUtilization(ProviderClaude, status, false)

	if got := testutil.ToFloat64(QuotaUtilization.WithLabelValues("claude", "daily")); got != 0.42 {
		t.Fatalf("daily utilization = %v, want 0.42", got)
	}
	if got := testutil.ToFloat64(QuotaUtilization.WithLabelValues("claude", "weekly")); got != 0.17 {
		t.Fatalf("weekly utilization = %v, want 0.17", got)
	}
}

func TestObserveQuotaUtilizationUnlimitedReportsZero(t *testing.T) {
	QuotaUtilization.Reset()

	// An unlimited provider with non-zero pct should still report 0 — there's
	// no quota to exhaust, so a non-zero utilization series would be misleading.
	status := &BudgetStatus{
		Provider:  ProviderLocal,
		DailyPct:  0.9,
		WeeklyPct: 0.9,
	}
	observeQuotaUtilization(ProviderLocal, status, true)

	if got := testutil.ToFloat64(QuotaUtilization.WithLabelValues("local", "daily")); got != 0 {
		t.Fatalf("unlimited daily utilization = %v, want 0", got)
	}
	if got := testutil.ToFloat64(QuotaUtilization.WithLabelValues("local", "weekly")); got != 0 {
		t.Fatalf("unlimited weekly utilization = %v, want 0", got)
	}
}

func TestObserveQuotaUtilizationNilStatusNoPanic(t *testing.T) {
	// Defensive: a nil status must be a no-op, not a panic.
	observeQuotaUtilization(ProviderClaude, nil, false)
}

func TestRequestsRejectedCounter(t *testing.T) {
	RequestsRejected.Reset()

	before := testutil.ToFloat64(RequestsRejected.WithLabelValues("no_providers"))
	RequestsRejected.WithLabelValues("no_providers").Inc()
	after := testutil.ToFloat64(RequestsRejected.WithLabelValues("no_providers"))

	if after-before != 1 {
		t.Fatalf("rejected counter delta = %v, want 1", after-before)
	}
}
