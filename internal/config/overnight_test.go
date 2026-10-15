package config

import (
	"strings"
	"testing"
)

func setOvernightEnv(t *testing.T, values map[string]string) {
	t.Helper()
	for _, key := range []string{
		"OVERNIGHT_ENABLED", "OVERNIGHT_DRY_RUN", "OVERNIGHT_SYMBOL",
		"ALPACA_PAPER", "ALPACA_DATA_FEED", "ALPACA_API_KEY_ID", "ALPACA_API_SECRET_KEY",
	} {
		t.Setenv(key, values[key])
	}
}

func TestLoadOvernightDefaults(t *testing.T) {
	setOvernightEnv(t, map[string]string{"ALPACA_API_KEY_ID": "id", "ALPACA_API_SECRET_KEY": "secret"})
	cfg, err := LoadOvernightFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Symbol != "SOXL" || cfg.Enabled || cfg.DryRun {
		t.Fatalf("cfg=%+v", cfg)
	}
	if cfg.Alpaca.Paper || cfg.Alpaca.DataFeed != "iex" {
		t.Fatalf("alpaca=%+v, want live with iex", cfg.Alpaca)
	}
}

func TestLoadOvernightReadsVariables(t *testing.T) {
	setOvernightEnv(t, map[string]string{
		"ALPACA_API_KEY_ID": "id", "ALPACA_API_SECRET_KEY": "secret",
		"OVERNIGHT_ENABLED": "true", "OVERNIGHT_DRY_RUN": "true", "OVERNIGHT_SYMBOL": "tqqq",
		"ALPACA_PAPER": "true", "ALPACA_DATA_FEED": "SIP",
	})
	cfg, err := LoadOvernightFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Symbol != "TQQQ" || !cfg.Enabled || !cfg.DryRun || !cfg.Alpaca.Paper || cfg.Alpaca.DataFeed != "sip" {
		t.Fatalf("cfg=%+v", cfg)
	}
}

func TestLoadOvernightValidation(t *testing.T) {
	tests := map[string]struct {
		env  map[string]string
		want string
	}{
		"missing key id":   {map[string]string{"ALPACA_API_SECRET_KEY": "s"}, "ALPACA_API_KEY_ID"},
		"missing secret":   {map[string]string{"ALPACA_API_KEY_ID": "i"}, "ALPACA_API_SECRET_KEY"},
		"bad enabled":      {map[string]string{"ALPACA_API_KEY_ID": "i", "ALPACA_API_SECRET_KEY": "s", "OVERNIGHT_ENABLED": "yes"}, "OVERNIGHT_ENABLED"},
		"bad symbol":       {map[string]string{"ALPACA_API_KEY_ID": "i", "ALPACA_API_SECRET_KEY": "s", "OVERNIGHT_SYMBOL": "SO XL"}, "OVERNIGHT_SYMBOL"},
		"unsupported feed": {map[string]string{"ALPACA_API_KEY_ID": "i", "ALPACA_API_SECRET_KEY": "s", "ALPACA_DATA_FEED": "otc"}, "ALPACA_DATA_FEED"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			setOvernightEnv(t, tt.env)
			if _, err := LoadOvernightFromEnv(); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err=%v, want mention of %s", err, tt.want)
			}
		})
	}
}
