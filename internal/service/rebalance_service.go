package service

import (
	"context"
	"fmt"
	"log/slog"

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

	// 4. Fetch rankings.
	rankings, err := s.ranking.GetRankings(ctx, cfg.IndexName)
	if err != nil {
		return fmt.Errorf("get rankings for index %q: %w", cfg.IndexName, err)
	}
	s.logger.Info("rankings fetched", "count", len(rankings))

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
			"shares", order.Shares,
			"reason", order.Reason,
		)
		orderID, err := s.broker.ExecuteOrder(ctx, order)
		if err != nil {
			s.logger.Error("buy failed",
				"ticker", order.Ticker,
				"shares", order.Shares,
				"error", err,
			)
			continue
		}
		s.logger.Info("buy executed", "ticker", order.Ticker, "orderID", orderID)
	}

	s.logger.Info("rebalance cycle complete")
	return nil
}
