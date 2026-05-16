package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/sirupsen/logrus"
	"github.com/spf13/viper"
	"github.com/sirus20x6/adamomaton-delegator/delegator/budget"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	logger := logrus.New()
	logger.SetFormatter(&logrus.TextFormatter{FullTimestamp: true})

	config, err := loadConfig()
	if err != nil {
		return fmt.Errorf("failed to load configuration: %w", err)
	}

	level, err := logrus.ParseLevel(config.LogLevel)
	if err != nil {
		level = logrus.InfoLevel
	}
	logger.SetLevel(level)

	if config.DSN == "" {
		return fmt.Errorf("budget_router.dsn is required (e.g. postgres://gogents:...@localhost:5433/gogents)")
	}

	store, err := budget.NewStore(config.DSN, logger)
	if err != nil {
		return fmt.Errorf("failed to open store: %w", err)
	}
	defer store.Close()

	tracker, err := budget.NewTracker(store, config, logger)
	if err != nil {
		return fmt.Errorf("failed to create tracker: %w", err)
	}
	defer tracker.Stop()

	// Build configs map for router
	providerConfigs := make(map[budget.Provider]*budget.ProviderConfig)
	for i := range config.Providers {
		pc := &config.Providers[i]
		providerConfigs[pc.Provider] = pc
	}

	router := budget.NewRouter(tracker, providerConfigs, logger)
	server := budget.NewServer(tracker, router, logger)
	server.SetAPIToken(config.APIToken)

	if config.APIToken == "" {
		logger.Warn("API_TOKEN is empty; budget-router auth is disabled. This is acceptable only on loopback bind.")
	}

	logger.Info("Starting budget router service")
	if err := server.Start(config.ListenAddr); err != nil {
		return fmt.Errorf("server error: %w", err)
	}
	return nil
}

func loadConfig() (*budget.ServiceConfig, error) {
	v := viper.New()
	v.SetConfigName("budget")
	v.SetConfigType("yaml")
	v.AddConfigPath("configs/")
	v.AddConfigPath(".")

	v.SetEnvPrefix("BUDGET")
	v.AutomaticEnv()
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))

	// Defaults
	v.SetDefault("budget_router.listen_addr", "127.0.0.1:8070")
	v.SetDefault("budget_router.dsn", os.Getenv("POSTGRES_DSN"))
	v.SetDefault("budget_router.daily_reset_hour", 0)
	v.SetDefault("budget_router.weekly_reset_day", 1)
	v.SetDefault("budget_router.timezone", "UTC")
	v.SetDefault("budget_router.log_level", "info")
	v.SetDefault("budget_router.api_token", "")

	if err := v.ReadInConfig(); err != nil {
		if _, ok := err.(viper.ConfigFileNotFoundError); !ok {
			return nil, fmt.Errorf("read config: %w", err)
		}
		// Config file not found — use defaults + env
	}

	var wrapper struct {
		BudgetRouter budget.ServiceConfig `mapstructure:"budget_router"`
	}
	if err := v.Unmarshal(&wrapper); err != nil {
		return nil, fmt.Errorf("unmarshal config: %w", err)
	}

	cfg := &wrapper.BudgetRouter

	// If no providers configured, use a minimal default set
	if len(cfg.Providers) == 0 {
		cfg.Providers = defaultProviders()
	}

	return cfg, nil
}

func defaultProviders() []budget.ProviderConfig {
	return []budget.ProviderConfig{
		{
			Provider:      budget.ProviderClaude,
			Tier:          budget.TierCloud,
			Strength:      0.95,
			DailyLimit:    200000,
			WeeklyLimit:   1000000,
			DefaultModel:  "claude-sonnet-4-5-20250929",
			Models:        map[string]string{"critical": "claude-opus-4-5-20251101", "high": "claude-sonnet-4-5-20250929", "medium": "claude-haiku-4-5-20251001", "low": "claude-haiku-4-5-20251001"},
			CostPerMToken: 3.00,
		},
		{
			Provider:      budget.ProviderOpenAI,
			Tier:          budget.TierCloud,
			Strength:      0.85,
			DailyLimit:    300000,
			WeeklyLimit:   1500000,
			DefaultModel:  "gpt-4o",
			Models:        map[string]string{"critical": "gpt-4o", "high": "gpt-4o", "medium": "gpt-4o-mini", "low": "gpt-4o-mini"},
			CostPerMToken: 2.50,
		},
		{
			Provider:      budget.ProviderGemini,
			Tier:          budget.TierCloud,
			Strength:      0.80,
			DailyLimit:    500000,
			WeeklyLimit:   2000000,
			DefaultModel:  "gemini-2.0-flash",
			Models:        map[string]string{"critical": "gemini-2.0-pro", "high": "gemini-2.0-flash", "medium": "gemini-2.0-flash", "low": "gemini-2.0-flash-lite"},
			CostPerMToken: 1.25,
		},
		{
			Provider:      budget.ProviderLocal,
			Tier:          budget.TierLocal,
			Strength:      0.45,
			DailyLimit:    0,
			WeeklyLimit:   0,
			DefaultModel:  "qwen-coder-next",
			CostPerMToken: 0.0,
		},
	}
}
