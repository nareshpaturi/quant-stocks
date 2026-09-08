package broker

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
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
// TradierBroker implements port.Broker using the Tradier REST API v1.
//
// Authentication uses a Bearer token set via the TRADIER_TOKEN environment
// variable. Set sandbox=true to use the paper trading environment.
//
// All strategy orders are market orders with duration "gtc".
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
	Position []tradierRawPosition
}

// UnmarshalJSON handles three Tradier response shapes for the "positions" field:
//  1. The string "null"  — no positions held
//  2. {"position": {...}} — a single position (object, not array)
//  3. {"position": [{…}, …]} — multiple positions (array)
func (w *tradierPositionWrapper) UnmarshalJSON(data []byte) error {
	if string(data) == `"null"` {
		return nil
	}
	var raw struct {
		Position json.RawMessage `json:"position"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	if len(raw.Position) == 0 || string(raw.Position) == "null" {
		return nil
	}
	if raw.Position[0] == '[' {
		return json.Unmarshal(raw.Position, &w.Position)
	}
	var p tradierRawPosition
	if err := json.Unmarshal(raw.Position, &p); err != nil {
		return err
	}
	w.Position = []tradierRawPosition{p}
	return nil
}

type tradierRawPosition struct {
	Symbol   string  `json:"symbol"`
	Quantity float64 `json:"quantity"`
}

type tradierQuoteResponse struct {
	Quotes tradierQuoteWrapper `json:"quotes"`
}

type tradierQuoteWrapper struct {
	Quote []tradierRawQuote
}

// UnmarshalJSON handles two Tradier response shapes for the "quotes" field:
//  1. {"quote": {...}}    — a single quote (object, not array)
//  2. {"quote": [{…}, …]} — multiple quotes (array)
func (w *tradierQuoteWrapper) UnmarshalJSON(data []byte) error {
	if string(data) == `"null"` {
		return nil
	}
	var raw struct {
		Quote json.RawMessage `json:"quote"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	if len(raw.Quote) == 0 || string(raw.Quote) == "null" {
		return nil
	}
	if raw.Quote[0] == '[' {
		return json.Unmarshal(raw.Quote, &w.Quote)
	}
	var q tradierRawQuote
	if err := json.Unmarshal(raw.Quote, &q); err != nil {
		return err
	}
	w.Quote = []tradierRawQuote{q}
	return nil
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
	Order []tradierRawOrder
}

// UnmarshalJSON handles three Tradier response shapes for the "orders" field:
//  1. The string "null"  — no open orders
//  2. {"order": {...}}   — a single order (object, not array)
//  3. {"order": [{…}, …]} — multiple orders (array)
func (w *tradierOrdersWrapper) UnmarshalJSON(data []byte) error {
	if string(data) == `"null"` {
		return nil
	}
	var raw struct {
		Order json.RawMessage `json:"order"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	if len(raw.Order) == 0 || string(raw.Order) == "null" {
		return nil
	}
	if raw.Order[0] == '[' {
		return json.Unmarshal(raw.Order, &w.Order)
	}
	var o tradierRawOrder
	if err := json.Unmarshal(raw.Order, &o); err != nil {
		return err
	}
	w.Order = []tradierRawOrder{o}
	return nil
}

type tradierRawOrder struct {
	ID                int     `json:"id"`
	Symbol            string  `json:"symbol"`
	Side              string  `json:"side"` // "buy", "sell", etc.
	Quantity          float64 `json:"quantity"`
	ExecQuantity      float64 `json:"exec_quantity"`
	RemainingQuantity float64 `json:"remaining_quantity"`
	AvgFillPrice      float64 `json:"avg_fill_price"`
	Status            string  `json:"status"`
	CreateDate        string  `json:"create_date"`
	TransactionDate   string  `json:"transaction_date"`
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

// ── GetAccount ────────────────────────────────────────────────────────────────

// GetAccount returns Tradier's cash_available as both cash and buying power.
func (t *TradierBroker) GetAccount(ctx context.Context) (domain.AccountSnapshot, error) {
	endpoint := fmt.Sprintf("%s/accounts/%s/balances", t.baseURL, t.accountID)
	var resp tradierBalancesResponse
	if err := t.get(ctx, endpoint, &resp); err != nil {
		return domain.AccountSnapshot{}, fmt.Errorf("tradier GetAccount: %w", err)
	}
	cash := resp.Balances.Cash.CashAvailable
	return domain.AccountSnapshot{AccountID: t.accountID, Cash: cash, BuyingPower: cash}, nil
}

// ── GetOrders ─────────────────────────────────────────────────────────────────

// GetOrders returns Tradier orders normalized into the shared lifecycle model.
func (t *TradierBroker) GetOrders(ctx context.Context, query domain.OrderQuery) ([]domain.BrokerOrder, error) {
	endpoint := fmt.Sprintf("%s/accounts/%s/orders?limit=1000", t.baseURL, t.accountID)
	var resp tradierOrdersResponse
	if err := t.get(ctx, endpoint, &resp); err != nil {
		return nil, fmt.Errorf("tradier GetOrders: %w", err)
	}

	orders := make([]domain.BrokerOrder, 0, len(resp.Orders.Order))
	for _, o := range resp.Orders.Order {
		side := domain.OrderSideBuy
		if strings.HasPrefix(o.Side, "sell") {
			side = domain.OrderSideSell
		}
		createdAt := parseTradierTime(o.CreateDate)
		if createdAt.IsZero() {
			createdAt = parseTradierTime(o.TransactionDate)
		}
		if !query.From.IsZero() && !createdAt.IsZero() && createdAt.Before(query.From) {
			continue
		}
		if !query.To.IsZero() && !createdAt.IsZero() && createdAt.After(query.To) {
			continue
		}
		filled := o.ExecQuantity
		status := normalizeTradierStatus(o.Status)
		if status == domain.BrokerOrderFilled && filled <= 0 {
			filled = o.Quantity
		}
		remaining := o.RemainingQuantity
		if remaining <= 0 && (status == domain.BrokerOrderOpen || status == domain.BrokerOrderPending || status == domain.BrokerOrderPartiallyFilled) {
			remaining = math.Max(0, o.Quantity-filled)
		}
		orders = append(orders, domain.BrokerOrder{
			ID: fmt.Sprintf("%d", o.ID), AccountID: t.accountID,
			Ticker: strings.ToUpper(o.Symbol), Side: side, Type: domain.OrderTypeMarket,
			Duration: domain.OrderDurationGTC, RequestedShares: o.Quantity,
			FilledShares: filled, RemainingShares: remaining, AverageFillPrice: o.AvgFillPrice,
			Status: status, CreatedAt: createdAt, UpdatedAt: createdAt, CycleID: query.CycleID,
		})
	}
	return orders, nil
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

// ── ReviewOrder / PlaceOrder ─────────────────────────────────────────────────

// ReviewOrder resolves Tradier's exact whole-share quantity locally.
func (t *TradierBroker) ReviewOrder(ctx context.Context, order domain.Order) (domain.OrderReview, error) {
	if order.Type == "" {
		order.Type = domain.OrderTypeMarket
	}
	if order.Type != domain.OrderTypeMarket {
		return domain.OrderReview{}, fmt.Errorf("tradier ReviewOrder: unsupported type %q", order.Type)
	}
	if order.Duration == "" {
		order.Duration = domain.OrderDurationGTC
	}
	if order.Side == domain.OrderSideBuy {
		prices, err := t.fetchQuotes(ctx, []string{order.Ticker})
		if err != nil {
			return domain.OrderReview{}, fmt.Errorf("tradier ReviewOrder: fresh quote for %s: %w", order.Ticker, err)
		}
		livePrice := prices[order.Ticker]
		if livePrice <= 0 {
			return domain.OrderReview{}, fmt.Errorf("tradier ReviewOrder: no live price for %s", order.Ticker)
		}
		order.Shares = math.Floor(order.Notional / livePrice)
		if order.Shares < 1 {
			return domain.OrderReview{}, fmt.Errorf("tradier ReviewOrder: notional %.2f buys no whole shares of %s at %.2f", order.Notional, order.Ticker, livePrice)
		}
		return domain.OrderReview{Order: order, EstimatedPrice: livePrice, EstimatedNotional: order.Shares * livePrice, Approved: true}, nil
	}
	if order.Side != domain.OrderSideSell || order.Shares <= 0 {
		return domain.OrderReview{}, fmt.Errorf("tradier ReviewOrder: invalid %s order for %s", order.Side, order.Ticker)
	}
	return domain.OrderReview{Order: order, EstimatedNotional: order.Shares * order.ReferencePrice, Approved: true}, nil
}

// PlaceOrder submits exactly the reviewed market equity order to Tradier.
func (t *TradierBroker) PlaceOrder(ctx context.Context, review domain.OrderReview) (domain.BrokerOrder, error) {
	if !review.Approved {
		return domain.BrokerOrder{}, fmt.Errorf("tradier PlaceOrder: order was not approved")
	}
	order := review.Order
	endpoint := fmt.Sprintf("%s/accounts/%s/orders", t.baseURL, t.accountID)

	form := url.Values{}
	form.Set("class", "equity")
	form.Set("symbol", order.Ticker)
	form.Set("side", string(order.Side))
	form.Set("type", string(order.Type))
	form.Set("duration", string(order.Duration))
	form.Set("quantity", fmt.Sprintf("%.0f", order.Shares))

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint,
		strings.NewReader(form.Encode()))
	if err != nil {
		return domain.BrokerOrder{}, fmt.Errorf("tradier PlaceOrder build request: %w", err)
	}
	t.setHeaders(req)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := t.httpClient.Do(req)
	if err != nil {
		return domain.BrokerOrder{}, fmt.Errorf("tradier PlaceOrder: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return domain.BrokerOrder{}, fmt.Errorf("tradier PlaceOrder: status %d body: %s", resp.StatusCode, body)
	}

	var placeResp tradierPlaceOrderResponse
	if err := json.NewDecoder(resp.Body).Decode(&placeResp); err != nil {
		return domain.BrokerOrder{}, fmt.Errorf("tradier PlaceOrder decode: %w", err)
	}
	if placeResp.Order.Status != "ok" {
		return domain.BrokerOrder{}, fmt.Errorf("tradier PlaceOrder: order status %q", placeResp.Order.Status)
	}
	return domain.BrokerOrder{
		ID: fmt.Sprintf("%d", placeResp.Order.ID), AccountID: t.accountID,
		ClientOrderID: order.ClientOrderID, Ticker: order.Ticker, Side: order.Side,
		Type: order.Type, Duration: order.Duration, RequestedShares: order.Shares,
		RemainingShares: order.Shares, EstimatedPrice: review.EstimatedPrice,
		EstimatedNotional: review.EstimatedNotional, Status: domain.BrokerOrderPending,
		CycleID: order.CycleID,
	}, nil
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

func normalizeTradierStatus(status string) domain.BrokerOrderStatus {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "pending", "pending_cancel", "ok":
		return domain.BrokerOrderPending
	case "open":
		return domain.BrokerOrderOpen
	case "partially_filled":
		return domain.BrokerOrderPartiallyFilled
	case "filled":
		return domain.BrokerOrderFilled
	case "canceled", "cancelled":
		return domain.BrokerOrderCanceled
	case "rejected":
		return domain.BrokerOrderRejected
	case "expired":
		return domain.BrokerOrderExpired
	default:
		return domain.BrokerOrderUnknown
	}
}

func parseTradierTime(value string) time.Time {
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05Z", "2006-01-02 15:04:05"} {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed
		}
	}
	return time.Time{}
}
