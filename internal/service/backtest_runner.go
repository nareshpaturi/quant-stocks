package service

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	rankingadapter "github.com/nareshpaturi/quant-stocks/internal/adapter/ranking"
	"github.com/nareshpaturi/quant-stocks/internal/domain"
	"github.com/nareshpaturi/quant-stocks/internal/port"
)

// weekRecord stores the trades and retains for one backtest week's summary.
type weekRecord struct {
	Date    string
	Sells   []tradeRecord
	Buys    []tradeRecord
	Retains []string
}

// tradeRecord captures ticker, share count, and price for one order.
type tradeRecord struct {
	Ticker string
	Shares float64
	Price  float64
}

// BacktestRunner executes a multi-week paper-trading backtest.
//
// Each week it fetches the historical leaderboard for that Friday,
// runs the rebalance strategy, and executes paper orders via the broker.
// Requires market hours — paper orders only fill when the market is open.
type BacktestRunner struct {
	broker  port.Broker
	ranking *rankingadapter.QuantMyStocksProvider
	logger  *slog.Logger
}

// NewBacktestRunner constructs a BacktestRunner.
func NewBacktestRunner(
	broker port.Broker,
	ranking *rankingadapter.QuantMyStocksProvider,
	logger *slog.Logger,
) *BacktestRunner {
	return &BacktestRunner{
		broker:  broker,
		ranking: ranking,
		logger:  logger,
	}
}

// Run executes the full backtest sequence:
//
//  1. Verify market is open.
//  2. Liquidate all current positions for a clean start.
//  3. For each of the past `weeks` Fridays (oldest → newest):
//     a. Fetch historical rankings for that date.
//     b. Fetch current (live) prices from the broker.
//     c. Compute orders via domain.Rebalance.
//     d. Execute paper sells, then paper buys.
//     e. Wait delaySeconds before the next week (skipped after last week).
//  4. Print a detailed summary.
func (b *BacktestRunner) Run(
	ctx context.Context,
	cfg domain.PortfolioConfig,
	weeks, delaySeconds int,
) error {
	// 1. Market hours check — paper orders don't fill when the market is closed.
	open, err := b.broker.IsMarketOpen(ctx)
	if err != nil {
		return fmt.Errorf("check market hours: %w", err)
	}
	if !open {
		return fmt.Errorf(
			"market is not currently open — backtest requires an open market " +
				"so that paper orders can execute at live prices",
		)
	}

	// 2. Clean start: liquidate any existing paper positions.
	b.logger.Info("backtest: liquidating all positions for clean start")
	if err := b.liquidateAll(ctx); err != nil {
		return fmt.Errorf("backtest clean start: %w", err)
	}

	// 3. Compute the weekly date sequence.
	dates := pastFridays(weeks)
	b.logger.Info("backtest: starting run", "weeks", weeks, "firstDate", dates[0], "lastDate", dates[len(dates)-1])

	records := make([]weekRecord, 0, len(dates))

	for i, date := range dates {
		b.logger.Info("backtest: week starting",
			"week", i+1,
			"of", weeks,
			"date", date,
		)

		rec, err := b.runWeek(ctx, cfg, date)
		if err != nil {
			return fmt.Errorf("backtest week %d (%s): %w", i+1, date, err)
		}
		records = append(records, rec)

		// Pause between weeks (skip after the last week).
		if i < len(dates)-1 && delaySeconds > 0 {
			b.logger.Info("backtest: waiting before next week", "seconds", delaySeconds)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(delaySeconds) * time.Second):
			}
		}
	}

	// 4. Print final summary.
	b.printSummary(records)
	return nil
}

// liquidateAll sells every currently held position in the paper account.
func (b *BacktestRunner) liquidateAll(ctx context.Context) error {
	positions, err := b.broker.GetPositions(ctx)
	if err != nil {
		return fmt.Errorf("get positions: %w", err)
	}
	if len(positions) == 0 {
		b.logger.Info("backtest: no positions to liquidate")
		return nil
	}
	for _, pos := range positions {
		order := domain.Order{
			Ticker: pos.Ticker,
			Side:   domain.OrderSideSell,
			Shares: pos.Shares,
			Reason: "backtest-clean-start",
		}
		b.logger.Info("backtest: liquidating", "ticker", pos.Ticker, "shares", pos.Shares)
		orderID, err := b.broker.ExecuteOrder(ctx, order)
		if err != nil {
			return fmt.Errorf("sell %s: %w", pos.Ticker, err)
		}
		b.logger.Info("backtest: liquidated", "ticker", pos.Ticker, "orderID", orderID)
	}
	return nil
}

// runWeek executes one week of the backtest for the given historical date.
func (b *BacktestRunner) runWeek(ctx context.Context, cfg domain.PortfolioConfig, date string) (weekRecord, error) {
	// Fetch current paper account state.
	positions, err := b.broker.GetPositions(ctx)
	if err != nil {
		return weekRecord{}, fmt.Errorf("get positions: %w", err)
	}
	cash, err := b.broker.GetCash(ctx)
	if err != nil {
		return weekRecord{}, fmt.Errorf("get cash: %w", err)
	}

	// Fetch historical rankings for this Friday.
	rankings, err := b.ranking.GetRankingsForDate(ctx, cfg.IndexName, date)
	if err != nil {
		return weekRecord{}, fmt.Errorf("get rankings for %s: %w", date, err)
	}

	// Enrich rankings with current live prices from the paper broker.
	tickers := make([]string, len(rankings))
	for i, r := range rankings {
		tickers[i] = r.Ticker
	}
	quotes, err := b.broker.GetQuotes(ctx, tickers)
	if err != nil {
		return weekRecord{}, fmt.Errorf("get quotes: %w", err)
	}
	for i := range rankings {
		if p := quotes[rankings[i].Ticker]; p > 0 {
			rankings[i].Price = p
		}
	}

	// Compute the rebalance orders.
	result := domain.Rebalance(cfg, positions, rankings, cash)
	b.logger.Info("backtest: week computed",
		"date", date,
		"sells", len(result.Sells),
		"buys", len(result.Buys),
		"retains", len(result.Retains),
	)

	// Build position price map for sell records.
	posPrice := make(map[string]float64, len(positions))
	for _, p := range positions {
		posPrice[p.Ticker] = p.CurrentPrice
	}

	// Execute sells.
	sells := make([]tradeRecord, 0, len(result.Sells))
	for _, order := range result.Sells {
		b.logger.Info("backtest: submitting sell", "ticker", order.Ticker, "shares", order.Shares)
		orderID, err := b.broker.ExecuteOrder(ctx, order)
		if err != nil {
			b.logger.Error("backtest: sell failed", "ticker", order.Ticker, "error", err)
			continue
		}
		b.logger.Info("backtest: sell executed", "ticker", order.Ticker, "orderID", orderID)
		sells = append(sells, tradeRecord{
			Ticker: order.Ticker,
			Shares: order.Shares,
			Price:  posPrice[order.Ticker],
		})
	}

	// Execute buys.
	buys := make([]tradeRecord, 0, len(result.Buys))
	for _, order := range result.Buys {
		b.logger.Info("backtest: submitting buy", "ticker", order.Ticker, "shares", order.Shares)
		orderID, err := b.broker.ExecuteOrder(ctx, order)
		if err != nil {
			b.logger.Error("backtest: buy failed", "ticker", order.Ticker, "error", err)
			continue
		}
		b.logger.Info("backtest: buy executed", "ticker", order.Ticker, "orderID", orderID)
		buys = append(buys, tradeRecord{
			Ticker: order.Ticker,
			Shares: order.Shares,
			Price:  quotes[order.Ticker],
		})
	}

	return weekRecord{
		Date:    date,
		Sells:   sells,
		Buys:    buys,
		Retains: result.Retains,
	}, nil
}

// printSummary writes a human-readable backtest summary to stdout.
func (b *BacktestRunner) printSummary(records []weekRecord) {
	totalSells := 0
	totalBuys := 0

	fmt.Printf("\n%s\n", "════════════════════════════════════════════════════════════════")
	fmt.Printf("  BACKTEST SUMMARY  (%d weeks)\n", len(records))
	fmt.Printf("%s\n\n", "════════════════════════════════════════════════════════════════")

	for i, rec := range records {
		fmt.Printf("Week %2d  %s\n", i+1, rec.Date)

		fmt.Printf("  Sells (%d):", len(rec.Sells))
		if len(rec.Sells) == 0 {
			fmt.Printf("  —")
		} else {
			for _, t := range rec.Sells {
				fmt.Printf("  %s × %.0f @ $%.2f", t.Ticker, t.Shares, t.Price)
			}
		}
		fmt.Println()

		fmt.Printf("  Buys  (%d):", len(rec.Buys))
		if len(rec.Buys) == 0 {
			fmt.Printf("  —")
		} else {
			for _, t := range rec.Buys {
				fmt.Printf("  %s × %.0f @ $%.2f ($%.0f)", t.Ticker, t.Shares, t.Price, t.Shares*t.Price)
			}
		}
		fmt.Println()

		fmt.Printf("  Keep  (%d):", len(rec.Retains))
		if len(rec.Retains) == 0 {
			fmt.Printf("  —")
		} else {
			for _, t := range rec.Retains {
				fmt.Printf("  %s", t)
			}
		}
		fmt.Println()
		fmt.Println()

		totalSells += len(rec.Sells)
		totalBuys += len(rec.Buys)
	}

	fmt.Printf("%s\n", "────────────────────────────────────────────────────────────────")
	fmt.Printf("Total sell orders: %-4d  Total buy orders: %d\n", totalSells, totalBuys)
	fmt.Printf("%s\n\n", "════════════════════════════════════════════════════════════════")
}

// pastFridays returns a slice of date strings (YYYY-MM-DD) for the last
// `weeksBack` Fridays, ordered from oldest to most recent.
//
// If today is a Friday, today's date is the most recent entry.
// Otherwise, the most recent past Friday is used.
func pastFridays(weeksBack int) []string {
	now := time.Now().UTC()

	// Number of days to roll back to reach the most recent Friday.
	var daysBack int
	switch now.Weekday() {
	case time.Friday:
		daysBack = 0
	case time.Saturday:
		daysBack = 1
	case time.Sunday:
		daysBack = 2
	case time.Monday:
		daysBack = 3
	case time.Tuesday:
		daysBack = 4
	case time.Wednesday:
		daysBack = 5
	case time.Thursday:
		daysBack = 6
	}

	mostRecentFriday := now.AddDate(0, 0, -daysBack)
	startFriday := mostRecentFriday.AddDate(0, 0, -(weeksBack-1)*7)

	dates := make([]string, weeksBack)
	for i := 0; i < weeksBack; i++ {
		dates[i] = startFriday.AddDate(0, 0, i*7).Format("2006-01-02")
	}
	return dates
}
