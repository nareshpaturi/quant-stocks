package port

import (
	"context"

	"github.com/nareshpaturi/quant-stocks/internal/domain"
)

// RankingProvider is the outbound port for obtaining stock momentum rankings.
//
// Implementations may call a REST API, read a CSV file, query a database,
// or use any other data source. The returned slice must include the current
// market price on each Rank so the domain layer can size buy orders without
// requiring a separate price lookup.
//
// The returned slice should be sorted by Position ascending (rank 1 = best),
// although the domain layer will re-sort defensively.
type RankingProvider interface {
	// GetRankings returns the ordered momentum ranking list for the given index.
	// indexName identifies which index to retrieve (e.g. "sp500-momentum", "ndx100").
	GetRankings(ctx context.Context, indexName string) ([]domain.Rank, error)
}
