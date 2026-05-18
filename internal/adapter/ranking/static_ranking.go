package ranking

import (
	"context"
	"encoding/csv"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/nareshpaturi/quant-stocks/internal/domain"
)

// StaticCSVRankingProvider reads momentum rankings from a CSV file on disk.
//
// CSV format (no header row, three columns per line):
//
//	ticker,rank,price
//
// Example:
//
//	AAPL,1,195.50
//	MSFT,2,415.20
//	NVDA,3,875.00
//
// The IndexName and date arguments are ignored; this provider always reads
// the same configured file. Commit or generate the file as part of your CI
// pipeline before the rebalancer job runs.
type StaticCSVRankingProvider struct {
	FilePath string
}

// NewStaticCSVRankingProvider constructs the provider with the given file path.
func NewStaticCSVRankingProvider(path string) *StaticCSVRankingProvider {
	return &StaticCSVRankingProvider{FilePath: path}
}

// GetRankingsForDate reads and parses the CSV file, returning a Rank slice.
// Rows with a leading '#' are treated as comments and skipped.
func (s *StaticCSVRankingProvider) GetRankingsForDate(_ context.Context, _, _ string) ([]domain.Rank, error) {
	f, err := os.Open(s.FilePath)
	if err != nil {
		return nil, fmt.Errorf("open rankings CSV %q: %w", s.FilePath, err)
	}
	defer f.Close()

	r := csv.NewReader(f)
	r.Comment = '#'
	r.TrimLeadingSpace = true
	r.FieldsPerRecord = 3

	records, err := r.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("parse rankings CSV %q: %w", s.FilePath, err)
	}

	rankings := make([]domain.Rank, 0, len(records))
	for i, rec := range records {
		ticker := strings.ToUpper(strings.TrimSpace(rec[0]))
		if ticker == "TICKER" {
			// Skip optional header row.
			continue
		}

		rank, err := strconv.Atoi(strings.TrimSpace(rec[1]))
		if err != nil {
			return nil, fmt.Errorf("rankings CSV row %d: invalid rank %q: %w", i+1, rec[1], err)
		}

		price, err := strconv.ParseFloat(strings.TrimSpace(rec[2]), 64)
		if err != nil {
			return nil, fmt.Errorf("rankings CSV row %d: invalid price %q: %w", i+1, rec[2], err)
		}

		rankings = append(rankings, domain.Rank{
			Ticker:   ticker,
			Position: rank,
			Price:    price,
		})
	}
	return rankings, nil
}
