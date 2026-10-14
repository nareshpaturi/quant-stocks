package config

import (
	"fmt"
	"os"
	"regexp"
	"strings"
)

// OvernightConfig configures the overnight-hold strategy (cmd/overnight). It is
// loaded separately from the rebalancer profiles so either can run alone.
//
//	GitHub Variables
//	  OVERNIGHT_ENABLED          "true" allows buys; anything else only sells
//	  OVERNIGHT_SYMBOL           symbol to hold overnight (default SOXL)
//	  ALPACA_PAPER               "true" uses the paper account (default live)
//	  ALPACA_DATA_FEED           quote feed: iex (free, default) or sip
//
//	GitHub Secrets
//	  ALPACA_API_KEY_ID          Alpaca API key id
//	  ALPACA_API_SECRET_KEY      Alpaca API secret key
type OvernightConfig struct {
	Symbol  string
	Enabled bool
	DryRun  bool // OVERNIGHT_DRY_RUN, set from the workflow's dry_run input
	Alpaca  AlpacaConfig
}

// AlpacaConfig holds Alpaca credentials and endpoints. Paper and live accounts
// have separate key pairs.
type AlpacaConfig struct {
	KeyID     string
	SecretKey string
	Paper     bool
	DataFeed  string
}

var symbolPattern = regexp.MustCompile(`^[A-Z][A-Z0-9.]{0,9}$`)

// LoadOvernightFromEnv reads and validates the overnight-hold settings.
func LoadOvernightFromEnv() (OvernightConfig, error) {
	enabled, err := optionalEnvBool("OVERNIGHT_ENABLED")
	if err != nil {
		return OvernightConfig{}, err
	}
	dryRun, err := optionalEnvBool("OVERNIGHT_DRY_RUN")
	if err != nil {
		return OvernightConfig{}, err
	}
	paper, err := optionalEnvBool("ALPACA_PAPER")
	if err != nil {
		return OvernightConfig{}, err
	}

	cfg := OvernightConfig{
		Symbol:  strings.ToUpper(strings.TrimSpace(envOrDefault("OVERNIGHT_SYMBOL", "SOXL"))),
		Enabled: enabled,
		DryRun:  dryRun,
		Alpaca: AlpacaConfig{
			KeyID:     os.Getenv("ALPACA_API_KEY_ID"),
			SecretKey: os.Getenv("ALPACA_API_SECRET_KEY"),
			Paper:     paper,
			DataFeed:  strings.ToLower(envOrDefault("ALPACA_DATA_FEED", "iex")),
		},
	}
	if !symbolPattern.MatchString(cfg.Symbol) {
		return OvernightConfig{}, fmt.Errorf("GitHub Variable OVERNIGHT_SYMBOL must be a ticker symbol, got %q", cfg.Symbol)
	}
	if cfg.Alpaca.KeyID == "" {
		return OvernightConfig{}, fmt.Errorf("GitHub Secret ALPACA_API_KEY_ID is not set")
	}
	if cfg.Alpaca.SecretKey == "" {
		return OvernightConfig{}, fmt.Errorf("GitHub Secret ALPACA_API_SECRET_KEY is not set")
	}
	if cfg.Alpaca.DataFeed != "iex" && cfg.Alpaca.DataFeed != "sip" {
		return OvernightConfig{}, fmt.Errorf("GitHub Variable ALPACA_DATA_FEED must be iex or sip, got %q", cfg.Alpaca.DataFeed)
	}
	return cfg, nil
}
