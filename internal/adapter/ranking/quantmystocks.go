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

// GetRankings calls the QuantMyStocks API for the given index and returns the
// ranked stock list. indexName must be one of: "sp500", "sp400", "sp600", "ndx".
func (q *QuantMyStocksProvider) GetRankings(ctx context.Context, indexName string) ([]domain.Rank, error) {
	indexID, ok := indexIDMap[indexName]
	if !ok {
		return nil, fmt.Errorf(
			"unsupported index %q — supported values: sp500, sp400, sp600, ndx", indexName,
		)
	}

	payload := qmsRequest{
		MomDay:  lastTradingDay(),
		AlgoID:  "1",
		IndexID: indexID,
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("quantmystocks marshal request: %w", err)
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
		return nil, fmt.Errorf("quantmystocks API call: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("quantmystocks API returned status %d", resp.StatusCode)
	}

	var stocks []qmsStock
	if err := json.NewDecoder(resp.Body).Decode(&stocks); err != nil {
		return nil, fmt.Errorf("quantmystocks decode response: %w", err)
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

// lastTradingDay returns the most recent weekday date as "YYYY-MM-DD".
// When the Action runs on Monday, this returns the prior Friday so the
// rankings reflect Friday's market close — the latest settled data.
func lastTradingDay() string {
	t := time.Now().UTC()
	switch t.Weekday() {
	case time.Monday:
		t = t.AddDate(0, 0, -3) // Monday → Friday
	case time.Sunday:
		t = t.AddDate(0, 0, -2) // Sunday → Friday
	case time.Saturday:
		t = t.AddDate(0, 0, -1) // Saturday → Friday
	}
	return t.Format("2006-01-02")
}
