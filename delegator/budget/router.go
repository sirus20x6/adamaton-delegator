package budget

import (
	"fmt"
	"math"
	"sort"

	"github.com/sirupsen/logrus"
)

// Router selects the optimal provider for a task based on budget, capability, and cost.
type Router struct {
	tracker *Tracker
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

	var candidates []scoredProvider

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

		// Composite score
		score := weights.capability*capScore + weights.headroom*headroomScore + weights.cost*costScore

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
			reason: fmt.Sprintf("score=%.3f (cap=%.2f head=%.2f cost=%.2f)",
				score, capScore, headroomScore, costScore),
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
		// All providers exhausted — fall back to local if it's both configured
		// AND its status row says it's actually available. If local has been
		// flagged unavailable (consecutive errors / hard outage), don't paper
		// over that by recommending it as a "fallback" — surface the failure.
		if localPC, ok := r.configs[ProviderLocal]; ok {
			if localStatus, hasStatus := statusMap[ProviderLocal]; hasStatus && localStatus.IsAvailable {
				return &RouteResponse{
					Provider:      ProviderLocal,
					Model:         localPC.ModelForComplexity(req.TaskComplexity),
					Reason:        "all cloud providers exhausted, falling back to local",
					BudgetWarning: "all cloud budgets exhausted",
					Confidence:    0.3,
				}, nil
			}
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

	return resp, nil
}
