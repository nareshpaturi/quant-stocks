// Command overnight runs one leg of the overnight-hold strategy on Alpaca:
//
//	overnight buy   started at 15:55 ET; buys with the account's cash at 15:58
//	overnight sell  started at 09:25 ET; sells the position at the 09:30 open
//
// Configuration comes from environment variables; see internal/config/overnight.go.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/nareshpaturi/quant-stocks/internal/adapter/broker/alpaca"
	"github.com/nareshpaturi/quant-stocks/internal/config"
	"github.com/nareshpaturi/quant-stocks/internal/service"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))
	if len(os.Args) != 2 || (os.Args[1] != "buy" && os.Args[1] != "sell") {
		fmt.Fprintln(os.Stderr, "usage: overnight buy|sell")
		os.Exit(2)
	}
	action := os.Args[1]

	cfg, err := config.LoadOvernightFromEnv()
	if err != nil {
		logger.Error("configuration error", "error", err)
		os.Exit(1)
	}
	broker, err := alpaca.New(cfg.Alpaca.KeyID, cfg.Alpaca.SecretKey, cfg.Alpaca.Paper, cfg.Alpaca.DataFeed)
	if err != nil {
		logger.Error("broker wiring failed", "error", err)
		os.Exit(1)
	}
	svc, err := service.NewOvernightService(broker, service.OvernightConfig{
		Symbol: cfg.Symbol, BuyEnabled: cfg.Enabled,
		CashBufferPct: cfg.CashBufferPct, DryRun: cfg.DryRun,
	}, logger.With("action", action, "symbol", cfg.Symbol))
	if err != nil {
		logger.Error("service wiring failed", "error", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	logger.Info("overnight run starting", "action", action, "symbol", cfg.Symbol,
		"paper", cfg.Alpaca.Paper, "buyEnabled", cfg.Enabled, "dryRun", cfg.DryRun)
	var report *service.OvernightReport
	if action == "buy" {
		report, err = svc.Buy(ctx)
	} else {
		report, err = svc.Sell(ctx)
	}
	report.Print(os.Stdout, err)
	if err != nil {
		logger.Error("overnight run failed", "error", err)
		os.Exit(1)
	}
}
