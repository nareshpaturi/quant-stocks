package domain

// PortfolioConfig defines the rules for one rebalancing cycle.
//
//   - IndexName: identifies which ranking list to pull (e.g. "sp500-momentum").
//   - MaxStocks: N, the target number of holdings.
//   - SlackValue: S, how many extra ranks a held stock tolerates before forced sale.
//     A held stock at rank R is kept if R <= N+S; sold if R > N+S.
//   - InitialAmountPerStock: dollar amount allocated per organic buy slot.
//     An "organic" slot is one that was already empty before this rebalance cycle
//     (i.e. not freed by a sell this run). This applies when the portfolio is
//     empty or has fewer than MaxStocks positions.
//     Must be > 0; config.Validate enforces this so the service never calls
//     Rebalance with a zero value when organic slots are expected.
type PortfolioConfig struct {
	IndexName            string
	MaxStocks            int
	SlackValue           int
	InitialAmountPerStock float64
}

// Position represents a currently held stock in the brokerage account.
type Position struct {
	Ticker       string
	Shares       float64
	CurrentPrice float64 // latest market price, populated at rebalance time
}

// MarketValue returns the total notional value of this position.
func (p Position) MarketValue() float64 {
	return p.Shares * p.CurrentPrice
}

// Rank represents a stock's position in the momentum ranking index.
// Position is 1-indexed (rank 1 = highest momentum / best rank).
// Price is the latest market price used for share-count calculations.
type Rank struct {
	Ticker   string
	Position int     // 1 = best rank
	Price    float64 // latest market price
}

// OrderSide enumerates buy and sell directions.
type OrderSide string

const (
	OrderSideBuy  OrderSide = "buy"
	OrderSideSell OrderSide = "sell"
)

// Order represents a single trade instruction produced by the rebalancer.
//
//   - For sells: Shares is the full position quantity.
//   - For buys: Shares is computed from projected cash divided by market price.
//   - Reason is a human-readable label for logging ("liquidate", "acquire", etc.).
type Order struct {
	Ticker string
	Side   OrderSide
	Shares float64
	Reason string
}

// RebalanceResult is the pure output of the domain Rebalance function.
// The service layer executes Sells first (to free cash), then Buys.
type RebalanceResult struct {
	Sells   []Order
	Buys    []Order
	Retains []string // tickers kept without trading (for audit logging)
}

// OpenOrder represents an already-submitted order that is pending or active at
// the broker. Used by the service layer for idempotency: an order whose
// (Ticker, Side) pair already exists is not re-submitted.
type OpenOrder struct {
	Ticker string
	Side   OrderSide
	Shares float64
	Status string // "open", "partially_filled", "pending"
}

// PendingKey returns a canonical deduplication key for this open order.
func (o OpenOrder) PendingKey() string {
	return o.Ticker + ":" + string(o.Side)
}
