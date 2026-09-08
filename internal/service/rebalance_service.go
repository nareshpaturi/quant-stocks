package service

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"time"
	_ "time/tzdata"

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
// Every run rebuilds the active weekly cycle from fresh positions, buying
// power, rankings, and broker order history. Only missing work is reviewed and
// placed, so retries remain safe across separate scheduled processes.
func (s *RebalanceService) Run(ctx context.Context, cfg domain.PortfolioConfig) error {
	s.logger.Info("rebalance cycle starting",
		"maxStocks", cfg.MaxStocks,
		"slackValue", cfg.SlackValue,
	)

	// 1. Fetch positions.
	positions, err := s.broker.GetPositions(ctx)
	if err != nil {
		return fmt.Errorf("get positions: %w", err)
	}
	s.logger.Info("positions fetched", "count", len(positions))

	// 2. Fetch account funds and identity.
	account, err := s.broker.GetAccount(ctx)
	if err != nil {
		return fmt.Errorf("get account: %w", err)
	}
	s.logger.Info("account fetched", "cash", account.Cash, "buyingPower", account.BuyingPower)

	// 3. Fetch rankings for the most recent Sunday in the market timezone.
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

	// 3b. Price only the decision window. Robinhood limits quotes to 20 symbols
	// per tool call and its adapter chunks this already-small list as needed.
	tickers := decisionTickers(rankings, threshold)
	quotes, err := s.broker.GetQuotes(ctx, tickers)
	if err != nil {
		return fmt.Errorf("get quotes for ranked tickers: %w", err)
	}
	for i := range rankings {
		if p := quotes[rankings[i].Ticker]; p > 0 {
			rankings[i].Price = p
		}
	}

	// 4. Read complete cycle order history and compute only missing work.
	cycleStart, cycleEnd, err := rankingCycleWindow(rankingDate)
	if err != nil {
		return fmt.Errorf("ranking cycle window: %w", err)
	}
	cycleID := domain.NewCycleID(cfg.ProfileName, account.AccountID, cfg.IndexName, rankingDate)
	orders, err := s.broker.GetOrders(ctx, domain.OrderQuery{
		AccountID: account.AccountID, CycleID: cycleID, From: cycleStart, To: cycleEnd,
	})
	if err != nil {
		return fmt.Errorf("get cycle orders: %w", err)
	}
	s.logger.Info("cycle orders fetched", "cycle", cycleID, "count", len(orders))

	result := domain.Reconcile(cfg, domain.CycleSnapshot{
		CycleID: cycleID, Account: account, Positions: positions, Rankings: rankings, Orders: orders,
	})
	s.logger.Info("rebalance reconciled",
		"sells", len(result.Sells),
		"buys", len(result.Buys),
		"retains", len(result.Retains),
		"blocked", len(result.Blocked),
	)
	for _, t := range result.Retains {
		s.logger.Info("retain (no trade)", "ticker", t)
	}

	criticalBlocked := false
	for _, blocked := range result.Blocked {
		s.logger.Warn("order blocked", "ticker", blocked.Ticker, "side", blocked.Side, "reason", blocked.Reason)
		if blocked.Reason != "insufficient buying power" {
			criticalBlocked = true
		}
	}

	// 5. Review and place missing sells first. A submitted sell is not treated as
	// filled; a later run observes the fill before allocating replacement cash.
	executionFailures := 0
	submittedOrders := make([]domain.BrokerOrder, 0, len(result.Sells)+len(result.Buys))
	for _, order := range result.Sells {
		s.logger.Info("submitting sell",
			"ticker", order.Ticker,
			"shares", order.Shares,
			"reason", order.Reason,
		)
		placed, err := s.reviewAndPlace(ctx, order)
		if err != nil {
			s.logger.Error("sell failed",
				"ticker", order.Ticker,
				"shares", order.Shares,
				"error", err,
			)
			executionFailures++
			continue
		}
		submittedOrders = append(submittedOrders, placed)
		s.logger.Info("sell submitted", "ticker", order.Ticker, "orderID", placed.ID, "status", placed.Status)
	}

	// 6. Review and place only buys funded by realized proceeds or current
	// buying power according to the reconciliation result.
	for _, order := range result.Buys {
		s.logger.Info("submitting buy",
			"ticker", order.Ticker,
			"notional", order.Notional,
			"reason", order.Reason,
		)
		placed, err := s.reviewAndPlace(ctx, order)
		if err != nil {
			s.logger.Error("buy failed",
				"ticker", order.Ticker,
				"notional", order.Notional,
				"error", err,
			)
			executionFailures++
			continue
		}
		submittedOrders = append(submittedOrders, placed)
		s.logger.Info("buy submitted", "ticker", order.Ticker, "orderID", placed.ID, "status", placed.Status)
	}

	// Refresh state so the final audit record reflects broker-accepted orders.
	summaryOrders := submittedOrders
	refreshed, refreshErr := s.broker.GetOrders(ctx, domain.OrderQuery{
		AccountID: account.AccountID, CycleID: cycleID, From: cycleStart, To: cycleEnd,
	})
	if refreshErr != nil {
		executionFailures++
		s.logger.Error("refresh cycle orders failed", "error", refreshErr)
	} else {
		summaryOrders = refreshed
		s.logger.Info("cycle state refreshed", "orders", len(refreshed))
	}

	printRebalanceSummary(cfg, domain.RebalanceResult{
		Sells: result.Sells, Buys: result.Buys, Retains: result.Retains,
	}, positions, rankings, summaryOrders, result.Blocked, executionFailures)

	s.logger.Info("rebalance cycle complete")
	if criticalBlocked {
		return fmt.Errorf("cycle %s contains broker state that cannot be reconciled safely", cycleID)
	}
	if executionFailures > 0 {
		return fmt.Errorf("cycle %s completed with %d execution failure(s)", cycleID, executionFailures)
	}
	return nil
}

func (s *RebalanceService) reviewAndPlace(ctx context.Context, order domain.Order) (domain.BrokerOrder, error) {
	review, err := s.broker.ReviewOrder(ctx, order)
	if err != nil {
		return domain.BrokerOrder{}, fmt.Errorf("review: %w", err)
	}
	if !review.Approved {
		return domain.BrokerOrder{}, fmt.Errorf("review was not approved")
	}
	if len(review.Warnings) > 0 {
		s.logger.Warn("broker review returned warnings", "ticker", order.Ticker, "count", len(review.Warnings))
	}
	return s.broker.PlaceOrder(ctx, review)
}

// lastRankingDay returns the most recent Sunday as "YYYY-MM-DD". The
// QuantMyStocks leaderboard publishes weekly on Sunday; this matches the
// computation in the adapter so the audit log reflects the date queried.
func lastRankingDay() string {
	return lastRankingDayAt(time.Now())
}

func lastRankingDayAt(now time.Time) string {
	location, err := time.LoadLocation("America/New_York")
	if err != nil {
		location = time.FixedZone("America/New_York", -5*60*60)
	}
	t := now.In(location)
	return t.AddDate(0, 0, -int(t.Weekday())).Format("2006-01-02")
}

func rankingCycleWindow(rankingDate string) (time.Time, time.Time, error) {
	location, err := time.LoadLocation("America/New_York")
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	start, err := time.ParseInLocation("2006-01-02", rankingDate, location)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	return start, start.AddDate(0, 0, 7), nil
}

func decisionTickers(rankings []domain.Rank, threshold int) []string {
	sorted := append([]domain.Rank(nil), rankings...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Position < sorted[j].Position })
	seen := make(map[string]bool)
	result := make([]string, 0, threshold)
	for _, rank := range sorted {
		if rank.Position > threshold {
			break
		}
		if !seen[rank.Ticker] {
			seen[rank.Ticker] = true
			result = append(result, rank.Ticker)
		}
	}
	return result
}

// printRebalanceSummary writes a human-readable one-cycle summary to stdout.
func printRebalanceSummary(
	cfg domain.PortfolioConfig,
	result domain.RebalanceResult,
	positions []domain.Position,
	rankings []domain.Rank,
	cycleOrders []domain.BrokerOrder,
	blocked []domain.ReconciliationBlock,
	executionFailures int,
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

	sortedOrders := append([]domain.BrokerOrder(nil), cycleOrders...)
	sort.Slice(sortedOrders, func(i, j int) bool {
		if sortedOrders[i].Ticker == sortedOrders[j].Ticker {
			return sortedOrders[i].Side < sortedOrders[j].Side
		}
		return sortedOrders[i].Ticker < sortedOrders[j].Ticker
	})
	fmt.Printf("  Cycle orders (%d):", len(sortedOrders))
	if len(sortedOrders) == 0 {
		fmt.Printf("  —")
	} else {
		for _, order := range sortedOrders {
			fmt.Printf("  %s(%s:%s %.0f/%.0f)", order.Ticker, order.Side, order.Status, order.FilledShares, order.RequestedShares)
		}
	}
	fmt.Println()

	fmt.Printf("  Blocked (%d):", len(blocked))
	if len(blocked) == 0 {
		fmt.Printf("  —")
	} else {
		for _, item := range blocked {
			fmt.Printf("  %s(%s: %s)", item.Ticker, item.Side, item.Reason)
		}
	}
	fmt.Println()
	fmt.Printf("  Execution failures: %d\n", executionFailures)
	fmt.Printf("%s\n\n", "════════════════════════════════════════════════════════════════")
}
