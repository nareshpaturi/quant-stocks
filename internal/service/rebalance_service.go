package service

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/nareshpaturi/quant-stocks/internal/domain"
	"github.com/nareshpaturi/quant-stocks/internal/port"
)

// RebalanceService orchestrates one full rebalancing cycle.
//
// It depends on the Broker and RankingProvider interfaces, never on concrete
// adapters, so it remains fully decoupled from external systems.
type RebalanceService struct {
	broker  port.Broker
	ranking port.RankingProvider
	logger  *slog.Logger
}

// NewRebalanceService constructs the service with its dependencies.
func NewRebalanceService(
	broker port.Broker,
	ranking port.RankingProvider,
	logger *slog.Logger,
) *RebalanceService {
	return &RebalanceService{
		broker:  broker,
		ranking: ranking,
		logger:  logger,
	}
}

// Run executes one full rebalance cycle for the given PortfolioConfig.
//
// Execution order:
//  1. Fetch current positions (with prices) from broker.
//  2. Fetch available cash from broker.
//  3. Fetch open orders from broker to build idempotency guard.
//  4. Fetch ranked stock list from ranking provider.
//  5. Call pure domain.Rebalance() to compute the order set.
//  6. Execute SELLS first, skipping any already-open sell orders.
//  7. Execute BUYS, skipping any already-open buy orders.
//
// Steps 1–4 return hard errors. Individual order failures are logged and
// skipped so the portfolio reaches a partially rebalanced state rather than
// no change at all.
func (s *RebalanceService) Run(ctx context.Context, cfg domain.PortfolioConfig) error {
	s.logger.Info("rebalance cycle starting",
		"index", cfg.IndexName,
		"maxStocks", cfg.MaxStocks,
		"slackValue", cfg.SlackValue,
	)

	// 1. Fetch positions.
	positions, err := s.broker.GetPositions(ctx)
	if err != nil {
		return fmt.Errorf("get positions: %w", err)
	}
	s.logger.Info("positions fetched", "count", len(positions))

	// 2. Fetch available cash.
	cash, err := s.broker.GetCash(ctx)
	if err != nil {
		return fmt.Errorf("get cash: %w", err)
	}
	s.logger.Info("cash balance fetched", "cash", cash)

	// 3. Fetch open orders and build idempotency guard.
	openOrders, err := s.broker.GetOpenOrders(ctx)
	if err != nil {
		return fmt.Errorf("get open orders: %w", err)
	}
	alreadyPending := make(map[string]bool, len(openOrders))
	for _, o := range openOrders {
		alreadyPending[o.PendingKey()] = true
	}
	s.logger.Info("open orders fetched", "count", len(openOrders))

	// 4. Fetch rankings for the most recent Sunday (when QuantMyStocks publishes).
	rankingDate := lastRankingDay()
	rankings, err := s.ranking.GetRankingsForDate(ctx, cfg.IndexName, rankingDate)
	if err != nil {
		return fmt.Errorf("get rankings for index %q on %s: %w", cfg.IndexName, rankingDate, err)
	}

	// Audit snapshot: log the date queried and the top N+slack entries from the
	// ranks API response (index is on every line via the parent logger). These
	// are the only ranks that drive trade decisions below, so this is the
	// point-in-time record to reconstruct why a given order was placed.
	threshold := cfg.MaxStocks + cfg.SlackValue
	sortedTop := make([]domain.Rank, len(rankings))
	copy(sortedTop, rankings)
	sort.Slice(sortedTop, func(i, j int) bool { return sortedTop[i].Position < sortedTop[j].Position })
	if len(sortedTop) > threshold {
		sortedTop = sortedTop[:threshold]
	}
	topEntries := make([]string, len(sortedTop))
	for i, r := range sortedTop {
		topEntries[i] = fmt.Sprintf("%s:%d", r.Ticker, r.Position)
	}
	s.logger.Info("rankings fetched",
		"date", rankingDate,
		"count", len(rankings),
		"threshold", threshold,
		"top", topEntries,
	)

	// 4b. Enrich rankings with current prices from the broker.
	// The QuantMyStocks API does not return prices; the broker quotes endpoint
	// provides them. If a ranking already has a non-zero price (e.g. from a
	// mock provider) it is preserved so mock runs stay self-contained.
	tickers := make([]string, len(rankings))
	for i, r := range rankings {
		tickers[i] = r.Ticker
	}
	quotes, err := s.broker.GetQuotes(ctx, tickers)
	if err != nil {
		return fmt.Errorf("get quotes for ranked tickers: %w", err)
	}
	for i := range rankings {
		if p := quotes[rankings[i].Ticker]; p > 0 {
			rankings[i].Price = p
		}
	}

	// 5. Compute orders via pure domain logic.
	result := domain.Rebalance(cfg, positions, rankings, cash)
	s.logger.Info("rebalance computed",
		"sells", len(result.Sells),
		"buys", len(result.Buys),
		"retains", len(result.Retains),
	)
	for _, t := range result.Retains {
		s.logger.Info("retain (no trade)", "ticker", t)
	}

	// 6. Execute sells first (critical: free cash before buying).
	for _, order := range result.Sells {
		key := (domain.OpenOrder{Ticker: order.Ticker, Side: order.Side}).PendingKey()
		if alreadyPending[key] {
			s.logger.Info("sell skipped (already open)", "ticker", order.Ticker)
			continue
		}
		s.logger.Info("submitting sell",
			"ticker", order.Ticker,
			"shares", order.Shares,
			"reason", order.Reason,
		)
		orderID, err := s.broker.ExecuteOrder(ctx, order)
		if err != nil {
			s.logger.Error("sell failed",
				"ticker", order.Ticker,
				"shares", order.Shares,
				"error", err,
			)
			continue
		}
		s.logger.Info("sell executed", "ticker", order.Ticker, "orderID", orderID)
	}

	// 7. Execute buys.
	for _, order := range result.Buys {
		key := (domain.OpenOrder{Ticker: order.Ticker, Side: order.Side}).PendingKey()
		if alreadyPending[key] {
			s.logger.Info("buy skipped (already open)", "ticker", order.Ticker)
			continue
		}
		s.logger.Info("submitting buy",
			"ticker", order.Ticker,
			"notional", order.Notional,
			"reason", order.Reason,
		)
		orderID, err := s.broker.ExecuteOrder(ctx, order)
		if err != nil {
			s.logger.Error("buy failed",
				"ticker", order.Ticker,
				"notional", order.Notional,
				"error", err,
			)
			continue
		}
		s.logger.Info("buy executed", "ticker", order.Ticker, "orderID", orderID)
	}

	// Print human-readable summary to stdout (same format as backtest).
	printRebalanceSummary(cfg, result, positions, rankings)

	s.logger.Info("rebalance cycle complete")
	return nil
}

// lastRankingDay returns the most recent Sunday as "YYYY-MM-DD". The
// QuantMyStocks leaderboard publishes weekly on Sunday; this matches the
// computation in the adapter so the audit log reflects the date queried.
func lastRankingDay() string {
	t := time.Now().UTC()
	return t.AddDate(0, 0, -int(t.Weekday())).Format("2006-01-02")
}

// printRebalanceSummary writes a human-readable one-cycle summary to stdout.
func printRebalanceSummary(
	cfg domain.PortfolioConfig,
	result domain.RebalanceResult,
	positions []domain.Position,
	rankings []domain.Rank,
) {
	n, s := cfg.MaxStocks, cfg.SlackValue

	rankByTicker := make(map[string]int, len(rankings))
	for _, r := range rankings {
		rankByTicker[r.Ticker] = r.Position
	}

	posPrice := make(map[string]float64, len(positions))
	for _, p := range positions {
		posPrice[p.Ticker] = p.CurrentPrice
	}

	threshold := n + s
	sortedTop := make([]domain.Rank, len(rankings))
	copy(sortedTop, rankings)
	sort.Slice(sortedTop, func(i, j int) bool { return sortedTop[i].Position < sortedTop[j].Position })
	if len(sortedTop) > threshold {
		sortedTop = sortedTop[:threshold]
	}

	fmt.Printf("\n%s\n", "════════════════════════════════════════════════════════════════")
	fmt.Printf("  REBALANCE SUMMARY  %s  [N=%d  slack=%d  threshold=%d]\n", cfg.IndexName, n, s, threshold)
	fmt.Printf("%s\n", "════════════════════════════════════════════════════════════════")

	fmt.Printf("  Ranks (%d):", len(sortedTop))
	if len(sortedTop) == 0 {
		fmt.Printf("  —")
	} else {
		for _, r := range sortedTop {
			fmt.Printf("  %s:%d", r.Ticker, r.Position)
		}
	}
	fmt.Println()

	fmt.Printf("  Sells (%d):", len(result.Sells))
	if len(result.Sells) == 0 {
		fmt.Printf("  —")
	} else {
		for _, o := range result.Sells {
			fmt.Printf("  %s(%s) × %.0f @ $%.2f",
				o.Ticker, rankLabel(rankByTicker, o.Ticker, n, s), o.Shares, posPrice[o.Ticker])
		}
	}
	fmt.Println()

	fmt.Printf("  Buys  (%d):", len(result.Buys))
	if len(result.Buys) == 0 {
		fmt.Printf("  —")
	} else {
		for _, o := range result.Buys {
			fmt.Printf("  %s(%s) $%.0f", o.Ticker, rankLabel(rankByTicker, o.Ticker, n, s), o.Notional)
		}
	}
	fmt.Println()

	fmt.Printf("  Keep  (%d):", len(result.Retains))
	if len(result.Retains) == 0 {
		fmt.Printf("  —")
	} else {
		for _, t := range result.Retains {
			fmt.Printf("  %s(%s)", t, rankLabel(rankByTicker, t, n, s))
		}
	}
	fmt.Println()
	fmt.Printf("%s\n\n", "════════════════════════════════════════════════════════════════")
}
