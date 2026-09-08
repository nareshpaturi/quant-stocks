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
	Date         string
	Sells        []tradeRecord
	Buys         []tradeRecord
	Retains      []string
	RankByTicker map[string]int // rank position for each ticker that appeared in the leaderboard
	MaxStocks    int
	SlackValue   int
}

// tradeRecord captures the details of one executed order.
// Sells use Shares+Price; buys use Notional (broker handles share sizing).
type tradeRecord struct {
	Ticker   string
	Shares   float64 // sells only
	Price    float64 // sells only
	Notional float64 // buys only
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
//  1. Liquidate all current positions for a clean start.
//  2. For each of the past `weeks` Sundays (oldest → newest):
//     a. Fetch historical rankings for that date.
//     b. Fetch current (live) prices from the broker.
//     c. Compute orders via domain.Rebalance.
//     d. Execute paper sells, then paper buys.
//     e. Wait delaySeconds before the next week (skipped after last week).
//  3. Print a detailed summary.
func (b *BacktestRunner) Run(
	ctx context.Context,
	cfg domain.PortfolioConfig,
	weeks, delaySeconds int,
) error {
	// 1. Clean start: liquidate any existing paper positions.
	b.logger.Info("backtest: liquidating all positions for clean start")
	if err := b.liquidateAll(ctx); err != nil {
		return fmt.Errorf("backtest clean start: %w", err)
	}

	// 3. Compute the weekly date sequence.
	dates := pastSundays(weeks)
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
			Type:   domain.OrderTypeMarket,
			Shares: pos.Shares,
			Reason: "backtest-clean-start",
		}
		b.logger.Info("backtest: liquidating", "ticker", pos.Ticker, "shares", pos.Shares)
		placed, err := b.submitOrder(ctx, order)
		if err != nil {
			return fmt.Errorf("sell %s: %w", pos.Ticker, err)
		}
		b.logger.Info("backtest: liquidated", "ticker", pos.Ticker, "orderID", placed.ID)
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
	account, err := b.broker.GetAccount(ctx)
	if err != nil {
		return weekRecord{}, fmt.Errorf("get account: %w", err)
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

	// Build rank lookup for the summary.
	rankByTicker := make(map[string]int, len(rankings))
	for _, r := range rankings {
		rankByTicker[r.Ticker] = r.Position
	}

	// Compute the rebalance orders.
	result := domain.Rebalance(cfg, positions, rankings, account.BuyingPower)
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
		placed, err := b.submitOrder(ctx, order)
		if err != nil {
			b.logger.Error("backtest: sell failed", "ticker", order.Ticker, "error", err)
			continue
		}
		b.logger.Info("backtest: sell executed", "ticker", order.Ticker, "orderID", placed.ID)
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
		placed, err := b.submitOrder(ctx, order)
		if err != nil {
			b.logger.Error("backtest: buy failed", "ticker", order.Ticker, "error", err)
			continue
		}
		b.logger.Info("backtest: buy executed", "ticker", order.Ticker, "orderID", placed.ID)
		buys = append(buys, tradeRecord{
			Ticker:   order.Ticker,
			Notional: order.Notional,
		})
	}

	return weekRecord{
		Date:         date,
		Sells:        sells,
		Buys:         buys,
		Retains:      result.Retains,
		RankByTicker: rankByTicker,
		MaxStocks:    cfg.MaxStocks,
		SlackValue:   cfg.SlackValue,
	}, nil
}

func (b *BacktestRunner) submitOrder(ctx context.Context, order domain.Order) (domain.BrokerOrder, error) {
	review, err := b.broker.ReviewOrder(ctx, order)
	if err != nil {
		return domain.BrokerOrder{}, fmt.Errorf("review order: %w", err)
	}
	return b.broker.PlaceOrder(ctx, review)
}

// rankLabel returns the display string for a ticker's rank position.
// Returns "—" when the ticker did not appear in the leaderboard that week.
func rankLabel(rankByTicker map[string]int, ticker string, maxStocks, slackValue int) string {
	r, ok := rankByTicker[ticker]
	if !ok {
		return "—"
	}
	threshold := maxStocks + slackValue
	switch {
	case r <= maxStocks:
		return fmt.Sprintf("#%d(top-%d)", r, maxStocks)
	case r <= threshold:
		return fmt.Sprintf("#%d(slack)", r)
	default:
		return fmt.Sprintf("#%d(sell)", r)
	}
}

// printSummary writes a human-readable backtest summary to stdout.
func (b *BacktestRunner) printSummary(records []weekRecord) {
	totalSells := 0
	totalBuys := 0

	fmt.Printf("\n%s\n", "════════════════════════════════════════════════════════════════")
	fmt.Printf("  BACKTEST SUMMARY  (%d weeks)\n", len(records))
	fmt.Printf("%s\n\n", "════════════════════════════════════════════════════════════════")

	for i, rec := range records {
		n, s := rec.MaxStocks, rec.SlackValue
		fmt.Printf("Week %2d  %s  [N=%d  slack=%d  threshold=%d]\n", i+1, rec.Date, n, s, n+s)

		fmt.Printf("  Sells (%d):", len(rec.Sells))
		if len(rec.Sells) == 0 {
			fmt.Printf("  —")
		} else {
			for _, t := range rec.Sells {
				fmt.Printf("  %s(%s) × %.0f @ $%.2f",
					t.Ticker, rankLabel(rec.RankByTicker, t.Ticker, n, s), t.Shares, t.Price)
			}
		}
		fmt.Println()

		fmt.Printf("  Buys  (%d):", len(rec.Buys))
		if len(rec.Buys) == 0 {
			fmt.Printf("  —")
		} else {
			for _, t := range rec.Buys {
				fmt.Printf("  %s(%s) $%.0f",
					t.Ticker, rankLabel(rec.RankByTicker, t.Ticker, n, s), t.Notional)
			}
		}
		fmt.Println()

		fmt.Printf("  Keep  (%d):", len(rec.Retains))
		if len(rec.Retains) == 0 {
			fmt.Printf("  —")
		} else {
			for _, t := range rec.Retains {
				fmt.Printf("  %s(%s)", t, rankLabel(rec.RankByTicker, t, n, s))
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

// pastSundays returns a slice of date strings (YYYY-MM-DD) for the last
// `weeksBack` Sundays, ordered from oldest to most recent.
// The QuantMyStocks leaderboard is published every Sunday, so these dates
// are passed as momDay when fetching historical rankings for each week.
func pastSundays(weeksBack int) []string {
	now := time.Now().UTC()

	// time.Weekday: Sunday=0, Monday=1, ..., Saturday=6.
	// Subtracting the weekday number always lands on the most recent Sunday.
	mostRecentSunday := now.AddDate(0, 0, -int(now.Weekday()))
	startSunday := mostRecentSunday.AddDate(0, 0, -(weeksBack-1)*7)

	dates := make([]string, weeksBack)
	for i := 0; i < weeksBack; i++ {
		dates[i] = startSunday.AddDate(0, 0, i*7).Format("2006-01-02")
	}
	return dates
}
