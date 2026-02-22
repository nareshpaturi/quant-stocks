package domain_test

import (
	"testing"

	"github.com/nareshpaturi/quant-stocks/internal/domain"
)

// tickerSet converts a slice of Orders into a set of tickers for easy comparison.
func tickerSet(orders []domain.Order) map[string]bool {
	m := make(map[string]bool, len(orders))
	for _, o := range orders {
		m[o.Ticker] = true
	}
	return m
}

// stringSet converts a string slice into a set.
func stringSet(ss []string) map[string]bool {
	m := make(map[string]bool, len(ss))
	for _, s := range ss {
		m[s] = true
	}
	return m
}

// assertOrderTickers fails the test if the actual order tickers don't match wantTickers exactly.
func assertOrderTickers(t *testing.T, label string, orders []domain.Order, wantTickers []string) {
	t.Helper()
	got := tickerSet(orders)
	want := stringSet(wantTickers)

	for w := range want {
		if !got[w] {
			t.Errorf("[%s] expected ticker %q not found; got %v", label, w, got)
		}
	}
	for g := range got {
		if !want[g] {
			t.Errorf("[%s] unexpected ticker %q; want %v", label, g, want)
		}
	}
	if len(got) != len(want) {
		t.Errorf("[%s] length mismatch: got %d orders, want %d", label, len(got), len(want))
	}
}

func assertStringSliceSet(t *testing.T, label string, got, want []string) {
	t.Helper()
	gotSet := stringSet(got)
	wantSet := stringSet(want)
	for w := range wantSet {
		if !gotSet[w] {
			t.Errorf("[%s] expected %q not found; got %v", label, w, gotSet)
		}
	}
	for g := range gotSet {
		if !wantSet[g] {
			t.Errorf("[%s] unexpected %q; want %v", label, g, wantSet)
		}
	}
}

func TestRebalance(t *testing.T) {
	tests := []struct {
		name        string
		cfg         domain.PortfolioConfig
		positions   []domain.Position
		rankings    []domain.Rank
		cash        float64
		wantSells   []string
		wantBuys    []string
		wantRetains []string
	}{
		{
			name: "top-N stock is retained with no sell",
			// AAPL rank 1, held, N=5 → keep, no trade.
			// 4 organic slots filled via InitialAmountPerStock; uniform $100 price.
			cfg: domain.PortfolioConfig{MaxStocks: 5, SlackValue: 2, InitialAmountPerStock: 5000},
			positions: []domain.Position{
				{Ticker: "AAPL", Shares: 10, CurrentPrice: 100},
			},
			rankings: []domain.Rank{
				{Ticker: "AAPL", Position: 1, Price: 100},
				{Ticker: "NVDA", Position: 2, Price: 100},
				{Ticker: "MSFT", Position: 3, Price: 100},
				{Ticker: "AMZN", Position: 4, Price: 100},
				{Ticker: "GOOG", Position: 5, Price: 100},
			},
			cash:        0,
			wantSells:   nil,
			wantBuys:    []string{"NVDA", "MSFT", "AMZN", "GOOG"}, // 4 organic slots
			wantRetains: []string{"AAPL"},
		},
		{
			name: "stock in slack zone is retained without sell",
			// TSLA rank 6, N=5, S=3 → threshold=8. 6<=8 → retain.
			// 2 held (AAPL, TSLA) → 3 organic slots → buy top-3 non-held (rank 2, 3, 4).
			cfg: domain.PortfolioConfig{MaxStocks: 5, SlackValue: 3, InitialAmountPerStock: 5000},
			positions: []domain.Position{
				{Ticker: "TSLA", Shares: 5, CurrentPrice: 100},
				{Ticker: "AAPL", Shares: 10, CurrentPrice: 100},
			},
			rankings: []domain.Rank{
				{Ticker: "AAPL", Position: 1, Price: 100},
				{Ticker: "NVDA", Position: 2, Price: 100},
				{Ticker: "MSFT", Position: 3, Price: 100},
				{Ticker: "AMZN", Position: 4, Price: 100},
				{Ticker: "GOOG", Position: 5, Price: 100},
				{Ticker: "TSLA", Position: 6, Price: 100},
			},
			cash:        0,
			wantSells:   nil,
			wantBuys:    []string{"NVDA", "MSFT", "AMZN"}, // 3 organic slots (MaxStocks=5 − 2 retained)
			wantRetains: []string{"TSLA", "AAPL"},
		},
		{
			name: "stock beyond slack is liquidated",
			// TSLA rank 9, N=5, S=3 → threshold=8. 9>8 → sell.
			// 1 sell (TSLA 5*100=$500) → 1 sell-funded slot (AAPL gets $500).
			// 4 organic slots (MSFT,NVDA,AMZN,GOOG) → each gets InitialAmountPerStock.
			cfg: domain.PortfolioConfig{MaxStocks: 5, SlackValue: 3, InitialAmountPerStock: 5000},
			positions: []domain.Position{
				{Ticker: "TSLA", Shares: 5, CurrentPrice: 100},
			},
			rankings: []domain.Rank{
				{Ticker: "AAPL", Position: 1, Price: 100},
				{Ticker: "MSFT", Position: 2, Price: 100},
				{Ticker: "NVDA", Position: 3, Price: 100},
				{Ticker: "AMZN", Position: 4, Price: 100},
				{Ticker: "GOOG", Position: 5, Price: 100},
				{Ticker: "TSLA", Position: 9, Price: 100},
			},
			cash:      0,
			wantSells: []string{"TSLA"},
			wantBuys:  []string{"AAPL", "MSFT", "NVDA", "AMZN", "GOOG"},
		},
		{
			name: "stock not in index at all is liquidated",
			// 1 sell (OBSOLETE 10*100=$1000) → 1 sell-funded slot (AAPL gets $1000).
			// 4 organic slots (MSFT,NVDA,AMZN,GOOG) → each gets InitialAmountPerStock.
			cfg: domain.PortfolioConfig{MaxStocks: 5, SlackValue: 2, InitialAmountPerStock: 5000},
			positions: []domain.Position{
				{Ticker: "OBSOLETE", Shares: 10, CurrentPrice: 100},
			},
			rankings: []domain.Rank{
				{Ticker: "AAPL", Position: 1, Price: 100},
				{Ticker: "MSFT", Position: 2, Price: 100},
				{Ticker: "NVDA", Position: 3, Price: 100},
				{Ticker: "AMZN", Position: 4, Price: 100},
				{Ticker: "GOOG", Position: 5, Price: 100},
			},
			cash:      0,
			wantSells: []string{"OBSOLETE"},
			wantBuys:  []string{"AAPL", "MSFT", "NVDA", "AMZN", "GOOG"},
		},
		{
			name: "empty portfolio buys top-N stocks from InitialAmountPerStock",
			// All 3 slots are organic → each gets InitialAmountPerStock=$2000.
			// AAPL floor(2000/200)=10, MSFT floor(2000/400)=5, NVDA floor(2000/800)=2.
			cfg: domain.PortfolioConfig{MaxStocks: 3, SlackValue: 1, InitialAmountPerStock: 2000},
			positions: nil,
			rankings: []domain.Rank{
				{Ticker: "AAPL", Position: 1, Price: 200},
				{Ticker: "MSFT", Position: 2, Price: 400},
				{Ticker: "NVDA", Position: 3, Price: 800},
				{Ticker: "GOOG", Position: 4, Price: 150}, // rank 4 > MaxStocks=3, excluded
			},
			cash:      0,
			wantSells: nil,
			wantBuys:  []string{"AAPL", "MSFT", "NVDA"},
		},
		{
			name: "sell proceeds fund replacement buy",
			// Sell 10 TSLA @ $250 = $2500 proceeds → 1 sell-funded slot.
			// AAPL: floor(2500/200) = 12 shares. No organic slots.
			cfg: domain.PortfolioConfig{MaxStocks: 1, SlackValue: 0},
			positions: []domain.Position{
				{Ticker: "TSLA", Shares: 10, CurrentPrice: 250},
			},
			rankings: []domain.Rank{
				{Ticker: "AAPL", Position: 1, Price: 200},
				{Ticker: "TSLA", Position: 5, Price: 250}, // rank 5 > threshold(1+0=1) → sell
			},
			cash:      0,
			wantSells: []string{"TSLA"},
			wantBuys:  []string{"AAPL"},
		},
		{
			name: "full portfolio with no changes needed",
			// All top-N stocks held → no sells, no buys.
			cfg: domain.PortfolioConfig{MaxStocks: 2, SlackValue: 1},
			positions: []domain.Position{
				{Ticker: "AAPL", Shares: 5, CurrentPrice: 200},
				{Ticker: "MSFT", Shares: 3, CurrentPrice: 400},
			},
			rankings: []domain.Rank{
				{Ticker: "AAPL", Position: 1, Price: 200},
				{Ticker: "MSFT", Position: 2, Price: 400},
			},
			cash:        500,
			wantSells:   nil,
			wantBuys:    nil,
			wantRetains: []string{"AAPL", "MSFT"},
		},
		{
			name: "zero InitialAmountPerStock means no organic buys",
			// InitialAmountPerStock=0 → sharesToBuy(0, 200)=0 → all organic slots skipped.
			cfg:  domain.PortfolioConfig{MaxStocks: 5, SlackValue: 1, InitialAmountPerStock: 0},
			positions: nil,
			rankings: []domain.Rank{
				{Ticker: "AAPL", Position: 1, Price: 200},
			},
			cash:      0,
			wantSells: nil,
			wantBuys:  nil,
		},
		{
			name: "buy share count uses floor not round",
			// Organic slot: $1000 / $300 = 3.33 → floor to 3, not 4.
			cfg:       domain.PortfolioConfig{MaxStocks: 1, SlackValue: 0, InitialAmountPerStock: 1000},
			positions: nil,
			rankings: []domain.Rank{
				{Ticker: "AAPL", Position: 1, Price: 300},
			},
			cash:      0,
			wantSells: nil,
			wantBuys:  []string{"AAPL"},
		},
		{
			name: "only top-N ranked stocks are eligible for buy",
			// NVDA at rank N+1 must not be purchased even if slots are open.
			// Both slots are organic → each gets InitialAmountPerStock.
			cfg:       domain.PortfolioConfig{MaxStocks: 2, SlackValue: 0, InitialAmountPerStock: 10000},
			positions: nil,
			rankings: []domain.Rank{
				{Ticker: "AAPL", Position: 1, Price: 200},
				{Ticker: "MSFT", Position: 2, Price: 300},
				{Ticker: "NVDA", Position: 3, Price: 800}, // rank 3 > MaxStocks=2
			},
			cash:      0,
			wantBuys:  []string{"AAPL", "MSFT"},
			wantSells: nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := domain.Rebalance(tc.cfg, tc.positions, tc.rankings, tc.cash)

			assertOrderTickers(t, "sells", result.Sells, tc.wantSells)
			assertOrderTickers(t, "buys", result.Buys, tc.wantBuys)

			if tc.wantRetains != nil {
				assertStringSliceSet(t, "retains", result.Retains, tc.wantRetains)
			}
		})
	}
}

// TestRebalanceShareCalculation verifies the exact share count math for organic buys.
func TestRebalanceShareCalculation(t *testing.T) {
	// Organic slot: InitialAmountPerStock=1000, price=300 → floor(1000/300) = 3.
	cfg := domain.PortfolioConfig{MaxStocks: 1, SlackValue: 0, InitialAmountPerStock: 1000}
	rankings := []domain.Rank{
		{Ticker: "AAPL", Position: 1, Price: 300.0},
	}

	result := domain.Rebalance(cfg, nil, rankings, 0)

	if len(result.Buys) != 1 {
		t.Fatalf("expected 1 buy, got %d", len(result.Buys))
	}
	buy := result.Buys[0]
	if buy.Ticker != "AAPL" {
		t.Errorf("expected AAPL buy, got %s", buy.Ticker)
	}
	// floor(1000/300) = 3
	if buy.Shares != 3.0 {
		t.Errorf("expected 3 shares (floor of 1000/300), got %f", buy.Shares)
	}
}

// TestRebalanceOrderSides verifies sell orders have OrderSideSell and buys have OrderSideBuy.
func TestRebalanceOrderSides(t *testing.T) {
	cfg := domain.PortfolioConfig{MaxStocks: 1, SlackValue: 0}
	positions := []domain.Position{
		{Ticker: "TSLA", Shares: 5, CurrentPrice: 250},
	}
	rankings := []domain.Rank{
		{Ticker: "AAPL", Position: 1, Price: 200},
		{Ticker: "TSLA", Position: 5, Price: 250}, // beyond threshold
	}

	result := domain.Rebalance(cfg, positions, rankings, 0)

	for _, sell := range result.Sells {
		if sell.Side != domain.OrderSideSell {
			t.Errorf("sell order for %s has wrong Side: %s", sell.Ticker, sell.Side)
		}
	}
	for _, buy := range result.Buys {
		if buy.Side != domain.OrderSideBuy {
			t.Errorf("buy order for %s has wrong Side: %s", buy.Ticker, buy.Side)
		}
	}
}

// TestRebalanceSlackBoundaryExact tests the exact boundary of the slack zone.
func TestRebalanceSlackBoundaryExact(t *testing.T) {
	cfg := domain.PortfolioConfig{MaxStocks: 5, SlackValue: 2} // threshold = 7

	// Rank 7 (= threshold exactly) → must RETAIN.
	t.Run("rank equals threshold is retained", func(t *testing.T) {
		positions := []domain.Position{
			{Ticker: "FOO", Shares: 1, CurrentPrice: 100},
		}
		rankings := []domain.Rank{
			{Ticker: "AAPL", Position: 1, Price: 100},
			{Ticker: "MSFT", Position: 2, Price: 100},
			{Ticker: "NVDA", Position: 3, Price: 100},
			{Ticker: "AMZN", Position: 4, Price: 100},
			{Ticker: "GOOG", Position: 5, Price: 100},
			{Ticker: "META", Position: 6, Price: 100},
			{Ticker: "FOO", Position: 7, Price: 100}, // exactly threshold
		}
		result := domain.Rebalance(cfg, positions, rankings, 0)
		assertOrderTickers(t, "sells", result.Sells, nil)
		if len(result.Retains) != 1 || result.Retains[0] != "FOO" {
			t.Errorf("expected FOO in retains, got %v", result.Retains)
		}
	})

	// Rank 8 (= threshold+1) → must SELL.
	t.Run("rank one beyond threshold is sold", func(t *testing.T) {
		positions := []domain.Position{
			{Ticker: "BAR", Shares: 2, CurrentPrice: 100},
		}
		rankings := []domain.Rank{
			{Ticker: "AAPL", Position: 1, Price: 100},
			{Ticker: "MSFT", Position: 2, Price: 100},
			{Ticker: "NVDA", Position: 3, Price: 100},
			{Ticker: "AMZN", Position: 4, Price: 100},
			{Ticker: "GOOG", Position: 5, Price: 100},
			{Ticker: "META", Position: 6, Price: 100},
			{Ticker: "NFLX", Position: 7, Price: 100},
			{Ticker: "BAR", Position: 8, Price: 100}, // one beyond threshold
		}
		result := domain.Rebalance(cfg, positions, rankings, 0)
		assertOrderTickers(t, "sells", result.Sells, []string{"BAR"})
	})
}
