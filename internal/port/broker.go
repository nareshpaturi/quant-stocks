package port

import (
	"context"

	"github.com/nareshpaturi/quant-stocks/internal/domain"
)

// Broker is the outbound port for all brokerage operations.
//
// Any concrete broker implementation (Tradier, Schwab, Interactive Brokers, mock)
// must satisfy this interface. The service layer depends only on this interface,
// never on a concrete adapter, keeping the domain fully decoupled from
// infrastructure concerns.
type Broker interface {
	// GetPositions returns all currently held positions.
	// Implementations must populate CurrentPrice on each Position so that
	// the domain layer can compute market values without extra API calls.
	GetPositions(ctx context.Context) ([]domain.Position, error)

	// GetAccount returns broker-reported cash and buying power. BuyingPower is
	// the final affordability boundary for live orders.
	GetAccount(ctx context.Context) (domain.AccountSnapshot, error)

	// GetOrders returns active and historical orders in the requested cycle
	// window so the service can reconcile fills and retries across processes.
	GetOrders(ctx context.Context, query domain.OrderQuery) ([]domain.BrokerOrder, error)

	// GetQuotes returns the latest last-trade price for each requested ticker.
	// Used by the service layer to price buy candidates whose price is not
	// included in the rankings feed.
	GetQuotes(ctx context.Context, tickers []string) (map[string]float64, error)

	// ReviewOrder resolves an exact quantity and performs the broker's pre-trade
	// validation. No order may be placed without a successful review.
	ReviewOrder(ctx context.Context, order domain.Order) (domain.OrderReview, error)

	// PlaceOrder submits exactly the instruction returned by ReviewOrder.
	PlaceOrder(ctx context.Context, review domain.OrderReview) (domain.BrokerOrder, error)

	// IsMarketOpen returns true when the US equity market is currently open for
	// regular trading. Used by the backtest runner to prevent paper orders from
	// being placed outside market hours.
	IsMarketOpen(ctx context.Context) (bool, error)
}
