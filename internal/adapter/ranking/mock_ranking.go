package ranking

import (
	"context"

	"github.com/nareshpaturi/quant-stocks/internal/domain"
)

// MockRankingProvider returns a hardcoded ranked list.
// Useful for deterministic local runs, integration tests, and CI dry-runs.
// The IndexName argument is intentionally ignored so the same instance can
// serve any index in test scenarios.
type MockRankingProvider struct {
	Rankings []domain.Rank
}

// NewMockRankingProvider constructs a MockRankingProvider with the given rankings.
func NewMockRankingProvider(rankings []domain.Rank) *MockRankingProvider {
	return &MockRankingProvider{Rankings: rankings}
}

func (m *MockRankingProvider) GetRankings(_ context.Context, _ string) ([]domain.Rank, error) {
	return m.Rankings, nil
}
