package domain_test

import (
	"testing"

	"github.com/nareshpaturi/quant-stocks/internal/domain"
)

func reconciliationFixture() (domain.PortfolioConfig, domain.CycleSnapshot) {
	cfg := domain.PortfolioConfig{ProfileName: "test", IndexName: "sp500", MaxStocks: 1, InitialAmountPerStock: 5000}
	cycleID := domain.NewCycleID("test", "agentic-1", "sp500", "2026-09-06")
	return cfg, domain.CycleSnapshot{
		CycleID:   cycleID,
		Account:   domain.AccountSnapshot{AccountID: "agentic-1", BuyingPower: 10000},
		Positions: []domain.Position{{Ticker: "TSLA", Shares: 10, CurrentPrice: 250}},
		Rankings: []domain.Rank{
			{Ticker: "AAPL", Position: 1, Price: 200},
			{Ticker: "TSLA", Position: 9, Price: 250},
		},
	}
}

func TestReconcileInitialCycleSubmitsSellButWaitsForReplacementFunding(t *testing.T) {
	cfg, snapshot := reconciliationFixture()
	got := domain.Reconcile(cfg, snapshot)
	if len(got.Sells) != 1 || got.Sells[0].Ticker != "TSLA" {
		t.Fatalf("expected TSLA sell, got %+v", got.Sells)
	}
	if len(got.Buys) != 0 {
		t.Fatalf("expected replacement buy to wait for realized proceeds, got %+v", got.Buys)
	}
	if got.Sells[0].ClientOrderID == "" || got.Sells[0].CycleID != snapshot.CycleID {
		t.Fatalf("sell missing cycle idempotency fields: %+v", got.Sells[0])
	}
}

func TestReconcileFilledSellRetriesFailedBuyWithActualProceeds(t *testing.T) {
	cfg, snapshot := reconciliationFixture()
	snapshot.Positions = nil
	snapshot.Orders = []domain.BrokerOrder{
		{ID: "sell-1", AccountID: "agentic-1", Ticker: "TSLA", Side: domain.OrderSideSell,
			RequestedShares: 10, FilledShares: 10, AverageFillPrice: 247, Status: domain.BrokerOrderFilled},
		{ID: "buy-1", AccountID: "agentic-1", Ticker: "AAPL", Side: domain.OrderSideBuy,
			RequestedShares: 12, Status: domain.BrokerOrderRejected},
	}

	got := domain.Reconcile(cfg, snapshot)
	if len(got.Sells) != 0 {
		t.Fatalf("filled sell must not be repeated: %+v", got.Sells)
	}
	if len(got.Buys) != 1 {
		t.Fatalf("expected one retry buy, got %+v", got.Buys)
	}
	if got.Buys[0].Notional != 2470 {
		t.Fatalf("buy notional=%v, want actual proceeds 2470", got.Buys[0].Notional)
	}
	want := domain.NewClientOrderID(snapshot.CycleID, "AAPL", domain.OrderSideBuy, 0, 1)
	if got.Buys[0].ClientOrderID != want {
		t.Fatalf("retry client id=%q, want %q", got.Buys[0].ClientOrderID, want)
	}
}

func TestReconcileActiveAndPartialOrdersAreNotDuplicated(t *testing.T) {
	cfg, snapshot := reconciliationFixture()
	snapshot.Orders = []domain.BrokerOrder{
		{ID: "sell-1", AccountID: "agentic-1", Ticker: "TSLA", Side: domain.OrderSideSell,
			RequestedShares: 10, FilledShares: 4, RemainingShares: 6, AverageFillPrice: 250,
			Status: domain.BrokerOrderPartiallyFilled},
	}
	snapshot.Positions[0].Shares = 6

	got := domain.Reconcile(cfg, snapshot)
	if len(got.Sells) != 0 {
		t.Fatalf("active partial sell was duplicated: %+v", got.Sells)
	}
	if len(got.Buys) != 1 || got.Buys[0].Notional != 1000 {
		t.Fatalf("expected buy limited to realized partial proceeds, got %+v", got.Buys)
	}
}

func TestReconcileUnknownStatusFailsClosedForSymbol(t *testing.T) {
	cfg, snapshot := reconciliationFixture()
	snapshot.Orders = []domain.BrokerOrder{{
		ID: "mystery", AccountID: "agentic-1", Ticker: "TSLA", Side: domain.OrderSideSell,
		Status: domain.BrokerOrderUnknown,
	}}
	got := domain.Reconcile(cfg, snapshot)
	if len(got.Sells) != 0 || len(got.Blocked) == 0 {
		t.Fatalf("unknown order must block submission: %+v", got)
	}
}

func TestReconcileDoesNotSpendUnvaluedSellFill(t *testing.T) {
	cfg, snapshot := reconciliationFixture()
	snapshot.Positions = nil
	snapshot.Orders = []domain.BrokerOrder{{
		ID: "sell-1", AccountID: "agentic-1", Ticker: "TSLA", Side: domain.OrderSideSell,
		RequestedShares: 10, FilledShares: 10, Status: domain.BrokerOrderFilled,
	}}
	got := domain.Reconcile(cfg, snapshot)
	if len(got.Buys) != 0 || len(got.Blocked) == 0 {
		t.Fatalf("unvalued fill must block replacement: %+v", got)
	}
}

func TestReconcileDoesNotRetryPartiallyFilledBuyWithoutFillPrice(t *testing.T) {
	cfg, snapshot := reconciliationFixture()
	snapshot.Positions = []domain.Position{{Ticker: "AAPL", Shares: 2, CurrentPrice: 200}}
	snapshot.Orders = []domain.BrokerOrder{
		{ID: "sell-1", AccountID: "agentic-1", Ticker: "TSLA", Side: domain.OrderSideSell,
			RequestedShares: 10, FilledShares: 10, AverageFillPrice: 247, Status: domain.BrokerOrderFilled},
		{ID: "buy-1", AccountID: "agentic-1", Ticker: "AAPL", Side: domain.OrderSideBuy,
			RequestedShares: 12, FilledShares: 2, Status: domain.BrokerOrderCanceled},
	}
	got := domain.Reconcile(cfg, snapshot)
	if len(got.Buys) != 0 || len(got.Blocked) == 0 {
		t.Fatalf("unvalued partial buy must block retry: %+v", got)
	}
}

func TestReconcileOrganicBuyUsesBuyingPower(t *testing.T) {
	cfg := domain.PortfolioConfig{ProfileName: "test", IndexName: "sp500", MaxStocks: 1, InitialAmountPerStock: 5000}
	snapshot := domain.CycleSnapshot{
		CycleID: "cycle", Account: domain.AccountSnapshot{BuyingPower: 1200},
		Rankings: []domain.Rank{{Ticker: "AAPL", Position: 1, Price: 200}},
	}
	got := domain.Reconcile(cfg, snapshot)
	if len(got.Buys) != 1 || got.Buys[0].Notional != 1200 {
		t.Fatalf("expected buy capped at 1200 buying power, got %+v", got.Buys)
	}
}

func TestNewClientOrderIDIsStableAndAttemptSpecific(t *testing.T) {
	a := domain.NewClientOrderID("cycle", "AAPL", domain.OrderSideBuy, 0, 0)
	b := domain.NewClientOrderID("cycle", "AAPL", domain.OrderSideBuy, 0, 0)
	c := domain.NewClientOrderID("cycle", "AAPL", domain.OrderSideBuy, 0, 1)
	if a != b || a == c || len(a) != 36 {
		t.Fatalf("unexpected ids: %q %q %q", a, b, c)
	}
}
