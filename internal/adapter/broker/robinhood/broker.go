package robinhood

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nareshpaturi/quant-stocks/internal/domain"
)

const DefaultMCPURL = "https://agent.robinhood.com/mcp/trading"

var ErrAmbiguousPlacement = errors.New("Robinhood order placement outcome is ambiguous")

type ExecutionMode string

const (
	ModeReadOnly ExecutionMode = "read-only"
	ModeShadow   ExecutionMode = "shadow"
	ModeReview   ExecutionMode = "review"
	ModeLive     ExecutionMode = "live"
)

type Config struct {
	Endpoint       string
	AccountID      string
	OAuthStateFile string
	Mode           ExecutionMode
	LiveTrading    bool
	SecretWriter   SecretWriter
}

// Broker implements the broker port with Robinhood's official Trading MCP.
type Broker struct {
	caller              toolCaller
	accountID           string
	implicitAccountSafe bool
	mode                ExecutionMode
	now                 func() time.Time
	mu                  sync.Mutex
	simulated           []domain.BrokerOrder
}

func New(ctx context.Context, cfg Config) (*Broker, error) {
	if cfg.Endpoint == "" {
		cfg.Endpoint = DefaultMCPURL
	}
	if err := validateMCPEndpoint(cfg.Endpoint); err != nil {
		return nil, err
	}
	if cfg.AccountID == "" || cfg.OAuthStateFile == "" {
		return nil, errors.New("Robinhood account id and OAuth state file are required")
	}
	if cfg.LiveTrading {
		cfg.Mode = ModeLive
	}
	if cfg.Mode == "" {
		cfg.Mode = ModeShadow
	}
	if cfg.Mode != ModeReadOnly && cfg.Mode != ModeShadow && cfg.Mode != ModeReview && cfg.Mode != ModeLive {
		return nil, fmt.Errorf("unsupported Robinhood execution mode %q", cfg.Mode)
	}
	if cfg.Mode == ModeLive && !cfg.LiveTrading {
		return nil, errors.New("Robinhood live mode requires LiveTrading=true")
	}
	if cfg.Mode == ModeLive && cfg.Endpoint != DefaultMCPURL {
		return nil, errors.New("Robinhood live mode requires the official MCP endpoint")
	}
	caller, err := newMCPCaller(ctx, mcpClientOptions{
		Endpoint: cfg.Endpoint, StateFile: cfg.OAuthStateFile, Writer: cfg.SecretWriter,
	})
	if err != nil {
		return nil, err
	}
	broker := &Broker{caller: caller, accountID: cfg.AccountID, mode: cfg.Mode, now: time.Now}
	if err := broker.validateAgenticAccount(ctx); err != nil {
		caller.Close()
		return nil, err
	}
	return broker, nil
}

func newWithCaller(caller toolCaller, accountID string, live bool) *Broker {
	mode := ModeShadow
	if live {
		mode = ModeLive
	}
	return &Broker{caller: caller, accountID: accountID, mode: mode, now: time.Now}
}

func (b *Broker) Close() error { return b.caller.Close() }

func (b *Broker) validateAgenticAccount(ctx context.Context) error {
	raw, err := b.caller.Call(ctx, "get_accounts", map[string]any{})
	if err != nil {
		return fmt.Errorf("Robinhood get_accounts: %w", err)
	}
	accounts, err := records(raw, "accounts")
	if err != nil {
		return fmt.Errorf("Robinhood accounts response: %w", err)
	}
	matches := make([]map[string]any, 0, 1)
	for _, account := range accounts {
		id := stringField(account, append(accountAliases, "id")...)
		if id == b.accountID {
			matches = append(matches, account)
		}
	}
	if len(matches) != 1 {
		return fmt.Errorf("configured Robinhood account %q matched %d accounts; refusing implicit fallback", b.accountID, len(matches))
	}
	if !isExplicitlyAgentic(matches[0]) {
		return fmt.Errorf("configured Robinhood account %q is not explicitly identified as Agentic", b.accountID)
	}
	agenticAccounts := 0
	for _, account := range accounts {
		if isExplicitlyAgentic(account) {
			agenticAccounts++
		}
	}
	b.implicitAccountSafe = agenticAccounts == 1
	if (!schemaHasAccountSelector(b.caller.InputSchema("review_equity_order")) ||
		!schemaHasAccountSelector(b.caller.InputSchema("place_equity_order"))) && !b.implicitAccountSafe {
		return errors.New("Robinhood order tools omit an account selector but OAuth exposes multiple Agentic accounts")
	}
	return nil
}

func isExplicitlyAgentic(account map[string]any) bool {
	agentic, known := boolField(account,
		"is_agentic", "isAgentic", "agentic", "agentic_enabled", "agenticEnabled",
		"agentic_allowed", "agenticAllowed", "is_agentic_account", "isAgenticAccount",
	)
	if known {
		return agentic
	}
	return explicitAgenticClassification(account)
}

func explicitAgenticClassification(account map[string]any) bool {
	for _, field := range []string{
		"type", "account_type", "accountType", "account_kind", "accountKind",
		"product", "capabilities", "features",
	} {
		if value, ok := lookup(account, field); ok && containsAgentic(value) {
			return true
		}
	}
	return false
}

func schemaHasAccountSelector(schema map[string]any) bool {
	properties, _ := schema["properties"].(map[string]any)
	if hasAnyProperty(properties, accountAliases) {
		return true
	}
	for _, property := range properties {
		if nested, ok := property.(map[string]any); ok && schemaHasAccountSelector(nested) {
			return true
		}
	}
	return false
}

func (b *Broker) GetAccount(ctx context.Context) (domain.AccountSnapshot, error) {
	args := newSchemaArgs(b.caller.InputSchema("get_portfolio"))
	accountSelector := args.setOptional(accountAliases, b.accountID)
	raw, err := b.caller.Call(ctx, "get_portfolio", args.values)
	if err != nil {
		return domain.AccountSnapshot{}, fmt.Errorf("Robinhood get_portfolio: %w", err)
	}
	records, err := records(raw, "portfolio", "account")
	if err != nil || len(records) == 0 {
		return domain.AccountSnapshot{}, fmt.Errorf("Robinhood portfolio response: no portfolio record")
	}
	matching := make([]map[string]any, 0, 1)
	for _, record := range records {
		accountID := stringField(record, accountAliases...)
		if accountID == b.accountID || (accountID == "" && accountSelector) {
			matching = append(matching, record)
		}
	}
	if len(matching) == 0 && !accountSelector && b.implicitAccountSafe && len(records) == 1 &&
		stringField(records[0], accountAliases...) == "" {
		matching = append(matching, records[0])
	}
	if len(matching) != 1 {
		return domain.AccountSnapshot{}, errors.New("Robinhood portfolio response did not identify exactly one configured Agentic account")
	}
	record := matching[0]
	buyingPower, ok := requiredFloatField(record, "buying_power", "buyingPower", "real_time_buying_power", "realTimeBuyingPower")
	if !ok {
		return domain.AccountSnapshot{}, errors.New("Robinhood portfolio response omitted buying power")
	}
	if buyingPower < 0 {
		return domain.AccountSnapshot{}, errors.New("Robinhood returned negative buying power")
	}
	limitedMargin, _ := boolField(record, "limited_margin", "limitedMargin", "is_limited_margin", "isLimitedMargin")
	return domain.AccountSnapshot{
		AccountID:   b.accountID,
		Cash:        floatField(record, "cash", "cash_available", "cashAvailable", "uninvested_cash"),
		BuyingPower: buyingPower, Agentic: true, LimitedMargin: limitedMargin,
	}, nil
}

func (b *Broker) GetPositions(ctx context.Context) ([]domain.Position, error) {
	args := newSchemaArgs(b.caller.InputSchema("get_equity_positions"))
	accountSelector := args.setOptional(accountAliases, b.accountID)
	raw, err := b.caller.Call(ctx, "get_equity_positions", args.values)
	if err != nil {
		return nil, fmt.Errorf("Robinhood get_equity_positions: %w", err)
	}
	items, err := records(raw, "positions", "equity_positions", "equityPositions")
	if err != nil {
		return nil, err
	}
	positions := make([]domain.Position, 0, len(items))
	for _, item := range items {
		ticker := strings.ToUpper(stringField(item, "symbol", "ticker"))
		quantity := floatField(item, "quantity", "shares", "qty")
		if ticker == "" || quantity <= 0 {
			continue
		}
		itemAccount := stringField(item, accountAliases...)
		if itemAccount != "" && itemAccount != b.accountID {
			continue
		}
		if !accountSelector && itemAccount == "" {
			return nil, errors.New("Robinhood position response omitted account identity for an account-wide query")
		}
		positions = append(positions, domain.Position{
			Ticker: ticker, Shares: quantity,
			CurrentPrice: floatField(item, "current_price", "currentPrice", "market_price", "marketPrice", "last_price", "lastPrice"),
		})
	}
	missing := make([]string, 0, len(positions))
	for _, position := range positions {
		if position.CurrentPrice <= 0 {
			missing = append(missing, position.Ticker)
		}
	}
	if len(missing) > 0 {
		quotes, err := b.GetQuotes(ctx, missing)
		if err != nil {
			return nil, fmt.Errorf("price Robinhood positions: %w", err)
		}
		for i := range positions {
			if positions[i].CurrentPrice <= 0 {
				positions[i].CurrentPrice = quotes[positions[i].Ticker]
			}
			if positions[i].CurrentPrice <= 0 {
				return nil, fmt.Errorf("Robinhood returned no current price for held symbol %s", positions[i].Ticker)
			}
		}
	}
	sort.Slice(positions, func(i, j int) bool { return positions[i].Ticker < positions[j].Ticker })
	return positions, nil
}

func (b *Broker) GetQuotes(ctx context.Context, tickers []string) (map[string]float64, error) {
	result := make(map[string]float64, len(tickers))
	unique := uniqueSymbols(tickers)
	for start := 0; start < len(unique); start += 20 {
		end := min(start+20, len(unique))
		chunk := unique[start:end]
		args := newSchemaArgs(b.caller.InputSchema("get_equity_quotes"))
		args.setOptional(accountAliases, b.accountID)
		if err := args.setRequired([]string{"symbols", "tickers", "symbol", "ticker"}, chunk); err != nil {
			return nil, err
		}
		raw, err := b.caller.Call(ctx, "get_equity_quotes", args.values)
		if err != nil {
			return nil, fmt.Errorf("Robinhood get_equity_quotes: %w", err)
		}
		items, err := records(raw, "quotes", "equity_quotes", "equityQuotes")
		if err != nil {
			return nil, err
		}
		for _, item := range items {
			ticker := strings.ToUpper(stringField(item, "symbol", "ticker"))
			price := floatField(item, "last_trade_price", "lastTradePrice", "last_price", "lastPrice", "price", "mark_price", "markPrice")
			if ticker != "" && price > 0 {
				result[ticker] = price
			}
		}
	}
	return result, nil
}

func (b *Broker) GetOrders(ctx context.Context, query domain.OrderQuery) ([]domain.BrokerOrder, error) {
	args := newSchemaArgs(b.caller.InputSchema("get_equity_orders"))
	accountSelector := args.setOptional(accountAliases, b.accountID)
	if !accountSelector && !b.implicitAccountSafe {
		return nil, errors.New("Robinhood order history omits an account selector with no unique Agentic account")
	}
	args.setOptional([]string{"limit", "page_size", "pageSize", "max_results", "maxResults"}, 100)
	if !query.From.IsZero() {
		if !args.setOptional([]string{"start_time", "startTime", "created_after", "createdAfter"}, query.From.Format(time.RFC3339)) {
			args.setOptional([]string{"start_date", "startDate"}, query.From.Format("2006-01-02"))
		}
	}
	if !query.To.IsZero() {
		if !args.setOptional([]string{"end_time", "endTime", "created_before", "createdBefore"}, query.To.Format(time.RFC3339)) {
			args.setOptional([]string{"end_date", "endDate"}, query.To.Format("2006-01-02"))
		}
	}
	raw, err := b.caller.Call(ctx, "get_equity_orders", args.values)
	if err != nil {
		return nil, fmt.Errorf("Robinhood get_equity_orders: %w", err)
	}
	items, err := records(raw, "orders", "equity_orders", "equityOrders")
	if err != nil {
		return nil, err
	}
	orders := make([]domain.BrokerOrder, 0, len(items))
	for _, item := range items {
		order := decodeBrokerOrder(item, b.accountID, query.CycleID)
		if order.ID == "" && order.Ticker == "" {
			continue
		}
		if order.ID == "" || order.Ticker == "" || (order.Side != domain.OrderSideBuy && order.Side != domain.OrderSideSell) {
			return nil, errors.New("Robinhood returned an incomplete equity order record")
		}
		if order.Active() && order.RemainingShares <= 0 {
			return nil, errors.New("Robinhood returned an active order without a remaining quantity")
		}
		if (!query.From.IsZero() || !query.To.IsZero()) && order.CreatedAt.IsZero() {
			return nil, errors.New("Robinhood returned an order without a cycle timestamp")
		}
		if order.AccountID != b.accountID {
			continue
		}
		if !query.From.IsZero() && !order.CreatedAt.IsZero() && order.CreatedAt.Before(query.From) {
			continue
		}
		if !query.To.IsZero() && !order.CreatedAt.IsZero() && !order.CreatedAt.Before(query.To) {
			continue
		}
		orders = append(orders, order)
	}
	b.mu.Lock()
	for _, order := range b.simulated {
		if query.CycleID == "" || order.CycleID == query.CycleID {
			orders = append(orders, order)
		}
	}
	b.mu.Unlock()
	return orders, nil
}

func (b *Broker) ReviewOrder(ctx context.Context, order domain.Order) (domain.OrderReview, error) {
	if order.Side != domain.OrderSideBuy && order.Side != domain.OrderSideSell {
		return domain.OrderReview{}, fmt.Errorf("unsupported Robinhood order side %q", order.Side)
	}
	if order.Type == "" {
		order.Type = domain.OrderTypeMarket
	}
	if order.Type != domain.OrderTypeMarket {
		return domain.OrderReview{}, fmt.Errorf("Robinhood supports only market orders in this strategy, got %q", order.Type)
	}
	if order.Duration == "" {
		order.Duration = domain.OrderDurationDay
	}
	if order.Duration != domain.OrderDurationDay {
		return domain.OrderReview{}, fmt.Errorf("Robinhood strategy orders must use day duration, got %q", order.Duration)
	}
	price := order.ReferencePrice
	if b.mode != ModeReadOnly {
		if err := b.checkTradability(ctx, order.Ticker); err != nil {
			return domain.OrderReview{}, err
		}
		quotes, err := b.GetQuotes(ctx, []string{order.Ticker})
		if err != nil {
			return domain.OrderReview{}, err
		}
		price = quotes[strings.ToUpper(order.Ticker)]
	}
	if price <= 0 {
		return domain.OrderReview{}, fmt.Errorf("Robinhood returned no quote for %s", order.Ticker)
	}
	if order.Side == domain.OrderSideBuy {
		order.Shares = math.Floor(order.Notional / price)
	} else if order.Shares != math.Floor(order.Shares) {
		return domain.OrderReview{}, fmt.Errorf("Robinhood strategy orders require whole shares, got %.8f", order.Shares)
	}
	if order.Shares < 1 {
		return domain.OrderReview{}, fmt.Errorf("order for %s buys or sells no whole shares", order.Ticker)
	}
	if b.mode == ModeReadOnly || b.mode == ModeShadow {
		estimated := order.Shares * price
		if err := b.ensureAffordable(ctx, order, estimated); err != nil {
			return domain.OrderReview{}, err
		}
		return domain.OrderReview{
			Order: order, EstimatedPrice: price, EstimatedNotional: estimated, Approved: true,
			BrokerData: reviewedPayload{fingerprint: orderFingerprint(order), reviewedAt: b.now()},
		}, nil
	}
	fields := canonicalOrderFields(order)
	args, err := orderArguments(b.caller.InputSchema("review_equity_order"), b.accountID, fields, "")
	if err != nil {
		return domain.OrderReview{}, fmt.Errorf("Robinhood review schema: %w", err)
	}
	raw, err := b.caller.Call(ctx, "review_equity_order", args)
	if err != nil {
		return domain.OrderReview{}, fmt.Errorf("Robinhood review_equity_order: %w", err)
	}
	items, err := records(raw, "review", "order_review", "orderReview")
	if err != nil || len(items) == 0 {
		return domain.OrderReview{}, errors.New("Robinhood review response was empty")
	}
	record := items[0]
	if approved, known := boolField(record, "approved", "can_place", "canPlace", "valid", "is_valid", "isValid"); known && !approved {
		return domain.OrderReview{}, fmt.Errorf("Robinhood rejected order review for %s", order.Ticker)
	}
	if err := validateReviewEcho(record, order, b.accountID); err != nil {
		return domain.OrderReview{}, err
	}
	estimated := floatField(record, "estimated_cost", "estimatedCost", "estimated_notional", "estimatedNotional", "notional")
	if estimated <= 0 {
		estimated = order.Shares * price
	}
	if err := b.ensureAffordable(ctx, order, estimated); err != nil {
		return domain.OrderReview{}, err
	}
	return domain.OrderReview{
		Order: order, ReviewID: stringField(record, "review_id", "reviewId", "order_review_id", "orderReviewId"),
		EstimatedPrice: price, EstimatedNotional: estimated,
		Warnings: stringSliceField(record, "warnings", "alerts", "messages"), Approved: true,
		BrokerData: reviewedPayload{fingerprint: orderFingerprint(order), reviewedAt: b.now()},
	}, nil
}

func (b *Broker) PlaceOrder(ctx context.Context, review domain.OrderReview) (domain.BrokerOrder, error) {
	payload, ok := review.BrokerData.(reviewedPayload)
	if !review.Approved || !ok || payload.fingerprint != orderFingerprint(review.Order) {
		return domain.BrokerOrder{}, errors.New("Robinhood placement requires the unchanged result of ReviewOrder")
	}
	if b.mode != ModeLive {
		order := brokerOrderFromReview(strings.ToUpper(string(b.mode))+"-"+review.Order.ClientOrderID, b.accountID, review, domain.BrokerOrderPending)
		b.mu.Lock()
		b.simulated = append(b.simulated, order)
		b.mu.Unlock()
		return order, nil
	}
	args, err := orderArguments(b.caller.InputSchema("place_equity_order"), b.accountID, canonicalOrderFields(review.Order), review.ReviewID)
	if err != nil {
		return domain.BrokerOrder{}, fmt.Errorf("Robinhood place schema: %w", err)
	}
	raw, callErr := b.caller.Call(ctx, "place_equity_order", args)
	if callErr != nil {
		return b.resolveAmbiguousPlacement(ctx, review, payload.reviewedAt, callErr)
	}
	items, err := records(raw, "order", "orders", "equity_order", "equityOrder")
	if err != nil || len(items) == 0 {
		return b.resolveAmbiguousPlacement(ctx, review, payload.reviewedAt, errors.New("placement response was empty"))
	}
	order := decodeBrokerOrder(items[0], b.accountID, review.Order.CycleID)
	if order.ID == "" {
		return b.resolveAmbiguousPlacement(ctx, review, payload.reviewedAt, errors.New("placement response did not contain an order id"))
	}
	applyReviewDefaults(&order, review, b.accountID)
	if order.Status == domain.BrokerOrderUnknown {
		return domain.BrokerOrder{}, fmt.Errorf("Robinhood placed order %s has unknown status", order.ID)
	}
	return order, nil
}

func (b *Broker) IsMarketOpen(context.Context) (bool, error) {
	// Robinhood market+day orders may queue for the next regular session. This
	// method exists for Tradier backtests, which reject Robinhood configuration.
	return true, nil
}

func (b *Broker) checkTradability(ctx context.Context, ticker string) error {
	args := newSchemaArgs(b.caller.InputSchema("get_equity_tradability"))
	args.setOptional(accountAliases, b.accountID)
	if err := args.setRequired([]string{"symbol", "ticker"}, strings.ToUpper(ticker)); err != nil {
		return err
	}
	raw, err := b.caller.Call(ctx, "get_equity_tradability", args.values)
	if err != nil {
		return fmt.Errorf("Robinhood get_equity_tradability: %w", err)
	}
	items, err := records(raw, "tradability", "result")
	if err != nil || len(items) == 0 {
		return errors.New("Robinhood tradability response was empty")
	}
	record := items[0]
	if tradable, known := boolField(record, "tradable", "is_tradable", "isTradable", "can_trade", "canTrade"); known {
		if !tradable {
			return fmt.Errorf("Robinhood reports %s is not tradable", ticker)
		}
		return nil
	}
	state := strings.ToLower(stringField(record, "state", "status", "tradability"))
	if state == "tradable" || state == "active" {
		return nil
	}
	return fmt.Errorf("Robinhood did not explicitly confirm %s is tradable", ticker)
}

type reviewedPayload struct {
	fingerprint string
	reviewedAt  time.Time
}

func canonicalOrderFields(order domain.Order) map[string]any {
	return map[string]any{
		"symbol": strings.ToUpper(order.Ticker), "side": string(order.Side),
		"quantity": order.Shares, "order_type": string(order.Type),
		"duration": string(order.Duration), "client_order_id": order.ClientOrderID,
	}
}

func orderFingerprint(order domain.Order) string {
	return fmt.Sprintf("%s|%s|%s|%s|%.8f|%s|%s", strings.ToUpper(order.Ticker), order.Side,
		order.Type, order.Duration, order.Shares, order.CycleID, order.ClientOrderID)
}

func validateReviewEcho(record map[string]any, order domain.Order, accountID string) error {
	if account := stringField(record, accountAliases...); account != "" && account != accountID {
		return fmt.Errorf("Robinhood review changed account from %s to %s", accountID, account)
	}
	if symbol := strings.ToUpper(stringField(record, "symbol", "ticker")); symbol != "" && symbol != strings.ToUpper(order.Ticker) {
		return fmt.Errorf("Robinhood review changed symbol from %s to %s", order.Ticker, symbol)
	}
	if side := strings.ToLower(stringField(record, "side")); side != "" && side != string(order.Side) {
		return fmt.Errorf("Robinhood review changed side from %s to %s", order.Side, side)
	}
	if quantity := floatField(record, "quantity", "qty", "shares"); quantity > 0 && math.Abs(quantity-order.Shares) > 1e-9 {
		return fmt.Errorf("Robinhood review changed quantity from %.8f to %.8f", order.Shares, quantity)
	}
	orderType := strings.ToLower(stringField(record, "order_type", "orderType"))
	if orderType == "" {
		candidate := strings.ToLower(stringField(record, "type"))
		switch candidate {
		case "market", "limit", "stop", "stop_market", "stop_limit":
			orderType = candidate
		}
	}
	if orderType != "" && orderType != string(order.Type) {
		return fmt.Errorf("Robinhood review changed order type from %s to %s", order.Type, orderType)
	}
	if duration := normalizeDuration(stringField(record, "time_in_force", "timeInForce", "duration")); duration != "" && duration != order.Duration {
		return fmt.Errorf("Robinhood review changed duration from %s to %s", order.Duration, duration)
	}
	if clientID := stringField(record, "client_order_id", "clientOrderId", "client_order_identifier"); clientID != "" && clientID != order.ClientOrderID {
		return errors.New("Robinhood review changed the client order id")
	}
	return nil
}

func (b *Broker) ensureAffordable(ctx context.Context, order domain.Order, estimatedNotional float64) error {
	if order.Side != domain.OrderSideBuy || b.mode == ModeReadOnly {
		return nil
	}
	account, err := b.GetAccount(ctx)
	if err != nil {
		return fmt.Errorf("refresh Robinhood buying power: %w", err)
	}
	if estimatedNotional-account.BuyingPower > 0.01 {
		return fmt.Errorf("Robinhood estimated notional %.2f exceeds buying power %.2f", estimatedNotional, account.BuyingPower)
	}
	return nil
}

func (b *Broker) resolveAmbiguousPlacement(ctx context.Context, review domain.OrderReview, since time.Time, cause error) (domain.BrokerOrder, error) {
	orders, err := b.GetOrders(ctx, domain.OrderQuery{
		AccountID: b.accountID, CycleID: review.Order.CycleID,
		From: since.Add(-time.Minute), To: b.now().Add(time.Minute),
	})
	if err != nil {
		return domain.BrokerOrder{}, fmt.Errorf("%w: %v; reconciliation failed: %v", ErrAmbiguousPlacement, cause, err)
	}
	matches := make([]domain.BrokerOrder, 0, 1)
	for _, order := range orders {
		if review.Order.ClientOrderID != "" && order.ClientOrderID == review.Order.ClientOrderID {
			matches = append(matches, order)
			continue
		}
		if order.Ticker == review.Order.Ticker && order.Side == review.Order.Side &&
			math.Abs(order.RequestedShares-review.Order.Shares) < 1e-9 && !order.CreatedAt.Before(since.Add(-time.Minute)) {
			matches = append(matches, order)
		}
	}
	if len(matches) == 1 {
		if matches[0].Status == domain.BrokerOrderUnknown {
			return domain.BrokerOrder{}, fmt.Errorf("%w: matching order %s has unknown status", ErrAmbiguousPlacement, matches[0].ID)
		}
		return matches[0], nil
	}
	return domain.BrokerOrder{}, fmt.Errorf("%w: %v; found %d matching orders", ErrAmbiguousPlacement, cause, len(matches))
}

func decodeBrokerOrder(record map[string]any, accountID, cycleID string) domain.BrokerOrder {
	status := normalizeStatus(stringField(record, "status", "state", "order_state", "orderState"))
	requested := floatField(record, "quantity", "requested_quantity", "requestedQuantity", "shares", "qty")
	filled := floatField(record, "filled_quantity", "filledQuantity", "executed_quantity", "executedQuantity", "cumulative_quantity", "cumulativeQuantity")
	if status == domain.BrokerOrderFilled && filled <= 0 {
		filled = requested
	}
	remaining := floatField(record, "remaining_quantity", "remainingQuantity", "leaves_quantity", "leavesQuantity")
	if remaining <= 0 && statusActive(status) {
		remaining = math.Max(0, requested-filled)
	}
	side := domain.OrderSide(strings.ToLower(stringField(record, "side")))
	return domain.BrokerOrder{
		ID:            stringField(record, "order_id", "orderId", "id"),
		ClientOrderID: stringField(record, "client_order_id", "clientOrderId", "client_order_identifier"),
		AccountID:     firstNonEmpty(stringField(record, accountAliases...), accountID),
		Ticker:        strings.ToUpper(stringField(record, "symbol", "ticker")), Side: side,
		Type:            domain.OrderType(strings.ToLower(stringField(record, "order_type", "orderType", "type"))),
		Duration:        normalizeDuration(stringField(record, "time_in_force", "timeInForce", "duration")),
		RequestedShares: requested, FilledShares: filled, RemainingShares: remaining,
		AverageFillPrice:  floatField(record, "average_fill_price", "averageFillPrice", "avg_fill_price", "avgFillPrice", "executed_price"),
		EstimatedPrice:    floatField(record, "estimated_price", "estimatedPrice", "price"),
		EstimatedNotional: floatField(record, "estimated_notional", "estimatedNotional", "estimated_cost", "estimatedCost"),
		Status:            status,
		CreatedAt: timeField(record,
			"created_at", "createdAt", "submitted_at", "submittedAt",
			"created_time", "createdTime", "created_date", "createdDate", "submitted_time", "submittedTime",
		),
		UpdatedAt: timeField(record, "updated_at", "updatedAt", "last_transaction_at", "lastTransactionAt"),
		CycleID:   cycleID,
	}
}

func normalizeStatus(value string) domain.BrokerOrderStatus {
	value = strings.ToLower(strings.TrimSpace(value))
	switch value {
	case "pending", "confirmed", "unconfirmed", "new", "placed", "submitted":
		return domain.BrokerOrderPending
	case "queued":
		return domain.BrokerOrderQueued
	case "open", "working":
		return domain.BrokerOrderOpen
	case "partially_filled", "partially filled", "partial_fill":
		return domain.BrokerOrderPartiallyFilled
	case "filled", "completed":
		return domain.BrokerOrderFilled
	case "canceled", "cancelled", "voided":
		return domain.BrokerOrderCanceled
	case "rejected", "failed":
		return domain.BrokerOrderRejected
	case "expired":
		return domain.BrokerOrderExpired
	default:
		return domain.BrokerOrderUnknown
	}
}

func brokerOrderFromReview(id, accountID string, review domain.OrderReview, status domain.BrokerOrderStatus) domain.BrokerOrder {
	return domain.BrokerOrder{
		ID: id, ClientOrderID: review.Order.ClientOrderID, AccountID: accountID,
		Ticker: review.Order.Ticker, Side: review.Order.Side, Type: review.Order.Type, Duration: review.Order.Duration,
		RequestedShares: review.Order.Shares, RemainingShares: review.Order.Shares,
		EstimatedPrice: review.EstimatedPrice, EstimatedNotional: review.EstimatedNotional,
		Status: status, CycleID: review.Order.CycleID, CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
}

func applyReviewDefaults(order *domain.BrokerOrder, review domain.OrderReview, accountID string) {
	if order.ClientOrderID == "" {
		order.ClientOrderID = review.Order.ClientOrderID
	}
	if order.AccountID == "" {
		order.AccountID = accountID
	}
	if order.Ticker == "" {
		order.Ticker = review.Order.Ticker
	}
	if order.Side == "" {
		order.Side = review.Order.Side
	}
	if order.Type == "" {
		order.Type = review.Order.Type
	}
	if order.Duration == "" {
		order.Duration = review.Order.Duration
	}
	if order.RequestedShares <= 0 {
		order.RequestedShares = review.Order.Shares
	}
	if order.EstimatedPrice <= 0 {
		order.EstimatedPrice = review.EstimatedPrice
	}
	if order.EstimatedNotional <= 0 {
		order.EstimatedNotional = review.EstimatedNotional
	}
	if order.CycleID == "" {
		order.CycleID = review.Order.CycleID
	}
}

func statusActive(status domain.BrokerOrderStatus) bool {
	return (domain.BrokerOrder{Status: status}).Active()
}

func normalizeDuration(value string) domain.OrderDuration {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "day", "gfd", "good_for_day":
		return domain.OrderDurationDay
	case "gtc", "good_til_canceled", "good_till_canceled":
		return domain.OrderDurationGTC
	default:
		return domain.OrderDuration(strings.ToLower(strings.TrimSpace(value)))
	}
}

func uniqueSymbols(values []string) []string {
	seen := make(map[string]bool, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.ToUpper(strings.TrimSpace(value))
		if value != "" && !seen[value] {
			seen[value] = true
			result = append(result, value)
		}
	}
	return result
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func requiredFloatField(record map[string]any, aliases ...string) (float64, bool) {
	value, ok := lookup(record, aliases...)
	if !ok {
		return 0, false
	}
	if parsed, ok := asFloat(value); ok {
		return parsed, true
	}
	if nested, ok := value.(map[string]any); ok {
		for _, alias := range append(append([]string(nil), aliases...), "amount", "value", "available") {
			if candidate, exists := nested[alias]; exists {
				if parsed, ok := asFloat(candidate); ok {
					return parsed, true
				}
			}
		}
	}
	return 0, false
}
