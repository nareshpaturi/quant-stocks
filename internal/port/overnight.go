package port

import (
	"context"

	"github.com/nareshpaturi/quant-stocks/internal/domain"
)

// OvernightBroker is the outbound port for the overnight-hold strategy.
//
// It is narrower than Broker: the strategy trades one symbol with plain market
// orders and needs the market clock and calendar to know when to act.
type OvernightBroker interface {
	// GetClock returns whether the regular session is open now and when it
	// next opens and closes.
	GetClock(ctx context.Context) (domain.MarketClock, error)

	// GetSession returns the regular session for date (YYYY-MM-DD, New York).
	// The bool is false when the market is closed that day.
	GetSession(ctx context.Context, date string) (domain.MarketSession, bool, error)

	// GetAccount returns broker-reported cash and buying power.
	GetAccount(ctx context.Context) (domain.AccountSnapshot, error)

	// GetPositionShares returns the shares held of symbol, or zero when none.
	GetPositionShares(ctx context.Context, symbol string) (float64, error)

	// GetQuote returns the latest quote and trade price for symbol.
	GetQuote(ctx context.Context, symbol string) (domain.Quote, error)

	// IsFractionable reports whether symbol can be bought in fractional
	// shares, and therefore with a dollar-amount (Notional) order.
	IsFractionable(ctx context.Context, symbol string) (bool, error)

	// PlaceOrder submits a market order for either Shares or a Notional dollar
	// amount, with the given time-in-force and ClientOrderID.
	PlaceOrder(ctx context.Context, order domain.Order) (domain.BrokerOrder, error)

	// GetOrder returns the current state of a previously placed order.
	GetOrder(ctx context.Context, id string) (domain.BrokerOrder, error)

	// GetOrderByClientID finds an order by the ClientOrderID it was placed
	// with. The bool is false when no such order exists.
	GetOrderByClientID(ctx context.Context, clientOrderID string) (domain.BrokerOrder, bool, error)

	// CancelOrder requests cancellation of an open order.
	CancelOrder(ctx context.Context, id string) error
}
