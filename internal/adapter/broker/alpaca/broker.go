// Package alpaca implements port.OvernightBroker with the Alpaca Trading and
// Market Data REST APIs.
package alpaca

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
	_ "time/tzdata"

	"github.com/nareshpaturi/quant-stocks/internal/domain"
)

const (
	LiveURL  = "https://api.alpaca.markets"
	PaperURL = "https://paper-api.alpaca.markets"
	DataURL  = "https://data.alpaca.markets"

	maxAttempts = 3
)

// errNotFound marks an Alpaca 404 for a missing position or order.
var errNotFound = errors.New("not found")

// Broker talks to one Alpaca account. Paper and live accounts use different
// key pairs, so the keys must match the selected base URL.
type Broker struct {
	httpClient *http.Client
	keyID      string
	secretKey  string
	tradingURL string
	dataURL    string
	feed       string
	newYork    *time.Location
}

// New constructs a Broker. feed selects the market data feed for quotes:
// "iex" works on Alpaca's free plan, "sip" needs a paid data subscription.
func New(keyID, secretKey string, paper bool, feed string) (*Broker, error) {
	newYork, err := time.LoadLocation("America/New_York")
	if err != nil {
		return nil, fmt.Errorf("alpaca: load New York timezone: %w", err)
	}
	tradingURL := LiveURL
	if paper {
		tradingURL = PaperURL
	}
	return &Broker{
		httpClient: &http.Client{Timeout: 15 * time.Second},
		keyID:      keyID,
		secretKey:  secretKey,
		tradingURL: tradingURL,
		dataURL:    DataURL,
		feed:       feed,
		newYork:    newYork,
	}, nil
}

// ── API response shapes ───────────────────────────────────────────────────────

type clockResponse struct {
	Timestamp time.Time `json:"timestamp"`
	IsOpen    bool      `json:"is_open"`
	NextOpen  time.Time `json:"next_open"`
	NextClose time.Time `json:"next_close"`
}

// calendarDay times are New York wall-clock "HH:MM" strings.
type calendarDay struct {
	Date  string `json:"date"`
	Open  string `json:"open"`
	Close string `json:"close"`
}

type accountResponse struct {
	AccountNumber  string `json:"account_number"`
	Status         string `json:"status"`
	Cash           string `json:"cash"`
	BuyingPower    string `json:"buying_power"`
	TradingBlocked bool   `json:"trading_blocked"`
	AccountBlocked bool   `json:"account_blocked"`
}

type positionResponse struct {
	Qty string `json:"qty"`
}

type snapshotResponse struct {
	LatestTrade *struct {
		Price float64 `json:"p"`
	} `json:"latestTrade"`
	LatestQuote *struct {
		Ask float64 `json:"ap"`
		Bid float64 `json:"bp"`
	} `json:"latestQuote"`
}

type assetResponse struct {
	Fractionable bool `json:"fractionable"`
}

// orderRequest carries exactly one of Qty and Notional; Alpaca rejects both.
type orderRequest struct {
	Symbol        string `json:"symbol"`
	Qty           string `json:"qty,omitempty"`
	Notional      string `json:"notional,omitempty"`
	Side          string `json:"side"`
	Type          string `json:"type"`
	TimeInForce   string `json:"time_in_force"`
	ClientOrderID string `json:"client_order_id"`
}

type orderResponse struct {
	ID             string     `json:"id"`
	ClientOrderID  string     `json:"client_order_id"`
	Symbol         string     `json:"symbol"`
	Side           string     `json:"side"`
	Type           string     `json:"type"`
	TimeInForce    string     `json:"time_in_force"`
	Qty            *string    `json:"qty"`
	Notional       *string    `json:"notional"`
	FilledQty      string     `json:"filled_qty"`
	FilledAvgPrice *string    `json:"filled_avg_price"`
	Status         string     `json:"status"`
	SubmittedAt    *time.Time `json:"submitted_at"`
	UpdatedAt      *time.Time `json:"updated_at"`
	FilledAt       *time.Time `json:"filled_at"`
}

// ── Market clock and calendar ────────────────────────────────────────────────

// GetClock returns Alpaca's market clock.
func (b *Broker) GetClock(ctx context.Context) (domain.MarketClock, error) {
	var resp clockResponse
	if err := b.do(ctx, http.MethodGet, b.tradingURL+"/v2/clock", nil, &resp); err != nil {
		return domain.MarketClock{}, fmt.Errorf("alpaca GetClock: %w", err)
	}
	return domain.MarketClock{
		Timestamp: resp.Timestamp, IsOpen: resp.IsOpen,
		NextOpen: resp.NextOpen, NextClose: resp.NextClose,
	}, nil
}

// GetSession returns the regular session for date, or false on a market
// holiday or weekend.
func (b *Broker) GetSession(ctx context.Context, date string) (domain.MarketSession, bool, error) {
	query := url.Values{"start": {date}, "end": {date}}
	var days []calendarDay
	if err := b.do(ctx, http.MethodGet, b.tradingURL+"/v2/calendar?"+query.Encode(), nil, &days); err != nil {
		return domain.MarketSession{}, false, fmt.Errorf("alpaca GetSession: %w", err)
	}
	for _, day := range days {
		if day.Date != date {
			continue
		}
		open, err := time.ParseInLocation("2006-01-02 15:04", date+" "+day.Open, b.newYork)
		if err != nil {
			return domain.MarketSession{}, false, fmt.Errorf("alpaca GetSession: open time %q: %w", day.Open, err)
		}
		closing, err := time.ParseInLocation("2006-01-02 15:04", date+" "+day.Close, b.newYork)
		if err != nil {
			return domain.MarketSession{}, false, fmt.Errorf("alpaca GetSession: close time %q: %w", day.Close, err)
		}
		return domain.MarketSession{Date: date, Open: open, Close: closing}, true, nil
	}
	return domain.MarketSession{}, false, nil
}

// ── Account and position ─────────────────────────────────────────────────────

// GetAccount returns cash and buying power, and fails when Alpaca has blocked
// trading on the account.
func (b *Broker) GetAccount(ctx context.Context) (domain.AccountSnapshot, error) {
	var resp accountResponse
	if err := b.do(ctx, http.MethodGet, b.tradingURL+"/v2/account", nil, &resp); err != nil {
		return domain.AccountSnapshot{}, fmt.Errorf("alpaca GetAccount: %w", err)
	}
	if resp.TradingBlocked || resp.AccountBlocked {
		return domain.AccountSnapshot{}, fmt.Errorf("alpaca GetAccount: account %s is blocked from trading (status %s)", resp.AccountNumber, resp.Status)
	}
	cash, err := decimal(resp.Cash)
	if err != nil {
		return domain.AccountSnapshot{}, fmt.Errorf("alpaca GetAccount: cash %q: %w", resp.Cash, err)
	}
	buyingPower, err := decimal(resp.BuyingPower)
	if err != nil {
		return domain.AccountSnapshot{}, fmt.Errorf("alpaca GetAccount: buying_power %q: %w", resp.BuyingPower, err)
	}
	return domain.AccountSnapshot{AccountID: resp.AccountNumber, Cash: cash, BuyingPower: buyingPower}, nil
}

// GetPositionShares returns the shares held of symbol. Alpaca answers 404 when
// there is no position.
func (b *Broker) GetPositionShares(ctx context.Context, symbol string) (float64, error) {
	var resp positionResponse
	err := b.do(ctx, http.MethodGet, b.tradingURL+"/v2/positions/"+url.PathEscape(symbol), nil, &resp)
	if errors.Is(err, errNotFound) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("alpaca GetPositionShares %s: %w", symbol, err)
	}
	shares, err := decimal(resp.Qty)
	if err != nil {
		return 0, fmt.Errorf("alpaca GetPositionShares %s: qty %q: %w", symbol, resp.Qty, err)
	}
	return shares, nil
}

// GetQuote returns the latest quote and trade from the configured data feed.
func (b *Broker) GetQuote(ctx context.Context, symbol string) (domain.Quote, error) {
	query := url.Values{"feed": {b.feed}}
	endpoint := b.dataURL + "/v2/stocks/" + url.PathEscape(symbol) + "/snapshot?" + query.Encode()
	var resp snapshotResponse
	if err := b.do(ctx, http.MethodGet, endpoint, nil, &resp); err != nil {
		return domain.Quote{}, fmt.Errorf("alpaca GetQuote %s: %w", symbol, err)
	}
	var quote domain.Quote
	if resp.LatestQuote != nil {
		quote.Bid, quote.Ask = resp.LatestQuote.Bid, resp.LatestQuote.Ask
	}
	if resp.LatestTrade != nil {
		quote.Last = resp.LatestTrade.Price
	}
	return quote, nil
}

// IsFractionable reports Alpaca's fractionable flag for symbol.
func (b *Broker) IsFractionable(ctx context.Context, symbol string) (bool, error) {
	var resp assetResponse
	if err := b.do(ctx, http.MethodGet, b.tradingURL+"/v2/assets/"+url.PathEscape(symbol), nil, &resp); err != nil {
		return false, fmt.Errorf("alpaca IsFractionable %s: %w", symbol, err)
	}
	return resp.Fractionable, nil
}

// ── Orders ────────────────────────────────────────────────────────────────────

// PlaceOrder submits a market order for Shares or for a Notional dollar
// amount. Only day and opg time-in-force are used by the strategy (Alpaca
// allows only day for dollar orders), and every order must carry a
// ClientOrderID so that a failed submission can be checked for later.
func (b *Broker) PlaceOrder(ctx context.Context, order domain.Order) (domain.BrokerOrder, error) {
	if order.Type != domain.OrderTypeMarket {
		return domain.BrokerOrder{}, fmt.Errorf("alpaca PlaceOrder: unsupported type %q", order.Type)
	}
	if order.Duration != domain.OrderDurationDay && order.Duration != domain.OrderDurationOPG {
		return domain.BrokerOrder{}, fmt.Errorf("alpaca PlaceOrder: unsupported time in force %q", order.Duration)
	}
	if order.ClientOrderID == "" {
		return domain.BrokerOrder{}, fmt.Errorf("alpaca PlaceOrder: order needs a client order id")
	}
	req := orderRequest{
		Symbol: order.Ticker, Side: string(order.Side), Type: string(order.Type),
		TimeInForce: string(order.Duration), ClientOrderID: order.ClientOrderID,
	}
	switch {
	case order.Notional > 0 && order.Shares == 0 && order.Duration == domain.OrderDurationDay:
		req.Notional = strconv.FormatFloat(order.Notional, 'f', 2, 64)
	case order.Shares > 0 && order.Notional == 0:
		req.Qty = strconv.FormatFloat(order.Shares, 'f', -1, 64)
	default:
		return domain.BrokerOrder{}, fmt.Errorf("alpaca PlaceOrder: order needs either shares or a day dollar amount, got shares %v notional %v (%s)",
			order.Shares, order.Notional, order.Duration)
	}
	var resp orderResponse
	if err := b.do(ctx, http.MethodPost, b.tradingURL+"/v2/orders", req, &resp); err != nil {
		return domain.BrokerOrder{}, fmt.Errorf("alpaca PlaceOrder: %w", err)
	}
	return resp.toDomain()
}

// GetOrder returns the current state of an order.
func (b *Broker) GetOrder(ctx context.Context, id string) (domain.BrokerOrder, error) {
	var resp orderResponse
	if err := b.do(ctx, http.MethodGet, b.tradingURL+"/v2/orders/"+url.PathEscape(id), nil, &resp); err != nil {
		return domain.BrokerOrder{}, fmt.Errorf("alpaca GetOrder %s: %w", id, err)
	}
	return resp.toDomain()
}

// GetOrderByClientID looks an order up by the client order id it was placed with.
func (b *Broker) GetOrderByClientID(ctx context.Context, clientOrderID string) (domain.BrokerOrder, bool, error) {
	query := url.Values{"client_order_id": {clientOrderID}}
	var resp orderResponse
	err := b.do(ctx, http.MethodGet, b.tradingURL+"/v2/orders:by_client_order_id?"+query.Encode(), nil, &resp)
	if errors.Is(err, errNotFound) {
		return domain.BrokerOrder{}, false, nil
	}
	if err != nil {
		return domain.BrokerOrder{}, false, fmt.Errorf("alpaca GetOrderByClientID %s: %w", clientOrderID, err)
	}
	order, err := resp.toDomain()
	return order, err == nil, err
}

// CancelOrder requests cancellation of an open order.
func (b *Broker) CancelOrder(ctx context.Context, id string) error {
	if err := b.do(ctx, http.MethodDelete, b.tradingURL+"/v2/orders/"+url.PathEscape(id), nil, nil); err != nil {
		return fmt.Errorf("alpaca CancelOrder %s: %w", id, err)
	}
	return nil
}

func (o orderResponse) toDomain() (domain.BrokerOrder, error) {
	var requested float64
	if o.Qty != nil {
		var err error
		if requested, err = decimal(*o.Qty); err != nil {
			return domain.BrokerOrder{}, fmt.Errorf("order %s qty %q: %w", o.ID, *o.Qty, err)
		}
	}
	filled, err := decimal(o.FilledQty)
	if err != nil {
		return domain.BrokerOrder{}, fmt.Errorf("order %s filled_qty %q: %w", o.ID, o.FilledQty, err)
	}
	var notional float64
	if o.Notional != nil {
		if notional, err = decimal(*o.Notional); err != nil {
			return domain.BrokerOrder{}, fmt.Errorf("order %s notional %q: %w", o.ID, *o.Notional, err)
		}
	}
	var avgPrice float64
	if o.FilledAvgPrice != nil {
		if avgPrice, err = decimal(*o.FilledAvgPrice); err != nil {
			return domain.BrokerOrder{}, fmt.Errorf("order %s filled_avg_price %q: %w", o.ID, *o.FilledAvgPrice, err)
		}
	}

	status := normalizeStatus(o.Status)
	// "calculated" means the order is done for the day with settlement math
	// still pending; whether it filled is only visible in the quantities.
	if o.Status == "calculated" {
		status = domain.BrokerOrderExpired
		if (requested > 0 && filled >= requested) || (requested == 0 && filled > 0) {
			status = domain.BrokerOrderFilled
		}
	}
	// A dollar order has no requested quantity; its amount is carried in
	// EstimatedNotional.
	order := domain.BrokerOrder{
		ID: o.ID, ClientOrderID: o.ClientOrderID, Ticker: o.Symbol,
		Side: domain.OrderSide(o.Side), Type: domain.OrderType(o.Type),
		Duration: domain.OrderDuration(o.TimeInForce), RequestedShares: requested,
		FilledShares: filled, AverageFillPrice: avgPrice, EstimatedNotional: notional,
		Status: status,
	}
	if order.Active() && requested > 0 {
		order.RemainingShares = requested - filled
	}
	if o.SubmittedAt != nil {
		order.CreatedAt = *o.SubmittedAt
	}
	switch {
	case o.FilledAt != nil:
		order.UpdatedAt = *o.FilledAt
	case o.UpdatedAt != nil:
		order.UpdatedAt = *o.UpdatedAt
	}
	return order, nil
}

func normalizeStatus(status string) domain.BrokerOrderStatus {
	switch status {
	case "pending_new", "accepted", "accepted_for_bidding", "held":
		return domain.BrokerOrderPending
	case "new", "pending_cancel", "pending_replace", "stopped", "suspended":
		return domain.BrokerOrderOpen
	case "partially_filled":
		return domain.BrokerOrderPartiallyFilled
	case "filled":
		return domain.BrokerOrderFilled
	case "canceled", "replaced":
		return domain.BrokerOrderCanceled
	case "expired", "done_for_day":
		return domain.BrokerOrderExpired
	case "rejected":
		return domain.BrokerOrderRejected
	default:
		return domain.BrokerOrderUnknown
	}
}

// ── Helpers ───────────────────────────────────────────────────────────────────

// do sends one API request and decodes a successful JSON body into dest.
//
// Reads and cancels are retried on network errors, 429 and 5xx. Order
// submission is sent once: a retry could place a second order, so the service
// instead resolves a failed submission by looking up its client order id.
func (b *Broker) do(ctx context.Context, method, endpoint string, body, dest any) error {
	var payload []byte
	if body != nil {
		var err error
		if payload, err = json.Marshal(body); err != nil {
			return fmt.Errorf("encode request: %w", err)
		}
	}
	attempts := maxAttempts
	if method == http.MethodPost {
		attempts = 1
	}

	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		if attempt > 1 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(attempt-1) * time.Second):
			}
		}

		req, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(payload))
		if err != nil {
			return err
		}
		req.Header.Set("APCA-API-KEY-ID", b.keyID)
		req.Header.Set("APCA-API-SECRET-KEY", b.secretKey)
		req.Header.Set("Accept", "application/json")
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}

		resp, err := b.httpClient.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		data, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			lastErr = err
			continue
		}

		switch {
		case resp.StatusCode == http.StatusNotFound && isAlpacaError(data):
			return errNotFound
		case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
			lastErr = fmt.Errorf("status %d: %s", resp.StatusCode, bytes.TrimSpace(data))
			continue
		case resp.StatusCode >= 300:
			return fmt.Errorf("status %d: %s", resp.StatusCode, bytes.TrimSpace(data))
		}
		if dest == nil || len(data) == 0 {
			return nil
		}
		return json.Unmarshal(data, dest)
	}
	return lastErr
}

// isAlpacaError reports whether body is Alpaca's JSON error envelope, so a 404
// from a wrong URL is not mistaken for "no position".
func isAlpacaError(body []byte) bool {
	var envelope struct {
		Code *int `json:"code"`
	}
	return json.Unmarshal(body, &envelope) == nil && envelope.Code != nil
}

func decimal(value string) (float64, error) {
	if value == "" {
		return 0, nil
	}
	return strconv.ParseFloat(value, 64)
}
