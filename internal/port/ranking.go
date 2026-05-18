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
	// GetRankingsForDate returns the ordered momentum ranking list for the given
	// index, as of the given date. indexName identifies which index to retrieve
	// (e.g. "sp500", "ndx"). date is "YYYY-MM-DD" — typically the most recent
	// Sunday for live runs, or a historical Sunday for backtests.
	GetRankingsForDate(ctx context.Context, indexName, date string) ([]domain.Rank, error)
}
