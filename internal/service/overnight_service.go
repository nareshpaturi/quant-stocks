package service

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"math"
	"strings"
	"time"
	_ "time/tzdata"

	"github.com/nareshpaturi/quant-stocks/internal/domain"
	"github.com/nareshpaturi/quant-stocks/internal/port"
)

const (
	// buyLeadBeforeClose puts the buy at 15:58 ET on a normal day.
	buyLeadBeforeClose = 2 * time.Minute
	// buyCutoffBeforeClose is the last moment a market buy is sent. A market
	// order that reaches Alpaca after the close is not rejected but queued for
	// the next open, which is exactly when the position should be sold.
	buyCutoffBeforeClose = 30 * time.Second
	// maxWait refuses runs started far from their order time, so a manual run
	// at the wrong time cannot sit in a runner past the job timeout.
	maxWait = 10 * time.Minute
	// auctionCutoffBeforeOpen matches Alpaca's 09:28 ET deadline for opg orders.
	auctionCutoffBeforeOpen = 2 * time.Minute
	// openFallbackDelay is when the sell checks for shares the opening auction
	// did not take and sells them with a regular market order.
	openFallbackDelay = time.Minute
	// fillTimeout bounds how long a run waits for a market order to finish.
	fillTimeout = 2 * time.Minute
)

// OvernightConfig holds the strategy settings for one account.
type OvernightConfig struct {
	Symbol string
	// BuyEnabled false stops new buys; sells always run so a held position
	// is still closed.
	BuyEnabled bool
	// CashBufferPct is the share of cash left unspent so that a price tick
	// between the quote and the fill cannot get the buy rejected.
	CashBufferPct float64
	// DryRun runs every check and reports the order without waiting for the
	// order time or placing anything.
	DryRun bool
}

// OvernightService buys Symbol with the account's cash two minutes before the
// regular-session close and sells it in the next session's opening auction.
//
// The scheduler starts each run a few minutes early because a GitHub Actions
// run takes a minute or two to start; the run itself waits for the exact time.
// Orders carry a client order id derived from the symbol and date, so a
// repeated run finds the earlier order instead of placing a second one.
type OvernightService struct {
	broker       port.OvernightBroker
	cfg          OvernightConfig
	logger       *slog.Logger
	newYork      *time.Location
	now          func() time.Time
	sleep        func(context.Context, time.Duration) error
	pollInterval time.Duration
}

// NewOvernightService constructs the service with the wall clock.
func NewOvernightService(broker port.OvernightBroker, cfg OvernightConfig, logger *slog.Logger) (*OvernightService, error) {
	newYork, err := time.LoadLocation("America/New_York")
	if err != nil {
		return nil, fmt.Errorf("load New York timezone: %w", err)
	}
	return &OvernightService{
		broker: broker, cfg: cfg, logger: logger, newYork: newYork,
		now: time.Now, sleep: sleepContext, pollInterval: 2 * time.Second,
	}, nil
}

// OvernightReport is the human-readable outcome of one run. It is printed
// even when the run fails, so the job summary and email show how far it got.
type OvernightReport struct {
	Action string
	Symbol string
	Lines  []string
}

func (r *OvernightReport) add(format string, args ...any) {
	r.Lines = append(r.Lines, fmt.Sprintf(format, args...))
}

// Print writes the report and, when the run failed, its error.
func (r *OvernightReport) Print(w io.Writer, runErr error) {
	rule := strings.Repeat("═", 64)
	fmt.Fprintf(w, "\n%s\n  OVERNIGHT %s  %s\n%s\n", rule, strings.ToUpper(r.Action), r.Symbol, rule)
	for _, line := range r.Lines {
		fmt.Fprintf(w, "  %s\n", line)
	}
	if runErr != nil {
		fmt.Fprintf(w, "  FAILED: %v\n", runErr)
	}
	fmt.Fprintf(w, "%s\n\n", rule)
}

// Buy places today's market buy at close minus two minutes.
func (s *OvernightService) Buy(ctx context.Context) (*OvernightReport, error) {
	report := &OvernightReport{Action: "buy", Symbol: s.cfg.Symbol}
	if !s.cfg.BuyEnabled {
		report.add("Buys are disabled (OVERNIGHT_ENABLED is not true). Nothing to do.")
		return report, nil
	}

	clock, err := s.broker.GetClock(ctx)
	if err != nil {
		return report, err
	}
	if !clock.IsOpen {
		// Covers weekends, holidays, and early-close days, which close before
		// the scheduled run starts.
		report.add("Market is closed at %s. No buy today.", s.clockTime(s.now()))
		return report, nil
	}

	buyAt := clock.NextClose.Add(-buyLeadBeforeClose)
	cutoff := clock.NextClose.Add(-buyCutoffBeforeClose)
	clientID := overnightClientID(s.cfg.Symbol, "buy", buyAt.In(s.newYork))

	existing, found, err := s.broker.GetOrderByClientID(ctx, clientID)
	if err != nil {
		return report, err
	}
	if found {
		report.add("Today's buy was already placed; not placing another.")
		return report, s.finish(ctx, report, existing)
	}

	held, err := s.broker.GetPositionShares(ctx, s.cfg.Symbol)
	if err != nil {
		return report, err
	}
	if held > 0 {
		return report, fmt.Errorf("already holding %s shares of %s; this morning's sell did not close the position, so no new buy",
			formatShares(held), s.cfg.Symbol)
	}

	if !s.cfg.DryRun {
		if wait := buyAt.Sub(s.now()); wait > maxWait {
			return report, fmt.Errorf("run started %s before the %s buy time; buys are meant to start at about 15:55 ET",
				wait.Round(time.Second), s.clockTime(buyAt))
		}
		s.logger.Info("waiting for buy time", "buyAt", buyAt)
		if err := s.sleepUntil(ctx, buyAt); err != nil {
			return report, err
		}
		if now := s.now(); now.After(cutoff) {
			return report, fmt.Errorf("too late to buy: it is %s, past the %s cutoff, and a market order now would wait for the next open",
				s.clockTime(now), s.clockTime(cutoff))
		}
	}

	account, err := s.broker.GetAccount(ctx)
	if err != nil {
		return report, err
	}
	quote, err := s.broker.GetQuote(ctx, s.cfg.Symbol)
	if err != nil {
		return report, err
	}
	// Size against the higher of ask and last trade so the order never costs
	// more than the cash it was sized from.
	price := math.Max(quote.Ask, quote.Last)
	if price <= 0 {
		return report, fmt.Errorf("no usable price for %s (bid %.2f, ask %.2f, last %.2f)", s.cfg.Symbol, quote.Bid, quote.Ask, quote.Last)
	}
	// Spend cash only: on a margin account buying power is a multiple of it.
	budget := math.Min(account.Cash, account.BuyingPower) * (1 - s.cfg.CashBufferPct/100)
	shares := math.Floor(budget / price)
	if shares < 1 {
		return report, fmt.Errorf("cash $%.2f (buying power $%.2f) buys no whole shares of %s at $%.2f",
			account.Cash, account.BuyingPower, s.cfg.Symbol, price)
	}
	report.add("Cash $%.2f, buying power $%.2f, %s at $%.2f.", account.Cash, account.BuyingPower, s.cfg.Symbol, price)
	report.add("Buying %s shares (about $%.2f) with a market order.", formatShares(shares), shares*price)
	if s.cfg.DryRun {
		report.add("Dry run: order not placed.")
		return report, nil
	}

	placed, err := s.place(ctx, domain.Order{
		Ticker: s.cfg.Symbol, Side: domain.OrderSideBuy, Type: domain.OrderTypeMarket,
		Duration: domain.OrderDurationDay, Shares: shares, ClientOrderID: clientID,
		Reason: "overnight-buy",
	})
	if err != nil {
		return report, err
	}
	return report, s.finish(ctx, report, placed)
}

// Sell closes the whole position at today's open: an opening-auction order
// for the whole shares, then a market order for anything the auction left.
func (s *OvernightService) Sell(ctx context.Context) (*OvernightReport, error) {
	report := &OvernightReport{Action: "sell", Symbol: s.cfg.Symbol}
	today := s.now().In(s.newYork)
	session, ok, err := s.broker.GetSession(ctx, today.Format(time.DateOnly))
	if err != nil {
		return report, err
	}
	if !ok {
		report.add("Market is closed on %s. Nothing to sell.", today.Format(time.DateOnly))
		return report, nil
	}

	held, err := s.broker.GetPositionShares(ctx, s.cfg.Symbol)
	if err != nil {
		return report, err
	}
	if held <= 0 {
		report.add("No %s position. Nothing to sell.", s.cfg.Symbol)
		return report, nil
	}
	report.add("Holding %s shares of %s.", formatShares(held), s.cfg.Symbol)
	if wait := session.Open.Sub(s.now()); wait > maxWait && !s.cfg.DryRun {
		return report, fmt.Errorf("run started %s before the %s open; sells are meant to start at about 09:25 ET",
			wait.Round(time.Second), s.clockTime(session.Open))
	}

	auctionID := overnightClientID(s.cfg.Symbol, "sell-open", today)
	auction, found, err := s.broker.GetOrderByClientID(ctx, auctionID)
	if err != nil {
		return report, err
	}
	switch whole := math.Floor(held); {
	case found:
		report.add("Today's opening-auction sell was already placed; not placing another.")
	case whole >= 1 && s.now().Before(session.Open.Add(-auctionCutoffBeforeOpen)):
		// The auction takes whole shares only; a fractional remainder is sold
		// by the market order after the open.
		report.add("Selling %s shares in the %s opening auction.", formatShares(whole), s.clockTime(session.Open))
		if s.cfg.DryRun {
			break
		}
		auction, err = s.place(ctx, domain.Order{
			Ticker: s.cfg.Symbol, Side: domain.OrderSideSell, Type: domain.OrderTypeMarket,
			Duration: domain.OrderDurationOPG, Shares: whole, ClientOrderID: auctionID,
			Reason: "overnight-sell",
		})
		if err != nil {
			return report, err
		}
	default:
		report.add("Past the opening-auction cutoff; selling with a market order after the open.")
	}
	if s.cfg.DryRun {
		report.add("Dry run: order not placed.")
		return report, nil
	}

	s.logger.Info("waiting for the open", "open", session.Open)
	if err := s.sleepUntil(ctx, session.Open.Add(openFallbackDelay)); err != nil {
		return report, err
	}
	if auction.ID != "" {
		if auction, err = s.settleAuction(ctx, auction); err != nil {
			return report, err
		}
		report.add("Opening auction: %s", describeOrder(auction, s.newYork))
	}

	remaining, err := s.broker.GetPositionShares(ctx, s.cfg.Symbol)
	if err != nil {
		return report, err
	}
	if remaining <= 0 {
		report.add("Position closed.")
		return report, nil
	}

	marketID := overnightClientID(s.cfg.Symbol, "sell", today)
	order, found, err := s.broker.GetOrderByClientID(ctx, marketID)
	if err != nil {
		return report, err
	}
	if found {
		report.add("Today's market sell was already placed; not placing another.")
	} else {
		report.add("Selling the remaining %s shares with a market order.", formatShares(remaining))
		order, err = s.place(ctx, domain.Order{
			Ticker: s.cfg.Symbol, Side: domain.OrderSideSell, Type: domain.OrderTypeMarket,
			Duration: domain.OrderDurationDay, Shares: remaining, ClientOrderID: marketID,
			Reason: "overnight-sell",
		})
		if err != nil {
			return report, err
		}
	}
	return report, s.finish(ctx, report, order)
}

// settleAuction returns the auction order once it is no longer working. An
// order still open after the open is cancelled first: leaving it working while
// a market order sells the same shares could sell more than is held.
func (s *OvernightService) settleAuction(ctx context.Context, order domain.BrokerOrder) (domain.BrokerOrder, error) {
	order, err := s.broker.GetOrder(ctx, order.ID)
	if err != nil {
		return order, err
	}
	if !order.Active() {
		return order, nil
	}
	if err := s.broker.CancelOrder(ctx, order.ID); err != nil {
		// The order may have finished between the read and the cancel; the
		// wait below shows which.
		s.logger.Warn("cancel opening-auction order failed", "order", order.ID, "error", err)
	}
	order, err = s.awaitDone(ctx, order)
	if err != nil {
		return order, err
	}
	if order.Active() {
		return order, fmt.Errorf("opening-auction order %s is still %s after cancelling; not sending a second sell", order.ID, order.Status)
	}
	return order, nil
}

// finish waits for a market order to finish, reports it, and fails unless it
// filled completely.
func (s *OvernightService) finish(ctx context.Context, report *OvernightReport, order domain.BrokerOrder) error {
	order, err := s.awaitDone(ctx, order)
	report.add("Order: %s", describeOrder(order, s.newYork))
	if err != nil {
		return err
	}
	if order.Active() {
		return fmt.Errorf("order %s is still %s after %s", order.ID, order.Status, fillTimeout)
	}
	if order.Status != domain.BrokerOrderFilled {
		return fmt.Errorf("order %s ended %s with %s of %s shares filled",
			order.ID, order.Status, formatShares(order.FilledShares), formatShares(order.RequestedShares))
	}
	return nil
}

// awaitDone polls an order until it stops working or fillTimeout passes.
func (s *OvernightService) awaitDone(ctx context.Context, order domain.BrokerOrder) (domain.BrokerOrder, error) {
	deadline := s.now().Add(fillTimeout)
	for order.Active() && s.now().Before(deadline) {
		if err := s.sleep(ctx, s.pollInterval); err != nil {
			return order, err
		}
		next, err := s.broker.GetOrder(ctx, order.ID)
		if err != nil {
			return order, err
		}
		order = next
	}
	return order, nil
}

// place submits order. When the submission fails, Alpaca may still have
// accepted it (for example on a timeout after the request was sent), so the
// client order id is checked before reporting a failure.
func (s *OvernightService) place(ctx context.Context, order domain.Order) (domain.BrokerOrder, error) {
	placed, err := s.broker.PlaceOrder(ctx, order)
	if err == nil {
		s.logger.Info("order placed", "id", placed.ID, "clientOrderID", order.ClientOrderID,
			"side", order.Side, "shares", order.Shares, "timeInForce", order.Duration)
		return placed, nil
	}
	existing, found, lookupErr := s.broker.GetOrderByClientID(ctx, order.ClientOrderID)
	if lookupErr == nil && found {
		s.logger.Warn("order submission reported an error but the order exists", "id", existing.ID, "error", err)
		return existing, nil
	}
	return domain.BrokerOrder{}, err
}

func (s *OvernightService) sleepUntil(ctx context.Context, at time.Time) error {
	if wait := at.Sub(s.now()); wait > 0 {
		return s.sleep(ctx, wait)
	}
	return nil
}

func (s *OvernightService) clockTime(t time.Time) string {
	return t.In(s.newYork).Format("15:04:05 MST")
}

// overnightClientID names one order per symbol, purpose, and New York date.
func overnightClientID(symbol, purpose string, day time.Time) string {
	return fmt.Sprintf("overnight-%s-%s-%s", symbol, purpose, day.Format(time.DateOnly))
}

func describeOrder(order domain.BrokerOrder, location *time.Location) string {
	text := fmt.Sprintf("%s %s %s shares (%s), %s",
		order.Side, order.Ticker, formatShares(order.RequestedShares), order.Duration, order.Status)
	if order.FilledShares > 0 {
		text += fmt.Sprintf(", filled %s @ $%.2f = $%.2f",
			formatShares(order.FilledShares), order.AverageFillPrice, order.FilledValue())
	}
	if !order.CreatedAt.IsZero() {
		text += ", submitted " + order.CreatedAt.In(location).Format("15:04:05")
	}
	if order.Status == domain.BrokerOrderFilled && !order.UpdatedAt.IsZero() {
		text += ", filled " + order.UpdatedAt.In(location).Format("15:04:05")
	}
	return text
}

func formatShares(shares float64) string {
	return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.6f", shares), "0"), ".")
}

func sleepContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
