package domain

import "time"

// PortfolioConfig defines the rules for one rebalancing cycle.
//
//   - IndexName: identifies which ranking list to pull (e.g. "sp500-momentum").
//   - BrokerName: identifies the execution broker in audit summaries.
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
	ProfileName           string
	IndexName             string
	BrokerName            string
	MaxStocks             int
	SlackValue            int
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

// OrderType enumerates supported order execution types.
type OrderType string

const (
	OrderTypeMarket OrderType = "market"
)

// OrderDuration is the time-in-force of a broker order. Strategy orders may
// leave this empty and let the selected broker apply its configured default.
type OrderDuration string

const (
	OrderDurationDay OrderDuration = "day"
	OrderDurationGTC OrderDuration = "gtc"
)

// Order represents a single trade instruction produced by the rebalancer.
//
//   - For sells: Shares is the full position quantity; Notional is zero.
//   - For buys: Notional is the dollar amount to spend; the broker fetches a
//     fresh quote at execution time and derives quantity = floor(Notional/livePrice).
//   - Type is the order execution type (e.g. OrderTypeMarket).
//   - Reason is a human-readable label for logging ("liquidate", "acquire", etc.).
type Order struct {
	Ticker         string
	Side           OrderSide
	Type           OrderType
	Duration       OrderDuration
	Shares         float64 // non-zero for sells; resolved by review for buys
	Notional       float64 // non-zero for buy strategy instructions
	ReferencePrice float64 // planning price; brokers fetch a fresh quote before review
	Reason         string
	CycleID        string
	ClientOrderID  string
}

// RebalanceResult is the pure output of the domain Rebalance function.
// The service layer executes Sells first (to free cash), then Buys.
type RebalanceResult struct {
	Sells   []Order
	Buys    []Order
	Retains []string // tickers kept without trading (for audit logging)
}

// BrokerOrderStatus is the broker-neutral lifecycle used by reconciliation.
type BrokerOrderStatus string

const (
	BrokerOrderPending         BrokerOrderStatus = "pending"
	BrokerOrderOpen            BrokerOrderStatus = "open"
	BrokerOrderQueued          BrokerOrderStatus = "queued"
	BrokerOrderPartiallyFilled BrokerOrderStatus = "partially_filled"
	BrokerOrderFilled          BrokerOrderStatus = "filled"
	BrokerOrderCanceled        BrokerOrderStatus = "canceled"
	BrokerOrderRejected        BrokerOrderStatus = "rejected"
	BrokerOrderExpired         BrokerOrderStatus = "expired"
	BrokerOrderUnknown         BrokerOrderStatus = "unknown"
)

// BrokerOrder is the normalized durable execution record returned by a broker.
// FilledShares and AverageFillPrice are authoritative for cycle accounting.
type BrokerOrder struct {
	ID                string
	ClientOrderID     string
	AccountID         string
	Ticker            string
	Side              OrderSide
	Type              OrderType
	Duration          OrderDuration
	RequestedShares   float64
	FilledShares      float64
	RemainingShares   float64
	AverageFillPrice  float64
	EstimatedPrice    float64
	EstimatedNotional float64
	Status            BrokerOrderStatus
	CreatedAt         time.Time
	UpdatedAt         time.Time
	CycleID           string
}

// Active reports whether an order still has a broker-managed remainder.
func (o BrokerOrder) Active() bool {
	switch o.Status {
	case BrokerOrderPending, BrokerOrderOpen, BrokerOrderQueued, BrokerOrderPartiallyFilled:
		return true
	default:
		return false
	}
}

// TerminalFailure reports whether an unfilled remainder may be retried.
func (o BrokerOrder) TerminalFailure() bool {
	switch o.Status {
	case BrokerOrderCanceled, BrokerOrderRejected, BrokerOrderExpired:
		return true
	default:
		return false
	}
}

// FilledValue returns actual execution value and never a quote estimate.
func (o BrokerOrder) FilledValue() float64 {
	return o.FilledShares * o.AverageFillPrice
}

// CommittedValue returns actual fills plus the best available estimate for an
// active remainder. It is used only for reserving cycle budget.
func (o BrokerOrder) CommittedValue() float64 {
	value := 0.0
	if o.AverageFillPrice > 0 {
		value = o.FilledValue()
	}
	if !o.Active() {
		return value
	}
	price := o.EstimatedPrice
	if price <= 0 {
		price = o.AverageFillPrice
	}
	value += o.RemainingShares * price
	if o.EstimatedNotional > value {
		return o.EstimatedNotional
	}
	return value
}

// AccountSnapshot contains broker-reported funds available for trading.
type AccountSnapshot struct {
	AccountID     string
	Cash          float64
	BuyingPower   float64
	Agentic       bool
	LimitedMargin bool
}

// OrderQuery selects broker orders relevant to one reconciliation cycle.
type OrderQuery struct {
	AccountID string
	CycleID   string
	From      time.Time
	To        time.Time
}

// OrderReview is an exact pre-trade instruction approved by the broker.
// BrokerData is opaque adapter state and must never be logged.
type OrderReview struct {
	Order             Order
	ReviewID          string
	EstimatedPrice    float64
	EstimatedNotional float64
	Warnings          []string
	Approved          bool
	BrokerData        any
}
