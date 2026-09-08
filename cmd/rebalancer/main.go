package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"time"

	brokeradapter "github.com/nareshpaturi/quant-stocks/internal/adapter/broker"
	robinhoodadapter "github.com/nareshpaturi/quant-stocks/internal/adapter/broker/robinhood"
	rankingadapter "github.com/nareshpaturi/quant-stocks/internal/adapter/ranking"
	"github.com/nareshpaturi/quant-stocks/internal/config"
	"github.com/nareshpaturi/quant-stocks/internal/domain"
	"github.com/nareshpaturi/quant-stocks/internal/port"
	"github.com/nareshpaturi/quant-stocks/internal/service"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))
	if len(os.Args) > 1 && os.Args[1] == "robinhood-auth" {
		if err := runRobinhoodAuth(os.Args[2:]); err != nil {
			logger.Error("Robinhood authentication failed", "error", err)
			os.Exit(1)
		}
		return
	}

	// All configuration comes from environment variables (GitHub Secrets + Variables).
	// See internal/config/config.go for the full list of required variables.
	cfg, err := config.LoadFromEnv()
	if err != nil {
		logger.Error("configuration error", "error", err)
		os.Exit(1)
	}
	if err := cfg.Validate(); err != nil {
		logger.Error("configuration error", "error", err)
		os.Exit(1)
	}

	ctx := context.Background()

	// ── Backtest mode ─────────────────────────────────────────────────────────
	if cfg.Backtest.Enabled {
		profile := cfg.Profiles[0]
		logger.Info("backtest mode",
			"profile", profile.Name,
			"index", profile.Index,
			"weeks", cfg.Backtest.Weeks,
			"delaySeconds", cfg.Backtest.DelaySeconds,
		)

		broker, err := wireBroker(ctx, profile.Broker, true) // always sandbox for backtest
		if err != nil {
			logger.Error("broker wiring failed", "error", err)
			os.Exit(1)
		}

		// Backtest always uses the live QuantMyStocks provider — historical
		// rankings are fetched by date directly from the API.
		qmsProvider := rankingadapter.NewQuantMyStocksProvider(cfg.RankingAPIURL, cfg.RankingAPIToken)

		runner := service.NewBacktestRunner(broker, qmsProvider, logger)
		if err := runner.Run(ctx, domain.PortfolioConfig{
			IndexName:             profile.Index,
			MaxStocks:             profile.MaxStocks,
			SlackValue:            profile.SlackValue,
			InitialAmountPerStock: profile.InitialAmountPerStock,
		}, cfg.Backtest.Weeks, cfg.Backtest.DelaySeconds); err != nil {
			logger.Error("backtest failed", "error", err)
			os.Exit(1)
		}
		closeBroker(broker, logger)
		return
	}

	// ── Normal rebalance mode ─────────────────────────────────────────────────
	logger.Info("profiles loaded", "count", len(cfg.Profiles))

	// Rankings are always fetched from the QuantMyStocks API.
	// All profiles share the same provider (different indexId per call).
	// RANKING_API_URL overrides the default endpoint (useful for testing).
	rankingProvider := wireRanking(cfg.RankingMode, cfg.RankingAPIURL, cfg.RankingAPIToken)

	hasError := false

	for _, profile := range cfg.Profiles {
		plog := logger.With("profile", profile.Name, "index", profile.Index)
		plog.Info("profile starting")

		broker, err := wireBroker(ctx, profile.Broker, false)
		if err != nil {
			plog.Error("broker wiring failed", "error", err)
			hasError = true
			continue
		}

		svc := service.NewRebalanceService(broker, rankingProvider, plog)
		if err := svc.Run(ctx, domain.PortfolioConfig{
			ProfileName:           profile.Name,
			IndexName:             profile.Index,
			MaxStocks:             profile.MaxStocks,
			SlackValue:            profile.SlackValue,
			InitialAmountPerStock: profile.InitialAmountPerStock,
		}); err != nil {
			plog.Error("profile rebalance failed", "error", err)
			closeBroker(broker, plog)
			hasError = true
			continue
		}
		closeBroker(broker, plog)

		plog.Info("profile complete")
	}

	if hasError {
		os.Exit(1)
	}
}

// wireBroker constructs the correct Broker adapter from a BrokerConfig.
// sandbox=true routes to sandbox.tradier.com; always false for normal rebalance runs.
func wireBroker(ctx context.Context, cfg config.BrokerConfig, sandbox bool) (port.Broker, error) {
	switch cfg.Type {
	case "tradier":
		return brokeradapter.NewTradierBroker(cfg.Token, cfg.AccountID, sandbox), nil
	case "robinhood":
		if sandbox {
			return nil, fmt.Errorf("Robinhood has no supported paper environment")
		}
		var writer robinhoodadapter.SecretWriter
		if os.Getenv("GITHUB_ACTIONS") == "true" {
			writer = robinhoodadapter.GitHubCLISecretWriter{
				Repository: os.Getenv("GITHUB_REPOSITORY"), Environment: "PROD",
				SecretName: cfg.Robinhood.OAuthSecretName,
			}
		}
		return robinhoodadapter.New(ctx, robinhoodadapter.Config{
			Endpoint: cfg.Robinhood.MCPURL, AccountID: cfg.Robinhood.AccountID,
			OAuthStateFile: cfg.Robinhood.OAuthStateFile,
			Mode:           robinhoodadapter.ExecutionMode(cfg.Robinhood.Mode),
			LiveTrading:    cfg.Robinhood.LiveTrading, SecretWriter: writer,
		})
	case "mock":
		return brokeradapter.NewMockBroker(
			[]domain.Position{
				{Ticker: "AAPL", Shares: 10, CurrentPrice: 195.50},
				{Ticker: "TSLA", Shares: 5, CurrentPrice: 250.00},
			},
			5000.00,
		), nil
	default:
		return nil, fmt.Errorf("unsupported broker type %q", cfg.Type)
	}
}

func closeBroker(broker port.Broker, logger *slog.Logger) {
	if closer, ok := broker.(interface{ Close() error }); ok {
		if err := closer.Close(); err != nil {
			logger.Warn("broker close failed", "error", err)
		}
	}
}

func runRobinhoodAuth(args []string) error {
	flags := flag.NewFlagSet("robinhood-auth", flag.ContinueOnError)
	endpoint := flags.String("mcp-url", robinhoodadapter.DefaultMCPURL, "Robinhood Trading MCP URL")
	output := flags.String("output", "", "protected OAuth state output file")
	port := flags.Int("callback-port", 3142, "localhost OAuth callback port")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *output == "" {
		return fmt.Errorf("--output is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	return robinhoodadapter.BootstrapOAuth(ctx, *endpoint, *output, *port, os.Stdout)
}

// wireRanking returns the QuantMyStocks provider for live runs, or a mock
// when mode=="mock" (useful for local testing without API access).
func wireRanking(mode, apiURL, token string) port.RankingProvider {
	if mode == "mock" {
		return rankingadapter.NewMockRankingProvider([]domain.Rank{
			{Ticker: "AAPL", Position: 1, Price: 195.50},
			{Ticker: "NVDA", Position: 2, Price: 875.00},
			{Ticker: "MSFT", Position: 3, Price: 415.20},
			{Ticker: "AMZN", Position: 4, Price: 182.00},
			{Ticker: "GOOG", Position: 5, Price: 171.00},
			{Ticker: "META", Position: 6, Price: 490.00},
			{Ticker: "NFLX", Position: 7, Price: 630.00},
		})
	}
	return rankingadapter.NewQuantMyStocksProvider(apiURL, token)
}
