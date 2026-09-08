package broker

import (
	"context"
	"fmt"
	"math"

	"github.com/nareshpaturi/quant-stocks/internal/domain"
)

// MockBroker is an in-memory Broker implementation for local testing and
// GitHub Actions dry-runs. It holds hardcoded positions and cash and records
// every reviewed order submitted through PlaceOrder without actually trading.
type MockBroker struct {
	Positions      []domain.Position
	Cash           float64
	Orders         []domain.BrokerOrder
	ExecutedOrders []domain.Order
}

// NewMockBroker constructs a MockBroker with the given initial state.
func NewMockBroker(positions []domain.Position, cash float64) *MockBroker {
	return &MockBroker{
		Positions: positions,
		Cash:      cash,
	}
}

func (m *MockBroker) GetPositions(_ context.Context) ([]domain.Position, error) {
	return m.Positions, nil
}

func (m *MockBroker) GetAccount(_ context.Context) (domain.AccountSnapshot, error) {
	return domain.AccountSnapshot{AccountID: "mock", Cash: m.Cash, BuyingPower: m.Cash}, nil
}

// GetQuotes returns the last-trade price for each requested ticker from the
// mock positions. Tickers not in Positions return 0 (price preserved from
// the ranking provider for mock runs).
func (m *MockBroker) GetQuotes(_ context.Context, tickers []string) (map[string]float64, error) {
	posMap := make(map[string]float64, len(m.Positions))
	for _, p := range m.Positions {
		posMap[p.Ticker] = p.CurrentPrice
	}
	result := make(map[string]float64, len(tickers))
	for _, t := range tickers {
		result[t] = posMap[t]
	}
	return result, nil
}

// GetOrders returns the pre-seeded order history (empty by default).
func (m *MockBroker) GetOrders(_ context.Context, _ domain.OrderQuery) ([]domain.BrokerOrder, error) {
	return m.Orders, nil
}

func (m *MockBroker) ReviewOrder(_ context.Context, order domain.Order) (domain.OrderReview, error) {
	if order.Type == "" {
		order.Type = domain.OrderTypeMarket
	}
	if order.Duration == "" {
		order.Duration = domain.OrderDurationDay
	}
	price := order.ReferencePrice
	if order.Side == domain.OrderSideBuy {
		if price <= 0 {
			return domain.OrderReview{}, fmt.Errorf("mock review: no price for %s", order.Ticker)
		}
		order.Shares = math.Floor(order.Notional / price)
		if order.Shares < 1 {
			return domain.OrderReview{}, fmt.Errorf("mock review: notional buys no whole shares of %s", order.Ticker)
		}
	}
	return domain.OrderReview{
		Order: order, EstimatedPrice: price,
		EstimatedNotional: order.Shares * price, Approved: true,
	}, nil
}

// PlaceOrder records the reviewed order and returns a synthetic pending order.
func (m *MockBroker) PlaceOrder(_ context.Context, review domain.OrderReview) (domain.BrokerOrder, error) {
	if !review.Approved {
		return domain.BrokerOrder{}, fmt.Errorf("mock place: order was not approved")
	}
	order := review.Order
	m.ExecutedOrders = append(m.ExecutedOrders, order)
	id := fmt.Sprintf("MOCK-%s-%s", string(order.Side), order.Ticker)
	placed := domain.BrokerOrder{
		ID: id, ClientOrderID: order.ClientOrderID, AccountID: "mock",
		Ticker: order.Ticker, Side: order.Side, Type: order.Type, Duration: order.Duration,
		RequestedShares: order.Shares, RemainingShares: order.Shares,
		EstimatedPrice: review.EstimatedPrice, EstimatedNotional: review.EstimatedNotional,
		Status: domain.BrokerOrderPending, CycleID: order.CycleID,
	}
	m.Orders = append(m.Orders, placed)
	return placed, nil
}

// IsMarketOpen always returns true for the mock — the mock is usable at any time.
func (m *MockBroker) IsMarketOpen(_ context.Context) (bool, error) {
	return true, nil
}
