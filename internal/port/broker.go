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

	// GetCash returns the total uninvested cash available in the account.
	GetCash(ctx context.Context) (float64, error)

	// GetOpenOrders returns all orders that are currently pending or active at
	// the broker (status: "open", "partially_filled", "pending").
	// Used by the service layer to enforce idempotency before placing new orders.
	GetOpenOrders(ctx context.Context) ([]domain.OpenOrder, error)

	// ExecuteOrder submits a market order to the broker.
	// Returns the broker-assigned order ID on success.
	// For sells, order.Shares is the full position quantity.
	// For buys, order.Shares is the floor-truncated share count computed by the domain.
	ExecuteOrder(ctx context.Context, order domain.Order) (string, error)
}
