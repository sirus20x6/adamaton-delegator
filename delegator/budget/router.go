package budget

import (
	"fmt"
	"math"
	"sort"

	"github.com/sirupsen/logrus"
)

// statusProvider is the slice of *Tracker that the Router actually depends on:
// the current per-provider budget snapshot. Narrowing the dependency to this
// interface (rather than the concrete *Tracker, which needs a live Postgres
// store) lets the load-aware routing logic be unit-tested with an injected fake.
// *Tracker satisfies it, so NewRouter's public signature is unchanged.
type statusProvider interface {
	Status() ([]BudgetStatus, error)
}

// Router selects the optimal provider for a task based on budget, capability, and cost.
type Router struct {
	tracker statusProvider
	configs map[Provider]*ProviderConfig
	logger  *logrus.Logger
}

// NewRouter creates a Router backed by the given Tracker.
func NewRouter(tracker *Tracker, configs map[Provider]*ProviderConfig, logger *logrus.Logger) *Router {
	return &Router{
		tracker: tracker,
		configs: configs,
		logger:  logger,
	}
}

// newRouterWithStatus builds a Router from any statusProvider. Test-only seam
// (lowercase, so it stays internal) that avoids standing up a Postgres-backed
// Tracker just to exercise the scoring/filtering logic.
func newRouterWithStatus(tracker statusProvider, configs map[Provider]*ProviderConfig, logger *logrus.Logger) *Router {
	return &Router{
		tracker: tracker,
		configs: configs,
		logger:  logger,
	}
}

// scoringWeights holds the weight factors for each complexity level.
type scoringWeights struct {
	capability float64
	headroom   float64
	cost       float64
}

var weightsByComplexity = map[TaskComplexity]scoringWeights{
	ComplexityCritical: {capability: 0.7, headroom: 0.2, cost: 0.1},
	ComplexityHigh:     {capability: 0.5, headroom: 0.3, cost: 0.2},
	ComplexityMedium:   {capability: 0.3, headroom: 0.3, cost: 0.4},
	ComplexityLow:      {capability: 0.1, headroom: 0.2, cost: 0.7},
}

// priorityCostMultiplier shifts the cost weight before scoring. Immediate
// tasks are willing to pay more; background tasks should pile onto whichever
// provider is cheapest. Within-call comparison is what matters — we don't
// renormalize the weight sum.
var priorityCostMultiplier = map[Priority]float64{
	PriorityImmediate:  0.3,
	PriorityNormal:     1.0,
	PriorityBackground: 1.5,
}

// minimumStrength is the floor below which a provider gets penalized for a complexity level.
var minimumStrength = map[TaskComplexity]float64{
	ComplexityCritical: 0.8,
	ComplexityHigh:     0.6,
	ComplexityMedium:   0.3,
	ComplexityLow:      0.0,
}

type scoredProvider struct {
	provider Provider
	config   *ProviderConfig
	status   *BudgetStatus
	score    float64
	model    string
	reason   string
}

// Route evaluates all providers and returns the best choice for the given request.
func (r *Router) Route(req RouteRequest) (*RouteResponse, error) {
	statuses, err := r.tracker.Status()
	if err != nil {
		return nil, fmt.Errorf("get statuses: %w", err)
	}

	statusMap := make(map[Provider]*BudgetStatus)
	for i := range statuses {
		statusMap[statuses[i].Provider] = &statuses[i]
	}

	// Publish quota-utilization gauges for every provider we have both config
	// and a fresh status for. Doing it here (rather than a separate scrape
	// loop) keeps the gauge in lockstep with the statuses the router actually
	// evaluated, and costs nothing extra since we already hold them.
	for provider, pc := range r.configs {
		if status, ok := statusMap[provider]; ok {
			observeQuotaUtilization(provider, status, pc.IsUnlimited())
		}
	}

	// Find max cost for normalization
	var maxCost float64
	for _, pc := range r.configs {
		if pc.CostPerMToken > maxCost {
			maxCost = pc.CostPerMToken
		}
	}
	if maxCost == 0 {
		maxCost = 1 // avoid division by zero
	}

	weights, ok := weightsByComplexity[req.TaskComplexity]
	if !ok {
		weights = weightsByComplexity[ComplexityMedium]
	}

	priority := req.Priority
	if priority == "" {
		priority = PriorityNormal
	}
	if mul, ok := priorityCostMultiplier[priority]; ok {
		weights.cost *= mul
	}

	minStr := minimumStrength[req.TaskComplexity]

	// loadEmphasis scales the load-proportional penalty. A deeper backlog means
	// we care more about spreading work, so the penalty grows with QueueDepth
	// (saturating so a runaway queue can't make the penalty dominate the score).
	// With no queue info it stays at the baseline so a single isolated request
	// still prefers the least-loaded provider, just less aggressively.
	loadEmphasis := loadEmphasisFor(req.QueueDepth)

	var (
		candidates []scoredProvider
		// concurrencyBlocked counts cloud providers that were viable on every
		// other axis but are currently at their concurrency cap. It lets us tell
		// "all cloud providers are momentarily saturated" apart from "no provider
		// is configured/available at all", so the no-candidates branch can pick
		// the right reason and fall back to local correctly.
		concurrencyBlocked int
	)

	// Iterate providers in sorted name order. Map iteration is unordered, so
	// feeding candidates to sort.Slice (unstable) means tied scores would
	// flap between calls on identical input. Sorting the keys first plus
	// using sort.SliceStable below makes Recommend()/Route() deterministic.
	names := make([]string, 0, len(r.configs))
	for n := range r.configs {
		names = append(names, string(n))
	}
	sort.Strings(names)

	for _, name := range names {
		provider := Provider(name)
		pc := r.configs[provider]
		status, hasStatus := statusMap[provider]
		if !hasStatus {
			continue
		}

		// Hard filter: unavailable
		if !status.IsAvailable {
			r.logger.WithField("provider", provider).Debug("Skipped: unavailable")
			continue
		}

		// Hard filter: require_cloud
		if req.RequireCloud && pc.Tier != TierCloud {
			r.logger.WithField("provider", provider).Debug("Skipped: not cloud")
			continue
		}

		// Hard filter: budget exhausted (cloud only)
		if pc.Tier == TierCloud && !pc.IsUnlimited() {
			if pc.DailyLimit > 0 && req.EstimatedTokens > status.DailyRemaining {
				r.logger.WithField("provider", provider).Debug("Skipped: exceeds daily remaining")
				continue
			}
			if pc.WeeklyLimit > 0 && req.EstimatedTokens > status.WeeklyRemaining {
				r.logger.WithField("provider", provider).Debug("Skipped: exceeds weekly remaining")
				continue
			}
		}

		// Hard filter: per-provider concurrency cap. A provider that already has
		// MaxConcurrency tasks in flight can't take another right now. Local /
		// unlimited providers leave MaxConcurrency at 0 so they're never blocked
		// here, which is what keeps the local fallback viable when every cloud
		// provider is saturated. We only count cloud providers as
		// "concurrency-blocked" for the fallback-reason heuristic.
		load := req.ProviderLoad[provider]
		if pc.MaxConcurrency > 0 && load >= pc.MaxConcurrency {
			r.logger.WithFields(logrus.Fields{
				"provider": provider,
				"load":     load,
				"cap":      pc.MaxConcurrency,
			}).Debug("Skipped: at concurrency cap")
			if pc.Tier == TierCloud {
				concurrencyBlocked++
			}
			continue
		}

		// Capability score
		capScore := pc.Strength
		if pc.Strength < minStr && minStr > 0 {
			capScore = pc.Strength / minStr * 0.5 // penalize below minimum
		}

		// Budget headroom score
		var headroomScore float64
		if pc.IsUnlimited() {
			headroomScore = 1.0 // local always has full headroom
		} else {
			dailyHeadroom := 1.0
			if pc.DailyLimit > 0 {
				dailyHeadroom = 1.0 - status.DailyPct
			}
			weeklyHeadroom := 1.0
			if pc.WeeklyLimit > 0 {
				weeklyHeadroom = 1.0 - status.WeeklyPct
			}
			headroomScore = math.Min(dailyHeadroom, weeklyHeadroom)
		}

		// Cost efficiency score (lower cost = higher score)
		costScore := 1.0 - (pc.CostPerMToken / maxCost)

		// Load score (less in-flight work = higher score). It's a fraction of
		// the provider's concurrency cap that's still free; uncapped providers
		// are always fully free (1.0). The penalty subtracted from the composite
		// is (1-loadScore) scaled by loadEmphasis, so a half-full provider with a
		// deep queue loses more ground than the same provider with no backlog.
		loadScore := loadScoreFor(load, pc.MaxConcurrency)

		// Composite score
		score := weights.capability*capScore + weights.headroom*headroomScore + weights.cost*costScore
		score -= loadEmphasis * (1.0 - loadScore)

		// Preference bonus
		if req.PreferProvider != "" && req.PreferProvider == provider {
			score += 0.1
		}

		model := pc.ModelForComplexity(req.TaskComplexity)

		candidates = append(candidates, scoredProvider{
			provider: provider,
			config:   pc,
			status:   status,
			score:    score,
			model:    model,
			reason: fmt.Sprintf("score=%.3f (cap=%.2f head=%.2f cost=%.2f load=%.2f)",
				score, capScore, headroomScore, costScore, loadScore),
		})
	}

	// Sort descending by score. Use SliceStable so that ties — which are
	// common at startup (empty stats) and when multiple providers are
	// exhausted — preserve the lexicographic provider order established by
	// the sorted-names iteration above. Result: identical inputs always
	// produce the same Provider/FallbackProvider, so operators see a stable
	// recommendation rather than two flapping ones.
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].score != candidates[j].score {
			return candidates[i].score > candidates[j].score
		}
		// Explicit tiebreaker mirrors the input order; defensive, since
		// the iteration above already feeds candidates in lex order.
		return candidates[i].provider < candidates[j].provider
	})

	if len(candidates) == 0 {
		// Distinguish "everything is saturated right now" from "nothing is
		// usable at all". When the only thing standing between us and a viable
		// cloud provider is its concurrency cap, the right answer is "retry
		// shortly" (or spill to local), not the hard "exhausted" message that
		// historically fired at :227 even though the cloud provider had budget
		// to spare — it was just momentarily busy.
		reason := "all cloud providers exhausted, falling back to local"
		warning := "all cloud budgets exhausted"
		rejectReason := "no_providers"
		if concurrencyBlocked > 0 {
			reason = "all cloud providers at concurrency cap, falling back to local"
			warning = "all cloud providers at concurrency cap"
			rejectReason = "all_at_concurrency_cap"
		}

		// Fall back to local if it's both configured AND its status row says it's
		// actually available. If local has been flagged unavailable (consecutive
		// errors / hard outage), don't paper over that by recommending it as a
		// "fallback" — surface the failure. The local fallback also respects its
		// own concurrency cap: an uncapped local (MaxConcurrency==0) is always
		// eligible, but a capped, saturated local is not.
		if localPC, ok := r.configs[ProviderLocal]; ok {
			localBlocked := localPC.MaxConcurrency > 0 && req.ProviderLoad[ProviderLocal] >= localPC.MaxConcurrency
			if localStatus, hasStatus := statusMap[ProviderLocal]; hasStatus && localStatus.IsAvailable && !localBlocked {
				return &RouteResponse{
					Provider:      ProviderLocal,
					Model:         localPC.ModelForComplexity(req.TaskComplexity),
					Reason:        reason,
					BudgetWarning: warning,
					Confidence:    0.3,
				}, nil
			}
		}
		RequestsRejected.WithLabelValues(rejectReason).Inc()
		if concurrencyBlocked > 0 {
			return nil, fmt.Errorf("no providers available for request: all cloud providers at concurrency cap")
		}
		return nil, fmt.Errorf("no providers available for request")
	}

	best := candidates[0]
	resp := &RouteResponse{
		Provider:   best.provider,
		Model:      best.model,
		Reason:     best.reason,
		Confidence: math.Min(best.score, 1.0),
	}

	// Second-best as fallback
	if len(candidates) > 1 {
		resp.FallbackProvider = candidates[1].provider
	}

	// Budget warning when either daily or weekly utilization is >80%.
	// Daily wins the slot if both are over because daily resets faster.
	if best.status != nil && best.config != nil && !best.config.IsUnlimited() {
		switch {
		case best.config.DailyLimit > 0 && best.status.DailyPct > 0.8:
			resp.BudgetWarning = fmt.Sprintf("%s daily budget at %.0f%%", best.provider, best.status.DailyPct*100)
		case best.config.WeeklyLimit > 0 && best.status.WeeklyPct > 0.8:
			resp.BudgetWarning = fmt.Sprintf("%s weekly budget at %.0f%%", best.provider, best.status.WeeklyPct*100)
		}
	}

	// Surface a concurrency warning when the winner is itself getting full, so
	// the operator sees pressure building before it tips into the fallback path.
	// A budget-utilization warning (set above) is the more urgent signal, so it
	// keeps the slot — only fill it from load when it's still empty.
	if resp.BudgetWarning == "" && best.config != nil && best.config.MaxConcurrency > 0 {
		bestLoad := req.ProviderLoad[best.provider]
		if float64(bestLoad) >= 0.8*float64(best.config.MaxConcurrency) {
			resp.BudgetWarning = fmt.Sprintf("%s at %d/%d concurrent tasks",
				best.provider, bestLoad, best.config.MaxConcurrency)
		}
	}

	return resp, nil
}

// Load-aware routing tunables. These are deliberately modest: the load penalty
// is a tiebreaker-grade nudge at baseline and only grows teeth when a backlog
// builds, so existing capability/cost/headroom routing decisions don't flip on
// a single in-flight task.
const (
	// baseLoadEmphasis is the load penalty weight with no queue backlog. Small
	// enough to act as a tiebreaker between otherwise-equal providers.
	baseLoadEmphasis = 0.05
	// maxLoadEmphasis caps the penalty weight so a runaway queue can't let load
	// alone override capability/cost entirely.
	maxLoadEmphasis = 0.5
	// queueDepthFullScale is the backlog size at which loadEmphasis reaches
	// maxLoadEmphasis. Past this the emphasis is clamped.
	queueDepthFullScale = 20.0
)

// loadEmphasisFor maps the queue depth to the load-penalty weight. It rises
// linearly from baseLoadEmphasis (empty queue) to maxLoadEmphasis at
// queueDepthFullScale, then clamps. Negative depths are treated as zero.
func loadEmphasisFor(queueDepth int) float64 {
	if queueDepth <= 0 {
		return baseLoadEmphasis
	}
	frac := float64(queueDepth) / queueDepthFullScale
	if frac > 1 {
		frac = 1
	}
	return baseLoadEmphasis + frac*(maxLoadEmphasis-baseLoadEmphasis)
}

// loadScoreFor returns a [0,1] "freeness" score for a provider given its
// in-flight load and concurrency cap. Uncapped providers (cap<=0) are always
// fully free. For capped providers it's the fraction of capacity still open,
// clamped to [0,1] so an over-cap load (shouldn't happen — it's hard-filtered
// earlier — but defensive) reads as zero rather than negative.
func loadScoreFor(load, capacity int) float64 {
	if capacity <= 0 {
		return 1.0
	}
	if load < 0 {
		load = 0
	}
	free := float64(capacity-load) / float64(capacity)
	if free < 0 {
		free = 0
	}
	if free > 1 {
		free = 1
	}
	return free
}
