package broker

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/nareshpaturi/quant-stocks/internal/domain"
)

const (
	tradierLiveURL    = "https://api.tradier.com/v1"
	tradierSandboxURL = "https://sandbox.tradier.com/v1"
)

// activeOrderStatuses is the set of order statuses considered "open" for
// idempotency purposes. Any order in one of these states means the same
// (ticker, side) trade must not be re-submitted.
var activeOrderStatuses = map[string]bool{
	"open":              true,
	"partially_filled":  true,
	"pending":           true,
}

// TradierBroker implements port.Broker using the Tradier REST API v1.
//
// Authentication uses a Bearer token set via the TRADIER_TOKEN environment
// variable. Set sandbox=true to use the paper trading environment.
//
// All orders are market orders with duration "day".
type TradierBroker struct {
	httpClient *http.Client
	token      string
	accountID  string
	baseURL    string
}

// NewTradierBroker constructs a TradierBroker.
// token and accountID are read from environment variables by the caller (main.go).
// Set sandbox=true to use the Tradier sandbox environment.
func NewTradierBroker(token, accountID string, sandbox bool) *TradierBroker {
	base := tradierLiveURL
	if sandbox {
		base = tradierSandboxURL
	}
	return &TradierBroker{
		httpClient: &http.Client{Timeout: 30 * time.Second},
		token:      token,
		accountID:  accountID,
		baseURL:    base,
	}
}

// ── API response shapes ───────────────────────────────────────────────────────

type tradierPositionsResponse struct {
	Positions tradierPositionWrapper `json:"positions"`
}

type tradierPositionWrapper struct {
	// Tradier returns a single object when there is exactly one position and
	// an array when there are many. We always decode into a slice.
	Position []tradierRawPosition `json:"position"`
}

type tradierRawPosition struct {
	Symbol   string  `json:"symbol"`
	Quantity float64 `json:"quantity"`
}

type tradierQuoteResponse struct {
	Quotes struct {
		Quote []tradierRawQuote `json:"quote"`
	} `json:"quotes"`
}

type tradierRawQuote struct {
	Symbol string  `json:"symbol"`
	Last   float64 `json:"last"`
}

type tradierBalancesResponse struct {
	Balances struct {
		Cash struct {
			CashAvailable float64 `json:"cash_available"`
		} `json:"cash"`
	} `json:"balances"`
}

type tradierOrdersResponse struct {
	Orders tradierOrdersWrapper `json:"orders"`
}

type tradierOrdersWrapper struct {
	Order []tradierRawOrder `json:"order"`
}

type tradierRawOrder struct {
	Symbol   string  `json:"symbol"`
	Side     string  `json:"side"`     // "buy", "sell", etc.
	Quantity float64 `json:"quantity"`
	Status   string  `json:"status"`
}

type tradierPlaceOrderResponse struct {
	Order struct {
		ID     int    `json:"id"`
		Status string `json:"status"`
	} `json:"order"`
}

// ── GetPositions ─────────────────────────────────────────────────────────────

// GetPositions fetches all open positions and enriches them with the latest
// market price via Tradier's quotes endpoint.
func (t *TradierBroker) GetPositions(ctx context.Context) ([]domain.Position, error) {
	endpoint := fmt.Sprintf("%s/accounts/%s/positions", t.baseURL, t.accountID)
	var posResp tradierPositionsResponse
	if err := t.get(ctx, endpoint, &posResp); err != nil {
		return nil, fmt.Errorf("tradier GetPositions: %w", err)
	}

	raw := posResp.Positions.Position
	if len(raw) == 0 {
		return nil, nil
	}

	// Collect symbols and batch-fetch current prices.
	symbols := make([]string, len(raw))
	for i, p := range raw {
		symbols[i] = p.Symbol
	}
	prices, err := t.fetchQuotes(ctx, symbols)
	if err != nil {
		return nil, fmt.Errorf("tradier GetPositions quotes: %w", err)
	}

	positions := make([]domain.Position, 0, len(raw))
	for _, p := range raw {
		positions = append(positions, domain.Position{
			Ticker:       p.Symbol,
			Shares:       p.Quantity,
			CurrentPrice: prices[p.Symbol],
		})
	}
	return positions, nil
}

// ── GetCash ───────────────────────────────────────────────────────────────────

// GetCash returns the cash_available balance from the account's cash sub-object.
func (t *TradierBroker) GetCash(ctx context.Context) (float64, error) {
	endpoint := fmt.Sprintf("%s/accounts/%s/balances", t.baseURL, t.accountID)
	var resp tradierBalancesResponse
	if err := t.get(ctx, endpoint, &resp); err != nil {
		return 0, fmt.Errorf("tradier GetCash: %w", err)
	}
	return resp.Balances.Cash.CashAvailable, nil
}

// ── GetOpenOrders ─────────────────────────────────────────────────────────────

// GetOpenOrders fetches all orders from the account and returns only those
// whose status is "open", "partially_filled", or "pending".
// The service layer uses this to skip orders that are already in flight.
func (t *TradierBroker) GetOpenOrders(ctx context.Context) ([]domain.OpenOrder, error) {
	endpoint := fmt.Sprintf("%s/accounts/%s/orders", t.baseURL, t.accountID)
	var resp tradierOrdersResponse
	if err := t.get(ctx, endpoint, &resp); err != nil {
		return nil, fmt.Errorf("tradier GetOpenOrders: %w", err)
	}

	var open []domain.OpenOrder
	for _, o := range resp.Orders.Order {
		if !activeOrderStatuses[o.Status] {
			continue
		}
		side := domain.OrderSideBuy
		if strings.HasPrefix(o.Side, "sell") {
			side = domain.OrderSideSell
		}
		open = append(open, domain.OpenOrder{
			Ticker: o.Symbol,
			Side:   side,
			Shares: o.Quantity,
			Status: o.Status,
		})
	}
	return open, nil
}

// ── GetQuotes ─────────────────────────────────────────────────────────────────

// GetQuotes returns the latest last-trade price for each requested ticker.
func (t *TradierBroker) GetQuotes(ctx context.Context, tickers []string) (map[string]float64, error) {
	if len(tickers) == 0 {
		return map[string]float64{}, nil
	}
	prices, err := t.fetchQuotes(ctx, tickers)
	if err != nil {
		return nil, fmt.Errorf("tradier GetQuotes: %w", err)
	}
	return prices, nil
}

// ── ExecuteOrder ─────────────────────────────────────────────────────────────

// ExecuteOrder submits a market equity order to Tradier using a form-encoded
// POST. Returns the Tradier-assigned numeric order ID as a string on success.
func (t *TradierBroker) ExecuteOrder(ctx context.Context, order domain.Order) (string, error) {
	endpoint := fmt.Sprintf("%s/accounts/%s/orders", t.baseURL, t.accountID)

	form := url.Values{}
	form.Set("class", "equity")
	form.Set("symbol", order.Ticker)
	form.Set("side", string(order.Side))
	form.Set("quantity", fmt.Sprintf("%.0f", order.Shares))
	form.Set("type", "market")
	form.Set("duration", "day")

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint,
		strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("tradier ExecuteOrder build request: %w", err)
	}
	t.setHeaders(req)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := t.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("tradier ExecuteOrder: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("tradier ExecuteOrder: unexpected status %d", resp.StatusCode)
	}

	var placeResp tradierPlaceOrderResponse
	if err := json.NewDecoder(resp.Body).Decode(&placeResp); err != nil {
		return "", fmt.Errorf("tradier ExecuteOrder decode: %w", err)
	}
	if placeResp.Order.Status != "ok" {
		return "", fmt.Errorf("tradier ExecuteOrder: order status %q", placeResp.Order.Status)
	}
	return fmt.Sprintf("%d", placeResp.Order.ID), nil
}

// ── IsMarketOpen ─────────────────────────────────────────────────────────────

type tradierClockResponse struct {
	Clock struct {
		State string `json:"state"` // "premarket", "open", "postmarket", "closed"
	} `json:"clock"`
}

// IsMarketOpen returns true when the Tradier market clock reports state "open".
// The backtest runner calls this before starting to ensure paper orders can fill.
func (t *TradierBroker) IsMarketOpen(ctx context.Context) (bool, error) {
	endpoint := fmt.Sprintf("%s/markets/clock", t.baseURL)
	var resp tradierClockResponse
	if err := t.get(ctx, endpoint, &resp); err != nil {
		return false, fmt.Errorf("tradier IsMarketOpen: %w", err)
	}
	return resp.Clock.State == "open", nil
}

// ── Helpers ───────────────────────────────────────────────────────────────────

// get performs a GET request and JSON-decodes the response body into dest.
func (t *TradierBroker) get(ctx context.Context, endpoint string, dest any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	t.setHeaders(req)

	resp, err := t.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status %d from %s", resp.StatusCode, endpoint)
	}
	return json.NewDecoder(resp.Body).Decode(dest)
}

// fetchQuotes returns a map of symbol → last price for the given symbols.
// Tradier's quotes endpoint accepts a comma-separated symbols query parameter.
func (t *TradierBroker) fetchQuotes(ctx context.Context, symbols []string) (map[string]float64, error) {
	endpoint := fmt.Sprintf("%s/markets/quotes?symbols=%s&greeks=false",
		t.baseURL, strings.Join(symbols, ","))
	var resp tradierQuoteResponse
	if err := t.get(ctx, endpoint, &resp); err != nil {
		return nil, err
	}

	prices := make(map[string]float64, len(resp.Quotes.Quote))
	for _, q := range resp.Quotes.Quote {
		prices[q.Symbol] = q.Last
	}
	return prices, nil
}

// setHeaders attaches the Tradier Bearer token and Accept headers.
func (t *TradierBroker) setHeaders(req *http.Request) {
	req.Header.Set("Authorization", "Bearer "+t.token)
	req.Header.Set("Accept", "application/json")
}
