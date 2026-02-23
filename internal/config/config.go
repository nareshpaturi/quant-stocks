// Package config loads the multi-profile configuration from environment variables.
//
// Users who fork this repo never edit code. They configure their portfolios
// entirely through GitHub Secrets and Variables:
//
//	GitHub Variables  (Settings → Secrets and Variables → Actions → Variables)
//	  PROFILE_COUNT              how many profiles to run (e.g. "3")
//	  PROFILE_1_NAME             human label  (e.g. "SP500 Momentum")
//	  PROFILE_1_INDEX            index key    (e.g. "sp500")
//	  PROFILE_1_MAX_STOCKS       target size  (e.g. "25")
//	  PROFILE_1_SLACK_VALUE      retention buffer (e.g. "5")
//	  PROFILE_1_TRADIER_SANDBOX  "true" for sandbox, "false" for live
//	  ... repeat for PROFILE_2_, PROFILE_3_, etc.
//
//	GitHub Secrets  (Settings → Secrets and Variables → Actions → Secrets)
//	  PROFILE_1_TRADIER_TOKEN       Tradier Bearer token
//	  PROFILE_1_TRADIER_ACCOUNT_ID  Tradier account number
//	  ... repeat for PROFILE_2_, PROFILE_3_, etc.
//
// Rankings are fetched automatically from the QuantMyStocks leaderboard API —
// no CSV files or external data preparation required.
package config

import (
	"fmt"
	"os"
	"strconv"
)

// Config is the root configuration holding all profiles.
type Config struct {
	Profiles        []ProfileConfig
	RankingAPIURL   string // overrides the default QuantMyStocks API URL (optional)
	RankingAPIToken string // Bearer token for the QuantMyStocks API (from GitHub Secret)
	RankingMode     string // "mock" skips the real API; empty/other uses QuantMyStocks
	Backtest        BacktestConfig
}

// BacktestConfig controls the paper-trading backtest runner.
// It is active when BACKTEST_MODE=true.
type BacktestConfig struct {
	// Enabled is true when BACKTEST_MODE=true.
	Enabled bool

	// Weeks is the number of past weeks to simulate (BACKTEST_WEEKS).
	// The runner starts from Weeks Fridays ago and runs up to the most recent Friday.
	Weeks int

	// DelaySeconds is the pause between consecutive weekly runs (BACKTEST_DELAY_SECONDS).
	// Defaults to 120 seconds to allow paper orders to settle before the next cycle.
	DelaySeconds int
}

// ProfileConfig defines one independent rebalancing portfolio.
// Each profile tracks a distinct index with its own broker account and strategy.
type ProfileConfig struct {
	// Name is a human-readable label used in log output (e.g. "SP500 Momentum").
	Name string

	// Index identifies which leaderboard to fetch.
	// Supported values: "sp500", "sp400", "sp600", "ndx".
	Index string

	// MaxStocks (N): target portfolio size.
	MaxStocks int

	// SlackValue (S): retention buffer.
	// A held stock at rank R is kept if R <= N+S, sold if R > N+S.
	SlackValue int

	// InitialAmountPerStock is the dollar amount allocated per organic buy slot.
	// Required — must be > 0. Organic slots arise when the portfolio has fewer
	// than MaxStocks positions (e.g. on first run or after an unexpected sell).
	InitialAmountPerStock float64

	Broker BrokerConfig
}

// BrokerConfig specifies which broker adapter to use and its credentials.
type BrokerConfig struct {
	// Type selects the adapter: "tradier" (default) or "mock".
	Type string

	// Token is the Tradier Bearer token (from GitHub Secret).
	Token string

	// AccountID is the Tradier account number (from GitHub Secret).
	AccountID string

	// Sandbox routes to sandbox.tradier.com when true.
	Sandbox bool
}

// LoadFromEnv reads all profile configuration from environment variables.
// Reads PROFILE_COUNT to determine how many profiles to load, then reads
// PROFILE_N_* variables for each profile N from 1 to PROFILE_COUNT.
func LoadFromEnv() (*Config, error) {
	countStr := os.Getenv("PROFILE_COUNT")
	if countStr == "" {
		return nil, fmt.Errorf(
			"GitHub Variable PROFILE_COUNT is not set — " +
				"set it to the number of index profiles you want to run (e.g. \"2\")",
		)
	}
	count, err := strconv.Atoi(countStr)
	if err != nil || count <= 0 {
		return nil, fmt.Errorf(
			"GitHub Variable PROFILE_COUNT must be a positive integer, got %q", countStr,
		)
	}

	profiles := make([]ProfileConfig, 0, count)
	for i := 1; i <= count; i++ {
		p, err := loadProfile(i)
		if err != nil {
			return nil, err
		}
		profiles = append(profiles, p)
	}

	backtest, err := loadBacktestConfig()
	if err != nil {
		return nil, err
	}

	return &Config{
		Profiles:        profiles,
		RankingAPIURL:   os.Getenv("RANKING_API_URL"),   // optional override; empty = use default
		RankingAPIToken: os.Getenv("RANKING_API_TOKEN"), // Bearer token for QuantMyStocks API
		RankingMode:     os.Getenv("RANKING_MODE"),      // "mock" for local testing without API
		Backtest:        backtest,
	}, nil
}

// loadBacktestConfig reads BACKTEST_MODE, BACKTEST_WEEKS, and BACKTEST_DELAY_SECONDS.
// Returns a zero-value BacktestConfig (Enabled=false) when BACKTEST_MODE is not "true".
func loadBacktestConfig() (BacktestConfig, error) {
	if os.Getenv("BACKTEST_MODE") != "true" {
		return BacktestConfig{}, nil
	}

	weeksStr := os.Getenv("BACKTEST_WEEKS")
	if weeksStr == "" {
		return BacktestConfig{}, fmt.Errorf(
			"BACKTEST_WEEKS must be set when BACKTEST_MODE=true (e.g. \"12\" for 12 weeks)",
		)
	}
	weeks, err := strconv.Atoi(weeksStr)
	if err != nil || weeks <= 0 {
		return BacktestConfig{}, fmt.Errorf(
			"BACKTEST_WEEKS must be a positive integer, got %q", weeksStr,
		)
	}

	delay := 120 // default: 2 minutes
	if d := os.Getenv("BACKTEST_DELAY_SECONDS"); d != "" {
		delay, err = strconv.Atoi(d)
		if err != nil || delay < 0 {
			return BacktestConfig{}, fmt.Errorf(
				"BACKTEST_DELAY_SECONDS must be a non-negative integer, got %q", d,
			)
		}
	}

	return BacktestConfig{
		Enabled:      true,
		Weeks:        weeks,
		DelaySeconds: delay,
	}, nil
}

// loadProfile reads one profile from the PROFILE_N_* env vars.
func loadProfile(n int) (ProfileConfig, error) {
	pfx := fmt.Sprintf("PROFILE_%d_", n)

	maxStocks, err := requireEnvInt(pfx+"MAX_STOCKS", n)
	if err != nil {
		return ProfileConfig{}, err
	}
	slackValue, err := requireEnvInt(pfx+"SLACK_VALUE", n)
	if err != nil {
		return ProfileConfig{}, err
	}
	initialAmount, err := requireEnvPositiveFloat(pfx+"INITIAL_AMOUNT_PER_STOCK", n)
	if err != nil {
		return ProfileConfig{}, err
	}

	return ProfileConfig{
		Name:                  os.Getenv(pfx + "NAME"),
		Index:                 os.Getenv(pfx + "INDEX"),
		MaxStocks:             maxStocks,
		SlackValue:            slackValue,
		InitialAmountPerStock: initialAmount,
		Broker: BrokerConfig{
			Type:      envOrDefault(pfx+"BROKER_TYPE", "tradier"),
			Token:     os.Getenv(pfx + "TRADIER_TOKEN"),
			AccountID: os.Getenv(pfx + "TRADIER_ACCOUNT_ID"),
			Sandbox:   os.Getenv(pfx+"TRADIER_SANDBOX") == "true",
		},
	}, nil
}

// Validate checks that every profile has all required fields set.
// Returns a clear, actionable error message pointing to the missing variable.
func (c *Config) Validate() error {
	if len(c.Profiles) == 0 {
		return fmt.Errorf("no profiles loaded — check PROFILE_COUNT")
	}

	if c.RankingMode != "mock" && c.RankingAPIToken == "" {
		return fmt.Errorf(
			"GitHub Secret RANKING_API_TOKEN is not set — " +
				"set it to your QuantMyStocks API Bearer token",
		)
	}

	if c.Backtest.Enabled {
		if len(c.Profiles) != 1 {
			return fmt.Errorf(
				"backtest mode supports exactly one profile — set PROFILE_COUNT=1",
			)
		}
		p := c.Profiles[0]
		if p.Broker.Type == "tradier" && !p.Broker.Sandbox {
			return fmt.Errorf(
				"backtest mode requires Tradier sandbox (paper trading) — " +
					"set PROFILE_1_TRADIER_SANDBOX=true",
			)
		}
	}

	seen := make(map[string]bool, len(c.Profiles))
	for i, p := range c.Profiles {
		n := i + 1
		pfx := fmt.Sprintf("PROFILE_%d_", n)

		if p.Name == "" {
			return fmt.Errorf("GitHub Variable %sNAME is not set", pfx)
		}
		if seen[p.Name] {
			return fmt.Errorf("profile %d: duplicate name %q", n, p.Name)
		}
		seen[p.Name] = true

		if p.Index == "" {
			return fmt.Errorf("GitHub Variable %sINDEX is not set (profile %q)", pfx, p.Name)
		}
		if p.MaxStocks <= 0 {
			return fmt.Errorf("GitHub Variable %sMAX_STOCKS must be > 0 (profile %q)", pfx, p.Name)
		}
		if p.SlackValue < 0 {
			return fmt.Errorf("GitHub Variable %sSLACK_VALUE must be >= 0 (profile %q)", pfx, p.Name)
		}

		switch p.Broker.Type {
		case "tradier":
			if p.Broker.Token == "" {
				return fmt.Errorf(
					"GitHub Secret %sTRADIER_TOKEN is not set (profile %q)", pfx, p.Name,
				)
			}
			if p.Broker.AccountID == "" {
				return fmt.Errorf(
					"GitHub Secret %sTRADIER_ACCOUNT_ID is not set (profile %q)", pfx, p.Name,
				)
			}
		case "mock":
			// no credentials required
		default:
			return fmt.Errorf(
				"GitHub Variable %sBROKER_TYPE is %q — must be \"tradier\" or \"mock\" (profile %q)",
				pfx, p.Broker.Type, p.Name,
			)
		}
	}
	return nil
}

func requireEnvInt(key string, profileN int) (int, error) {
	v := os.Getenv(key)
	if v == "" {
		return 0, fmt.Errorf("GitHub Variable %s is not set (profile %d)", key, profileN)
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("GitHub Variable %s must be an integer, got %q", key, v)
	}
	return n, nil
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func requireEnvPositiveFloat(key string, profileN int) (float64, error) {
	v := os.Getenv(key)
	if v == "" {
		return 0, fmt.Errorf("GitHub Variable %s is not set (profile %d)", key, profileN)
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || f <= 0 {
		return 0, fmt.Errorf("GitHub Variable %s must be a positive number, got %q", key, v)
	}
	return f, nil
}
