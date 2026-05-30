package budget

import (
	"github.com/prometheus/client_golang/prometheus"
)

// Budget-router-specific Prometheus collectors. These live in the budget
// package (rather than core/metrics) because they're tied to this service's
// domain model — provider quota utilization and routing rejections. They're
// exposed on the same `/metrics` endpoint the server already serves via
// core/metrics.Handler() (both register against the default registry).

// QuotaUtilization reports each provider's current budget utilization as a
// fraction in [0,1]. It's a gauge — the value moves up as usage accrues and
// snaps back down on the daily/weekly reset. The `window` label distinguishes
// the daily and weekly budgets so operators can alert on either independently.
//
// Labels:
//
//	provider — provider identifier ("claude", "openai", "gemini", "local").
//	window   — "daily" or "weekly".
//
// Unlimited providers (local, or any with no caps) report 0 — they have no
// quota to exhaust, so a utilization series for them would be meaningless.
var QuotaUtilization = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
		Name: "budget_provider_quota_utilization",
		Help: "Provider budget utilization as a fraction in [0,1], by provider and window (daily|weekly).",
	},
	[]string{"provider", "window"},
)

// RequestsRejected counts routing requests the router could not satisfy — i.e.
// no provider was available for the request (all exhausted/unavailable, or a
// hard filter like require_cloud left no candidates). Operators should alert on
// a non-zero rate: it means tasks are being turned away.
//
// Labels:
//
//	reason — short, low-cardinality cause. Currently "no_providers" (the
//	         router found zero viable candidates). Keep this set closed.
var RequestsRejected = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "budget_requests_rejected_total",
		Help: "Total routing requests rejected because no provider was available, by reason.",
	},
	[]string{"reason"},
)

func init() {
	prometheus.MustRegister(
		QuotaUtilization,
		RequestsRejected,
	)
}

// observeQuotaUtilization updates the QuotaUtilization gauge for a single
// provider from a fresh BudgetStatus. Unlimited providers report 0 for both
// windows. Called from the router on every Route() so the gauge tracks the
// statuses the router actually evaluated — no separate scrape loop needed.
func observeQuotaUtilization(provider Provider, status *BudgetStatus, unlimited bool) {
	if status == nil {
		return
	}
	daily, weekly := status.DailyPct, status.WeeklyPct
	if unlimited {
		daily, weekly = 0, 0
	}
	QuotaUtilization.WithLabelValues(string(provider), "daily").Set(daily)
	QuotaUtilization.WithLabelValues(string(provider), "weekly").Set(weekly)
}
