package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/nareshpaturi/quant-stocks/internal/domain"
)

var testNewYork = func() *time.Location {
	location, err := time.LoadLocation("America/New_York")
	if err != nil {
		panic(err)
	}
	return location
}()

func et(value string) time.Time {
	t, err := time.ParseInLocation("2006-01-02 15:04:05", value, testNewYork)
	if err != nil {
		panic(err)
	}
	return t
}

type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time { return c.t }

func (c *fakeClock) sleep(_ context.Context, d time.Duration) error {
	c.t = c.t.Add(d)
	return nil
}

// placedOrder records an order and the fake time it was submitted at.
type placedOrder struct {
	domain.Order
	at time.Time
}

// fakeOvernightBroker fills day market orders on the first poll. Opening
// auction orders follow auctionOutcome once the clock reaches the open.
type fakeOvernightBroker struct {
	clock     *fakeClock
	market    domain.MarketClock
	session   domain.MarketSession
	sessionOK bool
	account   domain.AccountSnapshot
	position  float64
	quote     domain.Quote
	price     float64 // fill price

	notFractionable bool

	// auctionOutcome is "fill" (default), or "stuck" to stay open until cancelled.
	auctionOutcome string
	// placeErr makes PlaceOrder report an error after accepting the order.
	placeErr error

	orders   map[string]domain.BrokerOrder
	byClient map[string]string
	placed   []placedOrder
	canceled []string
}

func newFakeOvernightBroker(clock *fakeClock) *fakeOvernightBroker {
	return &fakeOvernightBroker{
		clock: clock, price: 40, orders: map[string]domain.BrokerOrder{}, byClient: map[string]string{},
	}
}

func (f *fakeOvernightBroker) GetClock(context.Context) (domain.MarketClock, error) {
	return f.market, nil
}

func (f *fakeOvernightBroker) GetSession(_ context.Context, date string) (domain.MarketSession, bool, error) {
	if !f.sessionOK || f.session.Date != date {
		return domain.MarketSession{}, false, nil
	}
	return f.session, true, nil
}

func (f *fakeOvernightBroker) GetAccount(context.Context) (domain.AccountSnapshot, error) {
	return f.account, nil
}

func (f *fakeOvernightBroker) GetPositionShares(context.Context, string) (float64, error) {
	return f.position, nil
}

func (f *fakeOvernightBroker) GetQuote(context.Context, string) (domain.Quote, error) {
	return f.quote, nil
}

func (f *fakeOvernightBroker) IsFractionable(context.Context, string) (bool, error) {
	return !f.notFractionable, nil
}

func (f *fakeOvernightBroker) PlaceOrder(_ context.Context, order domain.Order) (domain.BrokerOrder, error) {
	if _, exists := f.byClient[order.ClientOrderID]; exists {
		return domain.BrokerOrder{}, errors.New("client_order_id must be unique")
	}
	id := fmt.Sprintf("order-%d", len(f.placed)+1)
	f.placed = append(f.placed, placedOrder{Order: order, at: f.clock.now()})
	placed := domain.BrokerOrder{
		ID: id, ClientOrderID: order.ClientOrderID, Ticker: order.Ticker, Side: order.Side,
		Type: order.Type, Duration: order.Duration, RequestedShares: order.Shares,
		RemainingShares: order.Shares, EstimatedNotional: order.Notional,
		Status: domain.BrokerOrderPending, CreatedAt: f.clock.now(),
	}
	f.orders[id] = placed
	f.byClient[order.ClientOrderID] = id
	if f.placeErr != nil {
		return domain.BrokerOrder{}, f.placeErr
	}
	return placed, nil
}

func (f *fakeOvernightBroker) GetOrder(_ context.Context, id string) (domain.BrokerOrder, error) {
	order := f.orders[id]
	if !order.Active() {
		return order, nil
	}
	if order.Duration == domain.OrderDurationOPG {
		if f.clock.now().Before(f.session.Open) || f.auctionOutcome == "stuck" {
			return order, nil
		}
	}
	f.fill(&order)
	return order, nil
}

func (f *fakeOvernightBroker) GetOrderByClientID(ctx context.Context, clientOrderID string) (domain.BrokerOrder, bool, error) {
	id, ok := f.byClient[clientOrderID]
	if !ok {
		return domain.BrokerOrder{}, false, nil
	}
	order, err := f.GetOrder(ctx, id)
	return order, true, err
}

func (f *fakeOvernightBroker) CancelOrder(_ context.Context, id string) error {
	f.canceled = append(f.canceled, id)
	order := f.orders[id]
	order.Status = domain.BrokerOrderCanceled
	order.RemainingShares = 0
	f.orders[id] = order
	return nil
}

func (f *fakeOvernightBroker) fill(order *domain.BrokerOrder) {
	order.Status = domain.BrokerOrderFilled
	order.FilledShares = order.RequestedShares
	if order.RequestedShares == 0 {
		order.FilledShares = order.EstimatedNotional / f.price
	}
	order.RemainingShares = 0
	order.AverageFillPrice = f.price
	order.UpdatedAt = f.clock.now()
	f.orders[order.ID] = *order
	if order.Side == domain.OrderSideBuy {
		f.position += order.FilledShares
	} else {
		f.position -= order.FilledShares
	}
}

func newTestOvernightService(broker *fakeOvernightBroker, cfg OvernightConfig) *OvernightService {
	if cfg.Symbol == "" {
		cfg.Symbol = "SOXL"
	}
	return &OvernightService{
		broker: broker, cfg: cfg, logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		newYork: testNewYork, now: broker.clock.now, sleep: broker.clock.sleep,
		pollInterval: 2 * time.Second,
	}
}

// buyDay is Tuesday 2026-10-13 with the run starting at 15:55:20 EDT.
func buyDay() *fakeOvernightBroker {
	broker := newFakeOvernightBroker(&fakeClock{t: et("2026-10-13 15:55:20")})
	broker.market = domain.MarketClock{
		IsOpen: true, NextClose: et("2026-10-13 16:00:00"), NextOpen: et("2026-10-14 09:30:00"),
	}
	broker.account = domain.AccountSnapshot{Cash: 10000, BuyingPower: 20000}
	broker.quote = domain.Quote{Bid: 40.05, Ask: 40.10, Last: 40.00}
	return broker
}

// sellDay is Wednesday 2026-10-14 with the run starting at 09:25:30 EDT.
func sellDay(position float64) *fakeOvernightBroker {
	broker := newFakeOvernightBroker(&fakeClock{t: et("2026-10-14 09:25:30")})
	broker.session = domain.MarketSession{
		Date: "2026-10-14", Open: et("2026-10-14 09:30:00"), Close: et("2026-10-14 16:00:00"),
	}
	broker.sessionOK = true
	broker.position = position
	return broker
}

func TestBuyWaitsUntilCloseMinusTwoMinutesAndSpendsCash(t *testing.T) {
	broker := buyDay()
	report, err := newTestOvernightService(broker, OvernightConfig{BuyEnabled: true}).Buy(context.Background())
	if err != nil {
		t.Fatalf("Buy() error: %v", err)
	}
	if len(broker.placed) != 1 {
		t.Fatalf("placed %d orders, want 1", len(broker.placed))
	}
	got := broker.placed[0]
	// All $10,000 of cash, not the $20,000 margin buying power.
	if got.Side != domain.OrderSideBuy || got.Notional != 10000 || got.Shares != 0 || got.Duration != domain.OrderDurationDay {
		t.Fatalf("placed %+v, want $10000 market day buy", got.Order)
	}
	if got.ClientOrderID != "overnight-SOXL-buy-2026-10-13" {
		t.Fatalf("ClientOrderID=%q", got.ClientOrderID)
	}
	if !got.at.Equal(et("2026-10-13 15:58:00")) {
		t.Fatalf("placed at %s, want 15:58:00 ET", got.at.In(testNewYork))
	}
	if broker.position != 250 {
		t.Fatalf("position=%v, want 250", broker.position)
	}
	if !strings.Contains(strings.Join(report.Lines, "\n"), "buy SOXL $10000.00 (day), filled, filled 250 @ $40.00") {
		t.Fatalf("report does not show the fill:\n%s", strings.Join(report.Lines, "\n"))
	}
}

func TestBuyWholeSharesWhenSymbolIsNotFractionable(t *testing.T) {
	broker := buyDay()
	broker.notFractionable = true
	if _, err := newTestOvernightService(broker, OvernightConfig{BuyEnabled: true}).Buy(context.Background()); err != nil {
		t.Fatalf("Buy() error: %v", err)
	}
	// $10,000 less the 1% cushion at the $40.10 ask, the higher of ask and
	// last: floor(246.88) = 246.
	if len(broker.placed) != 1 || broker.placed[0].Shares != 246 || broker.placed[0].Notional != 0 {
		t.Fatalf("placed %+v, want a 246-share buy", broker.placed)
	}
}

func TestBuyRefusesDollarOrderBelowMinimum(t *testing.T) {
	broker := buyDay()
	broker.account = domain.AccountSnapshot{Cash: 0.5, BuyingPower: 0.5}
	_, err := newTestOvernightService(broker, OvernightConfig{BuyEnabled: true}).Buy(context.Background())
	if err == nil || !strings.Contains(err.Error(), "minimum dollar order") {
		t.Fatalf("Buy() error=%v, want minimum", err)
	}
	if len(broker.placed) != 0 {
		t.Fatalf("placed %d orders", len(broker.placed))
	}
}

func TestBuySkipsWhenMarketIsClosed(t *testing.T) {
	// An early-close day: the market closed at 13:00, before the 15:55 run.
	broker := buyDay()
	broker.market = domain.MarketClock{IsOpen: false, NextOpen: et("2026-11-30 09:30:00")}
	if _, err := newTestOvernightService(broker, OvernightConfig{BuyEnabled: true}).Buy(context.Background()); err != nil {
		t.Fatalf("Buy() error: %v", err)
	}
	if len(broker.placed) != 0 {
		t.Fatalf("placed %d orders on a closed market", len(broker.placed))
	}
}

func TestBuyRefusesAfterCutoff(t *testing.T) {
	broker := buyDay()
	broker.clock.t = et("2026-10-13 15:59:31")
	_, err := newTestOvernightService(broker, OvernightConfig{BuyEnabled: true}).Buy(context.Background())
	if err == nil || !strings.Contains(err.Error(), "too late") {
		t.Fatalf("Buy() error=%v, want too late", err)
	}
	if len(broker.placed) != 0 {
		t.Fatalf("placed %d orders after the cutoff", len(broker.placed))
	}
}

func TestBuyLateRunBeforeCutoffBuysImmediately(t *testing.T) {
	broker := buyDay()
	broker.clock.t = et("2026-10-13 15:59:10")
	if _, err := newTestOvernightService(broker, OvernightConfig{BuyEnabled: true}).Buy(context.Background()); err != nil {
		t.Fatalf("Buy() error: %v", err)
	}
	if len(broker.placed) != 1 || !broker.placed[0].at.Equal(et("2026-10-13 15:59:10")) {
		t.Fatalf("placed %+v, want one order at 15:59:10", broker.placed)
	}
}

func TestBuyRefusesRunStartedHoursEarly(t *testing.T) {
	broker := buyDay()
	broker.clock.t = et("2026-10-13 11:00:00")
	_, err := newTestOvernightService(broker, OvernightConfig{BuyEnabled: true}).Buy(context.Background())
	if err == nil || !strings.Contains(err.Error(), "before the") {
		t.Fatalf("Buy() error=%v, want early-run refusal", err)
	}
	if len(broker.placed) != 0 {
		t.Fatalf("placed %d orders", len(broker.placed))
	}
}

func TestBuyRefusesWhilePositionIsHeld(t *testing.T) {
	broker := buyDay()
	broker.position = 120
	_, err := newTestOvernightService(broker, OvernightConfig{BuyEnabled: true}).Buy(context.Background())
	if err == nil || !strings.Contains(err.Error(), "already holding 120") {
		t.Fatalf("Buy() error=%v, want already holding", err)
	}
	if len(broker.placed) != 0 {
		t.Fatalf("placed %d orders", len(broker.placed))
	}
}

func TestBuyRerunFindsTodaysOrder(t *testing.T) {
	broker := buyDay()
	svc := newTestOvernightService(broker, OvernightConfig{BuyEnabled: true})
	if _, err := svc.Buy(context.Background()); err != nil {
		t.Fatalf("first Buy() error: %v", err)
	}
	if _, err := svc.Buy(context.Background()); err != nil {
		t.Fatalf("second Buy() error: %v", err)
	}
	if len(broker.placed) != 1 {
		t.Fatalf("placed %d orders across two runs, want 1", len(broker.placed))
	}
}

func TestBuyAcceptsOrderWhoseSubmissionReportedAnError(t *testing.T) {
	broker := buyDay()
	broker.placeErr = errors.New("timeout awaiting response headers")
	if _, err := newTestOvernightService(broker, OvernightConfig{BuyEnabled: true}).Buy(context.Background()); err != nil {
		t.Fatalf("Buy() error: %v", err)
	}
	if len(broker.placed) != 1 || broker.placed[0].Notional != 10000 || broker.position != 250 {
		t.Fatalf("placed %d orders, position %v", len(broker.placed), broker.position)
	}
}

func TestBuyDisabledDoesNothing(t *testing.T) {
	broker := buyDay()
	if _, err := newTestOvernightService(broker, OvernightConfig{}).Buy(context.Background()); err != nil {
		t.Fatalf("Buy() error: %v", err)
	}
	if len(broker.placed) != 0 {
		t.Fatalf("placed %d orders while disabled", len(broker.placed))
	}
}

func TestBuyDryRunNeitherWaitsNorPlaces(t *testing.T) {
	broker := buyDay()
	report, err := newTestOvernightService(broker, OvernightConfig{BuyEnabled: true, DryRun: true}).Buy(context.Background())
	if err != nil {
		t.Fatalf("Buy() error: %v", err)
	}
	if len(broker.placed) != 0 || !broker.clock.t.Equal(et("2026-10-13 15:55:20")) {
		t.Fatalf("dry run placed %d orders and moved the clock to %s", len(broker.placed), broker.clock.t)
	}
	if !strings.Contains(strings.Join(report.Lines, "\n"), "Buying $10000.00 of SOXL") {
		t.Fatalf("report does not size the order:\n%s", strings.Join(report.Lines, "\n"))
	}
}

func TestSellUsesOpeningAuction(t *testing.T) {
	broker := sellDay(246)
	report, err := newTestOvernightService(broker, OvernightConfig{}).Sell(context.Background())
	if err != nil {
		t.Fatalf("Sell() error: %v", err)
	}
	if len(broker.placed) != 1 {
		t.Fatalf("placed %d orders, want 1", len(broker.placed))
	}
	got := broker.placed[0]
	if got.Side != domain.OrderSideSell || got.Shares != 246 || got.Duration != domain.OrderDurationOPG {
		t.Fatalf("placed %+v, want opening-auction sell of 246", got.Order)
	}
	if got.ClientOrderID != "overnight-SOXL-sell-open-2026-10-14" {
		t.Fatalf("ClientOrderID=%q", got.ClientOrderID)
	}
	if broker.position != 0 {
		t.Fatalf("position=%v, want 0", broker.position)
	}
	if !strings.Contains(strings.Join(report.Lines, "\n"), "Position closed.") {
		t.Fatalf("report:\n%s", strings.Join(report.Lines, "\n"))
	}
}

func TestSellCancelsUnfilledAuctionOrderBeforeMarketSell(t *testing.T) {
	broker := sellDay(246)
	broker.auctionOutcome = "stuck"
	if _, err := newTestOvernightService(broker, OvernightConfig{}).Sell(context.Background()); err != nil {
		t.Fatalf("Sell() error: %v", err)
	}
	if len(broker.canceled) != 1 || broker.canceled[0] != "order-1" {
		t.Fatalf("canceled=%v, want the auction order", broker.canceled)
	}
	if len(broker.placed) != 2 {
		t.Fatalf("placed %d orders, want auction then market", len(broker.placed))
	}
	fallback := broker.placed[1]
	if fallback.Duration != domain.OrderDurationDay || fallback.Shares != 246 || fallback.at.Before(et("2026-10-14 09:31:00")) {
		t.Fatalf("fallback %+v at %s, want day sell of 246 after 09:31", fallback.Order, fallback.at)
	}
	if broker.position != 0 {
		t.Fatalf("position=%v, want 0", broker.position)
	}
}

func TestSellPastAuctionCutoffUsesMarketOrderAfterOpen(t *testing.T) {
	broker := sellDay(246)
	broker.clock.t = et("2026-10-14 09:28:30")
	if _, err := newTestOvernightService(broker, OvernightConfig{}).Sell(context.Background()); err != nil {
		t.Fatalf("Sell() error: %v", err)
	}
	if len(broker.placed) != 1 || broker.placed[0].Duration != domain.OrderDurationDay {
		t.Fatalf("placed %+v, want one day order", broker.placed)
	}
	if broker.placed[0].at.Before(et("2026-10-14 09:30:00")) {
		t.Fatalf("market sell sent at %s, before the open", broker.placed[0].at)
	}
}

func TestSellSendsFractionalRemainderAfterAuction(t *testing.T) {
	// The position a dollar-amount buy leaves.
	broker := sellDay(10.5)
	if _, err := newTestOvernightService(broker, OvernightConfig{}).Sell(context.Background()); err != nil {
		t.Fatalf("Sell() error: %v", err)
	}
	if len(broker.placed) != 2 || broker.placed[0].Shares != 10 || broker.placed[1].Shares != 0.5 {
		t.Fatalf("placed %+v, want 10 in the auction then 0.5", broker.placed)
	}
}

func TestSellRefusesRunStartedLongBeforeOpen(t *testing.T) {
	broker := sellDay(246)
	broker.clock.t = et("2026-10-14 08:00:00")
	_, err := newTestOvernightService(broker, OvernightConfig{}).Sell(context.Background())
	if err == nil || !strings.Contains(err.Error(), "before the") {
		t.Fatalf("Sell() error=%v, want early-run refusal", err)
	}
	if len(broker.placed) != 0 {
		t.Fatalf("placed %d orders", len(broker.placed))
	}
}

func TestSellSkipsClosedDayAndEmptyPosition(t *testing.T) {
	holiday := sellDay(246)
	holiday.sessionOK = false
	empty := sellDay(0)
	for name, broker := range map[string]*fakeOvernightBroker{"holiday": holiday, "empty": empty} {
		if _, err := newTestOvernightService(broker, OvernightConfig{}).Sell(context.Background()); err != nil {
			t.Fatalf("%s: Sell() error: %v", name, err)
		}
		if len(broker.placed) != 0 {
			t.Fatalf("%s: placed %d orders", name, len(broker.placed))
		}
	}
}

func TestSellRunsWhileBuysAreDisabled(t *testing.T) {
	broker := sellDay(246)
	if _, err := newTestOvernightService(broker, OvernightConfig{BuyEnabled: false}).Sell(context.Background()); err != nil {
		t.Fatalf("Sell() error: %v", err)
	}
	if broker.position != 0 {
		t.Fatalf("position=%v, want 0", broker.position)
	}
}

func TestOvernightReportPrintsFailure(t *testing.T) {
	report := &OvernightReport{Action: "buy", Symbol: "SOXL"}
	report.add("Cash $10.00.")
	var out bytes.Buffer
	report.Print(&out, errors.New("boom"))
	for _, want := range []string{"OVERNIGHT BUY  SOXL", "Cash $10.00.", "FAILED: boom"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("output missing %q:\n%s", want, out.String())
		}
	}
}
