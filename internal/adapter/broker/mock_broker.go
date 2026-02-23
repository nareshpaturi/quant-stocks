package broker

import (
	"context"
	"fmt"

	"github.com/nareshpaturi/quant-stocks/internal/domain"
)

// MockBroker is an in-memory Broker implementation for local testing and
// GitHub Actions dry-runs. It holds hardcoded positions and cash and records
// every order submitted through ExecuteOrder without actually trading.
type MockBroker struct {
	Positions      []domain.Position
	Cash           float64
	OpenOrders     []domain.OpenOrder // pre-seeded open orders for idempotency testing
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

func (m *MockBroker) GetCash(_ context.Context) (float64, error) {
	return m.Cash, nil
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

// GetOpenOrders returns the pre-seeded open orders (empty by default).
func (m *MockBroker) GetOpenOrders(_ context.Context) ([]domain.OpenOrder, error) {
	return m.OpenOrders, nil
}

// ExecuteOrder records the order and returns a synthetic order ID.
// No real trades are placed.
func (m *MockBroker) ExecuteOrder(_ context.Context, order domain.Order) (string, error) {
	m.ExecutedOrders = append(m.ExecutedOrders, order)
	return fmt.Sprintf("MOCK-%s-%s", string(order.Side), order.Ticker), nil
}

// IsMarketOpen always returns true for the mock — the mock is usable at any time.
func (m *MockBroker) IsMarketOpen(_ context.Context) (bool, error) {
	return true, nil
}
