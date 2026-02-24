package ranking

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/nareshpaturi/quant-stocks/internal/domain"
)

const DefaultAPIURL = "https://quantmystocks.com/api/stockrank"

// indexIDMap maps the human-readable index key (used in profile config)
// to the numeric indexId expected by the QuantMyStocks API.
var indexIDMap = map[string]string{
	"sp500": "9",
	"sp400": "13",
	"sp600": "12",
	"ndx":   "8",
}

// ── Request / Response types ──────────────────────────────────────────────────

type qmsRequest struct {
	MomDay  string `json:"momDay"`  // "YYYY-MM-DD" — most recent trading day
	AlgoID  string `json:"algoId"`  // always "1"
	IndexID string `json:"indexId"` // "8" | "9" | "12" | "13"
}

// qmsStock mirrors one entry in the QuantMyStocks API response array.
// Field names match the API's JSON keys — update tags here if the API changes.
type qmsStock struct {
	Ticker string `json:"symbol"`
	Rank   int    `json:"wgdzscorerank"`
}

// ── Adapter ───────────────────────────────────────────────────────────────────

// QuantMyStocksProvider fetches momentum rankings from the QuantMyStocks
// leaderboard API. It implements port.RankingProvider.
//
// Supported index keys (passed as indexName to GetRankings):
//
//	"sp500"  →  indexId 9
//	"sp400"  →  indexId 13
//	"sp600"  →  indexId 12
//	"ndx"    →  indexId 8
type QuantMyStocksProvider struct {
	apiURL     string
	token      string
	httpClient *http.Client
}

// NewQuantMyStocksProvider constructs the provider.
// apiURL defaults to DefaultAPIURL if empty.
// token is sent as a Bearer token in the Authorization header.
func NewQuantMyStocksProvider(apiURL, token string) *QuantMyStocksProvider {
	if apiURL == "" {
		apiURL = DefaultAPIURL
	}
	return &QuantMyStocksProvider{
		apiURL:     apiURL,
		token:      token,
		httpClient: &http.Client{Timeout: 30 * time.Second},
	}
}

// GetRankings calls the QuantMyStocks API for the given index using the most
// recent trading day. indexName must be one of: "sp500", "sp400", "sp600", "ndx".
func (q *QuantMyStocksProvider) GetRankings(ctx context.Context, indexName string) ([]domain.Rank, error) {
	return q.GetRankingsForDate(ctx, indexName, lastRankingDay())
}

// GetRankingsForDate calls the QuantMyStocks API for the given index and date.
// date must be in "YYYY-MM-DD" format (typically a Friday market close).
// Used by the backtest runner to fetch historical leaderboard snapshots.
//
// Transient failures (network errors, 5xx responses) are retried up to
// maxRankingAttempts times with exponential backoff (1 s, 2 s, 4 s, 8 s).
// Non-retryable errors (4xx) are returned immediately.
func (q *QuantMyStocksProvider) GetRankingsForDate(ctx context.Context, indexName, date string) ([]domain.Rank, error) {
	indexID, ok := indexIDMap[indexName]
	if !ok {
		return nil, fmt.Errorf(
			"unsupported index %q — supported values: sp500, sp400, sp600, ndx", indexName,
		)
	}

	payload := qmsRequest{
		MomDay:  date,
		AlgoID:  "1",
		IndexID: indexID,
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("quantmystocks marshal request: %w", err)
	}

	const maxAttempts = 5
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			// Exponential backoff: 1 s, 2 s, 4 s, 8 s
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Duration(1<<uint(attempt-1)) * time.Second):
			}
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, q.apiURL, bytes.NewReader(body))
		if err != nil {
			return nil, fmt.Errorf("quantmystocks build request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")
		if q.token != "" {
			req.Header.Set("Authorization", "Bearer "+q.token)
		}

		resp, err := q.httpClient.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("quantmystocks API call (attempt %d/%d): %w", attempt+1, maxAttempts, err)
			continue
		}

		if resp.StatusCode >= 500 {
			resp.Body.Close()
			lastErr = fmt.Errorf("quantmystocks API returned status %d (attempt %d/%d)", resp.StatusCode, attempt+1, maxAttempts)
			continue
		}

		if resp.StatusCode != http.StatusOK {
			// 4xx — not retryable (auth failure, bad request, etc.)
			resp.Body.Close()
			return nil, fmt.Errorf("quantmystocks API returned status %d", resp.StatusCode)
		}

		var stocks []qmsStock
		decErr := json.NewDecoder(resp.Body).Decode(&stocks)
		resp.Body.Close()
		if decErr != nil {
			return nil, fmt.Errorf("quantmystocks decode response: %w", decErr)
		}

		ranks := make([]domain.Rank, 0, len(stocks))
		for _, s := range stocks {
			ranks = append(ranks, domain.Rank{
				Ticker:   s.Ticker,
				Position: s.Rank,
				// Price is not included in the API response; the service layer
				// fetches prices via Broker.GetQuotes after rankings are loaded.
			})
		}
		return ranks, nil
	}

	return nil, lastErr
}

// lastRankingDay returns the most recent Sunday date as "YYYY-MM-DD".
// The QuantMyStocks leaderboard is published every Sunday, so this is the
// date to pass as momDay to retrieve the current week's rankings.
// When the Action runs on Monday, this returns yesterday (Sunday).
func lastRankingDay() string {
	t := time.Now().UTC()
	// time.Weekday: Sunday=0, Monday=1, ..., Saturday=6
	// Subtracting the weekday number always lands on the most recent Sunday.
	t = t.AddDate(0, 0, -int(t.Weekday()))
	return t.Format("2006-01-02")
}
