package config

import (
	"path/filepath"
	"strings"
	"testing"
)

func setBaseProfileEnv(t *testing.T) {
	t.Helper()
	// Keep configuration tests deterministic when the suite itself runs in
	// GitHub Actions; individual tests opt back into Actions validation.
	t.Setenv("GITHUB_ACTIONS", "")
	t.Setenv("GH_TOKEN", "")
	t.Setenv("GITHUB_REPOSITORY", "")
	t.Setenv("RUNNER_TEMP", "")
	t.Setenv("PROFILE_COUNT", "1")
	t.Setenv("PROFILE_1_NAME", "Momentum")
	t.Setenv("PROFILE_1_INDEX", "sp500")
	t.Setenv("PROFILE_1_MAX_STOCKS", "5")
	t.Setenv("PROFILE_1_SLACK_VALUE", "2")
	t.Setenv("PROFILE_1_INITIAL_AMOUNT_PER_STOCK", "5000")
	t.Setenv("RANKING_MODE", "mock")
}

func TestRobinhoodLiveTradingRejectsMalformedBoolean(t *testing.T) {
	setBaseProfileEnv(t)
	t.Setenv("PROFILE_1_BROKER_TYPE", "robinhood")
	t.Setenv("PROFILE_1_ROBINHOOD_LIVE_TRADING", "yes-please")
	if _, err := LoadFromEnv(); err == nil || !strings.Contains(err.Error(), "must be true or false") {
		t.Fatalf("expected boolean validation error, got %v", err)
	}
}

func TestRobinhoodProfileLoadsWithSafeDefaults(t *testing.T) {
	setBaseProfileEnv(t)
	t.Setenv("PROFILE_1_BROKER_TYPE", "robinhood")
	t.Setenv("PROFILE_1_ROBINHOOD_ACCOUNT_ID", "agent-1")
	t.Setenv("PROFILE_1_ROBINHOOD_OAUTH_STATE_FILE", filepath.Join(t.TempDir(), "oauth.json"))
	cfg, err := LoadFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	rh := cfg.Profiles[0].Broker.Robinhood
	if rh.MCPURL != DefaultRobinhoodMCPURL || rh.LiveTrading || rh.Mode != "shadow" {
		t.Fatalf("unsafe defaults: %+v", rh)
	}
}

func TestRobinhoodDuplicateAccountsAreRejected(t *testing.T) {
	setBaseProfileEnv(t)
	t.Setenv("PROFILE_COUNT", "2")
	for _, n := range []string{"1", "2"} {
		t.Setenv("PROFILE_"+n+"_NAME", "Momentum "+n)
		t.Setenv("PROFILE_"+n+"_INDEX", "sp500")
		t.Setenv("PROFILE_"+n+"_MAX_STOCKS", "5")
		t.Setenv("PROFILE_"+n+"_SLACK_VALUE", "2")
		t.Setenv("PROFILE_"+n+"_INITIAL_AMOUNT_PER_STOCK", "5000")
		t.Setenv("PROFILE_"+n+"_BROKER_TYPE", "robinhood")
		t.Setenv("PROFILE_"+n+"_ROBINHOOD_ACCOUNT_ID", "same-agent")
		t.Setenv("PROFILE_"+n+"_ROBINHOOD_OAUTH_STATE_FILE", filepath.Join(t.TempDir(), "oauth-"+n+".json"))
	}
	cfg, err := LoadFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "same Robinhood") {
		t.Fatalf("expected duplicate account error, got %v", err)
	}
}

func TestRobinhoodDuplicateOAuthStateFilesAreRejected(t *testing.T) {
	setBaseProfileEnv(t)
	t.Setenv("PROFILE_COUNT", "2")
	sharedFile := filepath.Join(t.TempDir(), "oauth.json")
	for _, n := range []string{"1", "2"} {
		t.Setenv("PROFILE_"+n+"_NAME", "Momentum "+n)
		t.Setenv("PROFILE_"+n+"_INDEX", "sp500")
		t.Setenv("PROFILE_"+n+"_MAX_STOCKS", "5")
		t.Setenv("PROFILE_"+n+"_SLACK_VALUE", "2")
		t.Setenv("PROFILE_"+n+"_INITIAL_AMOUNT_PER_STOCK", "5000")
		t.Setenv("PROFILE_"+n+"_BROKER_TYPE", "robinhood")
		t.Setenv("PROFILE_"+n+"_ROBINHOOD_ACCOUNT_ID", "agent-"+n)
		t.Setenv("PROFILE_"+n+"_ROBINHOOD_OAUTH_STATE_FILE", sharedFile)
	}
	cfg, err := LoadFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "same Robinhood OAuth") {
		t.Fatalf("expected duplicate OAuth file error, got %v", err)
	}
}

func TestRobinhoodActionsStateMustBeBelowRunnerTemp(t *testing.T) {
	setBaseProfileEnv(t)
	t.Setenv("PROFILE_1_BROKER_TYPE", "robinhood")
	t.Setenv("PROFILE_1_ROBINHOOD_ACCOUNT_ID", "agent-1")
	t.Setenv("PROFILE_1_ROBINHOOD_OAUTH_STATE_FILE", "/tmp/outside/oauth.json")
	t.Setenv("GITHUB_ACTIONS", "true")
	t.Setenv("RUNNER_TEMP", filepath.Join(t.TempDir(), "runner"))
	t.Setenv("GH_TOKEN", "installation-token")
	cfg, err := LoadFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "below RUNNER_TEMP") {
		t.Fatalf("expected path validation error, got %v", err)
	}
}

func TestRobinhoodActionsRequiresSecretWriterToken(t *testing.T) {
	setBaseProfileEnv(t)
	runnerTemp := filepath.Join(t.TempDir(), "runner")
	t.Setenv("PROFILE_1_BROKER_TYPE", "robinhood")
	t.Setenv("PROFILE_1_ROBINHOOD_ACCOUNT_ID", "agent-1")
	t.Setenv("PROFILE_1_ROBINHOOD_OAUTH_STATE_FILE", filepath.Join(runnerTemp, "oauth.json"))
	t.Setenv("GITHUB_ACTIONS", "true")
	t.Setenv("RUNNER_TEMP", runnerTemp)
	t.Setenv("GITHUB_REPOSITORY", "owner/repo")
	cfg, err := LoadFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "GH_TOKEN") {
		t.Fatalf("expected secret-writer token error, got %v", err)
	}
}
