package robinhood

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/nareshpaturi/quant-stocks/internal/domain"
)

type fakeCall struct {
	name string
	args map[string]any
}

type fakeCaller struct {
	schemas map[string]map[string]any
	handler func(string, map[string]any) (json.RawMessage, error)
	calls   []fakeCall
}

func (f *fakeCaller) Call(_ context.Context, name string, args map[string]any) (json.RawMessage, error) {
	copyArgs := make(map[string]any, len(args))
	for key, value := range args {
		copyArgs[key] = value
	}
	f.calls = append(f.calls, fakeCall{name: name, args: copyArgs})
	return f.handler(name, args)
}
func (f *fakeCaller) InputSchema(name string) map[string]any { return f.schemas[name] }
func (f *fakeCaller) Close() error                           { return nil }

func testSchemas() map[string]map[string]any {
	schema := func(properties map[string]any) map[string]any {
		return map[string]any{"type": "object", "properties": properties}
	}
	stringProperty := map[string]any{"type": "string"}
	numberProperty := map[string]any{"type": "number"}
	account := map[string]any{"account_id": stringProperty}
	order := func(includeReview bool) map[string]any {
		props := map[string]any{
			"account_id": stringProperty, "symbol": stringProperty, "side": stringProperty,
			"quantity": numberProperty, "dollar_amount": stringProperty,
			"order_type": stringProperty, "time_in_force": stringProperty,
			"market_hours": stringProperty, "client_order_id": stringProperty,
		}
		if includeReview {
			props["review_id"] = stringProperty
		}
		return schema(props)
	}
	return map[string]map[string]any{
		"get_accounts":         schema(nil),
		"get_portfolio":        schema(account),
		"get_equity_positions": schema(account),
		"get_equity_quotes":    schema(map[string]any{"symbols": map[string]any{"type": "array"}}),
		"get_equity_orders":    schema(account),
		"get_equity_tradability": schema(map[string]any{
			"account_number": stringProperty,
			"symbols":        map[string]any{"type": "array"},
		}),
		"review_equity_order": order(false),
		"place_equity_order":  order(true),
	}
}

func rawJSON(value string) json.RawMessage { return json.RawMessage(value) }

func TestValidateAgenticAccountFailsClosed(t *testing.T) {
	caller := &fakeCaller{schemas: testSchemas()}
	caller.handler = func(name string, _ map[string]any) (json.RawMessage, error) {
		if name != "get_accounts" {
			t.Fatalf("unexpected call %s", name)
		}
		return rawJSON(`{"accounts":[{"account_id":"primary","type":"individual"},{"account_id":"agent-1","type":"agentic"}]}`), nil
	}
	broker := newWithCaller(caller, "agent-1", false)
	if err := broker.validateAgenticAccount(context.Background()); err != nil {
		t.Fatalf("agentic account rejected: %v", err)
	}
	broker.accountID = "primary"
	if err := broker.validateAgenticAccount(context.Background()); err == nil {
		t.Fatal("primary account was accepted")
	}
}

func TestValidateAgenticAccountDoesNotTrustAccountIDLabel(t *testing.T) {
	caller := &fakeCaller{schemas: testSchemas()}
	caller.handler = func(name string, _ map[string]any) (json.RawMessage, error) {
		if name != "get_accounts" {
			t.Fatalf("unexpected call %s", name)
		}
		return rawJSON(`{"accounts":[{"account_id":"agentic-looking-id","type":"individual"}]}`), nil
	}
	broker := newWithCaller(caller, "agentic-looking-id", false)
	if err := broker.validateAgenticAccount(context.Background()); err == nil {
		t.Fatal("individual account was accepted because its id contained agentic")
	}
}

func TestValidateAgenticAccountRejectsImplicitRoutingWithMultipleAgenticAccounts(t *testing.T) {
	schemas := testSchemas()
	for _, name := range []string{"review_equity_order", "place_equity_order"} {
		delete(schemas[name]["properties"].(map[string]any), "account_id")
	}
	caller := &fakeCaller{schemas: schemas}
	caller.handler = func(name string, _ map[string]any) (json.RawMessage, error) {
		if name != "get_accounts" {
			t.Fatalf("unexpected call %s", name)
		}
		return rawJSON(`{"accounts":[{"account_id":"agent-1","agentic_allowed":true},{"account_id":"agent-2","agentic_allowed":true}]}`), nil
	}
	broker := newWithCaller(caller, "agent-1", false)
	if err := broker.validateAgenticAccount(context.Background()); err == nil {
		t.Fatal("implicit order routing was accepted with multiple Agentic accounts")
	}
}

func TestGetQuotesChunksAtTwentySymbols(t *testing.T) {
	caller := &fakeCaller{schemas: testSchemas()}
	caller.handler = func(name string, args map[string]any) (json.RawMessage, error) {
		if name != "get_equity_quotes" {
			t.Fatalf("unexpected call %s", name)
		}
		symbols := args["symbols"].([]string)
		quotes := make([]map[string]any, 0, len(symbols))
		for _, symbol := range symbols {
			quotes = append(quotes, map[string]any{"symbol": symbol, "last_trade_price": 100})
		}
		data, _ := json.Marshal(map[string]any{"quotes": quotes})
		return data, nil
	}
	broker := newWithCaller(caller, "agent-1", false)
	var tickers []string
	for i := 0; i < 45; i++ {
		tickers = append(tickers, fmt.Sprintf("T%02d", i))
	}
	quotes, err := broker.GetQuotes(context.Background(), tickers)
	if err != nil {
		t.Fatal(err)
	}
	if len(quotes) != 45 || len(caller.calls) != 3 {
		t.Fatalf("got %d quotes in %d calls", len(quotes), len(caller.calls))
	}
}

func TestGetPositionsFiltersAccountWideResponse(t *testing.T) {
	schemas := testSchemas()
	schemas["get_equity_positions"] = map[string]any{"type": "object", "properties": map[string]any{}}
	caller := &fakeCaller{schemas: schemas}
	caller.handler = func(name string, _ map[string]any) (json.RawMessage, error) {
		if name != "get_equity_positions" {
			t.Fatalf("unexpected call %s", name)
		}
		return rawJSON(`{"positions":[{"account_id":"primary","symbol":"AAPL","quantity":3,"current_price":200},{"account_id":"agent-1","symbol":"MSFT","quantity":2,"current_price":400}]}`), nil
	}
	broker := newWithCaller(caller, "agent-1", false)
	broker.implicitAccountSafe = true
	positions, err := broker.GetPositions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(positions) != 1 || positions[0].Ticker != "MSFT" {
		t.Fatalf("unexpected positions: %+v", positions)
	}
}

func TestGetAccountReadsNestedBuyingPower(t *testing.T) {
	caller := &fakeCaller{schemas: testSchemas()}
	caller.handler = func(name string, _ map[string]any) (json.RawMessage, error) {
		if name != "get_portfolio" {
			t.Fatalf("unexpected call %s", name)
		}
		return rawJSON(`{"portfolio":{"cash":"1000","buying_power":{"buying_power":"3000","unleveraged_buying_power":"2500"}}}`), nil
	}
	broker := newWithCaller(caller, "agent-1", false)
	account, err := broker.GetAccount(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if account.BuyingPower != 3000 || account.Cash != 1000 {
		t.Fatalf("unexpected account: %+v", account)
	}
}

func TestReviewAndPlaceUseExactSameOrder(t *testing.T) {
	caller := &fakeCaller{schemas: testSchemas()}
	caller.handler = func(name string, args map[string]any) (json.RawMessage, error) {
		switch name {
		case "get_portfolio":
			return rawJSON(`{"portfolio":{"buying_power":10000}}`), nil
		case "get_equity_tradability":
			return rawJSON(`{"tradable":true,"fractional_tradability":"tradable"}`), nil
		case "get_equity_quotes":
			return rawJSON(`{"quotes":[{"symbol":"AAPL","last_trade_price":"190"}]}`), nil
		case "review_equity_order":
			return rawJSON(`{"review":{"review_id":"review-1","approved":true,"symbol":"AAPL","side":"buy","dollar_amount":1000,"quantity":5,"estimated_cost":950}}`), nil
		case "place_equity_order":
			return rawJSON(`{"order":{"order_id":"order-1","status":"queued","symbol":"AAPL","side":"buy","quantity":5}}`), nil
		default:
			return nil, fmt.Errorf("unexpected call %s", name)
		}
	}
	broker := newWithCaller(caller, "agent-1", true)
	review, err := broker.ReviewOrder(context.Background(), domain.Order{
		Ticker: "AAPL", Side: domain.OrderSideBuy, Type: domain.OrderTypeMarket,
		Notional: 1000, CycleID: "cycle", ClientOrderID: "client-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	tampered := review
	tampered.Order.Notional++
	if _, err := broker.PlaceOrder(context.Background(), tampered); err == nil {
		t.Fatal("placement accepted a changed dollar amount")
	}
	placed, err := broker.PlaceOrder(context.Background(), review)
	if err != nil {
		t.Fatal(err)
	}
	if placed.ID != "order-1" || placed.Status != domain.BrokerOrderQueued {
		t.Fatalf("unexpected placed order: %+v", placed)
	}
	var reviewArgs, placeArgs map[string]any
	for _, call := range caller.calls {
		if call.name == "review_equity_order" {
			reviewArgs = call.args
		}
		if call.name == "place_equity_order" {
			placeArgs = call.args
		}
	}
	for _, key := range []string{"account_id", "symbol", "side", "dollar_amount", "order_type", "time_in_force", "market_hours", "client_order_id"} {
		if !reflect.DeepEqual(reviewArgs[key], placeArgs[key]) {
			t.Fatalf("%s differs: review=%v place=%v", key, reviewArgs[key], placeArgs[key])
		}
	}
	if _, exists := reviewArgs["quantity"]; exists {
		t.Fatalf("dollar-based review unexpectedly contained quantity: %v", reviewArgs)
	}
	if placeArgs["review_id"] != "review-1" {
		t.Fatalf("review id not passed: %v", placeArgs)
	}
}

func TestShadowModeNeverCallsReviewOrPlaceTools(t *testing.T) {
	caller := &fakeCaller{schemas: testSchemas()}
	caller.handler = func(name string, args map[string]any) (json.RawMessage, error) {
		switch name {
		case "get_portfolio":
			return rawJSON(`{"portfolio":{"buying_power":10000}}`), nil
		case "get_equity_tradability":
			if args["account_number"] != "agent-1" || !reflect.DeepEqual(args["symbols"], []string{"AAPL"}) {
				t.Fatalf("unexpected tradability arguments: %#v", args)
			}
			return rawJSON(`{"data":{"results":[{"symbol":"AAPL","state":"active","tradeable":true,"fractional_tradability":"tradable","all_day_tradability":"tradable","account_type_tradabilities":[{"account_type":"individual","account_type_tradability":"tradable"}]}]}}`), nil
		case "get_equity_quotes":
			return rawJSON(`{"data":{"results":[{"quote":{"symbol":"AAPL","last_trade_price":1800}}]}}`), nil
		default:
			return nil, fmt.Errorf("unsafe tool called in shadow mode: %s", name)
		}
	}
	broker := newWithCaller(caller, "agent-1", false)
	review, err := broker.ReviewOrder(context.Background(), domain.Order{
		Ticker: "AAPL", Side: domain.OrderSideBuy, Type: domain.OrderTypeMarket,
		Notional: 1000, ClientOrderID: "client-1", ReferencePrice: 195,
	})
	if err != nil {
		t.Fatal(err)
	}
	if review.Order.Shares != 0.555555 || review.EstimatedNotional != 1000 {
		t.Fatalf("unexpected fractional review: %+v", review)
	}
	placed, err := broker.PlaceOrder(context.Background(), review)
	if err != nil {
		t.Fatal(err)
	}
	if placed.ID != "SHADOW-client-1" {
		t.Fatalf("unexpected shadow order: %+v", placed)
	}
	for _, call := range caller.calls {
		if call.name == "review_equity_order" || call.name == "place_equity_order" {
			t.Fatalf("unsafe call in shadow mode: %s", call.name)
		}
	}
}

func TestReviewFallsBackToFractionalQuantity(t *testing.T) {
	schemas := testSchemas()
	for _, tool := range []string{"review_equity_order", "place_equity_order"} {
		delete(schemas[tool]["properties"].(map[string]any), "dollar_amount")
	}
	caller := &fakeCaller{schemas: schemas}
	caller.handler = func(name string, args map[string]any) (json.RawMessage, error) {
		switch name {
		case "get_equity_tradability":
			return rawJSON(`{"data":{"results":[{"symbol":"SNDK","state":"active","tradeable":true,"fractional_tradability":"tradable","account_type_tradabilities":[{"account_type":"individual","account_type_tradability":"tradable"}]}]}}`), nil
		case "get_equity_quotes":
			return rawJSON(`{"data":{"results":[{"quote":{"symbol":"SNDK","last_trade_price":1800}}]}}`), nil
		case "review_equity_order":
			if got := args["quantity"]; got != 0.555555 {
				t.Fatalf("quantity = %#v, want 0.555555", got)
			}
			if _, exists := args["dollar_amount"]; exists {
				t.Fatalf("unsupported dollar_amount was sent: %#v", args)
			}
			return rawJSON(`{"review":{"approved":true,"symbol":"SNDK","side":"buy","quantity":0.555555,"estimated_cost":999.999}}`), nil
		case "get_portfolio":
			return rawJSON(`{"portfolio":{"buying_power":10000}}`), nil
		default:
			return nil, fmt.Errorf("unexpected call %s", name)
		}
	}
	broker := newWithCaller(caller, "agent-1", false)
	broker.mode = ModeReview
	review, err := broker.ReviewOrder(context.Background(), domain.Order{
		Ticker: "SNDK", Side: domain.OrderSideBuy, Type: domain.OrderTypeMarket,
		Notional: 1000, ClientOrderID: "client-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if review.Order.Shares != 0.555555 {
		t.Fatalf("shares = %.8f, want 0.555555", review.Order.Shares)
	}
}

func TestReviewRejectsUnsupportedFractionalBuy(t *testing.T) {
	caller := &fakeCaller{schemas: testSchemas()}
	caller.handler = func(name string, _ map[string]any) (json.RawMessage, error) {
		switch name {
		case "get_equity_tradability":
			return rawJSON(`{"tradable":true,"fractional_tradability":"untradable"}`), nil
		case "get_equity_quotes":
			return rawJSON(`{"quotes":[{"symbol":"OTC","last_trade_price":1800}]}`), nil
		default:
			return nil, fmt.Errorf("unexpected call %s", name)
		}
	}
	broker := newWithCaller(caller, "agent-1", false)
	_, err := broker.ReviewOrder(context.Background(), domain.Order{
		Ticker: "OTC", Side: domain.OrderSideBuy, Type: domain.OrderTypeMarket, Notional: 1000,
	})
	if err == nil {
		t.Fatal("fractional buy was accepted without broker eligibility")
	}
}

func TestShadowReviewAllowsFractionalPositionClosingSell(t *testing.T) {
	caller := &fakeCaller{schemas: testSchemas()}
	caller.handler = func(name string, _ map[string]any) (json.RawMessage, error) {
		switch name {
		case "get_equity_tradability":
			return rawJSON(`{"tradable":true,"fractional_tradability":"position_closing_only"}`), nil
		case "get_equity_quotes":
			return rawJSON(`{"quotes":[{"symbol":"AAPL","last_trade_price":1800}]}`), nil
		default:
			return nil, fmt.Errorf("unexpected call %s", name)
		}
	}
	broker := newWithCaller(caller, "agent-1", false)
	review, err := broker.ReviewOrder(context.Background(), domain.Order{
		Ticker: "AAPL", Side: domain.OrderSideSell, Type: domain.OrderTypeMarket, Shares: 0.555555,
	})
	if err != nil {
		t.Fatal(err)
	}
	if review.Order.Shares != 0.555555 {
		t.Fatalf("shares = %.8f, want 0.555555", review.Order.Shares)
	}
}

func TestDecodeBrokerOrderReadsNestedDollarAmount(t *testing.T) {
	items, err := records(rawJSON(`{"order":{"order_id":"order-1","symbol":"AAPL","side":"buy","state":"queued","quantity":"0.5","dollar_based_amount":{"amount":"1000","currency_code":"USD"}}}`), "order")
	if err != nil || len(items) != 1 {
		t.Fatalf("decode records: items=%v err=%v", items, err)
	}
	order := decodeBrokerOrder(items[0], "agent-1", "cycle")
	if order.EstimatedNotional != 1000 {
		t.Fatalf("estimated notional = %.2f, want 1000", order.EstimatedNotional)
	}
}

func TestTradabilityRejectsAccountTypeRestriction(t *testing.T) {
	caller := &fakeCaller{schemas: testSchemas()}
	caller.handler = func(name string, _ map[string]any) (json.RawMessage, error) {
		if name != "get_equity_tradability" {
			return nil, fmt.Errorf("unexpected call %s", name)
		}
		return rawJSON(`{"data":{"results":[{"symbol":"AAPL","state":"active","tradeable":true,"account_type_tradabilities":[{"account_type":"individual","account_type_tradability":"untradable"}]}]}}`), nil
	}
	broker := newWithCaller(caller, "agent-1", false)
	err := broker.checkTradability(context.Background(), "AAPL")
	if err == nil || err.Error() != "Robinhood reports AAPL is not tradable for this account type" {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestTradabilityFailsClosedForUnknownResponse(t *testing.T) {
	caller := &fakeCaller{schemas: testSchemas()}
	caller.handler = func(name string, _ map[string]any) (json.RawMessage, error) {
		if name != "get_equity_tradability" {
			return nil, fmt.Errorf("unexpected call %s", name)
		}
		return rawJSON(`{"data":{"results":[{"symbol":"AAPL","fractional_tradability":"tradable"}]}}`), nil
	}
	broker := newWithCaller(caller, "agent-1", false)
	err := broker.checkTradability(context.Background(), "AAPL")
	if err == nil {
		t.Fatal("ambiguous tradability response was accepted")
	}
}

func TestReviewFailsWhenBrokerEstimateExceedsFreshBuyingPower(t *testing.T) {
	caller := &fakeCaller{schemas: testSchemas()}
	caller.handler = func(name string, _ map[string]any) (json.RawMessage, error) {
		switch name {
		case "get_equity_tradability":
			return rawJSON(`{"tradable":true,"fractional_tradability":"tradable"}`), nil
		case "get_equity_quotes":
			return rawJSON(`{"quotes":[{"symbol":"AAPL","last_trade_price":200}]}`), nil
		case "review_equity_order":
			return rawJSON(`{"review":{"approved":true,"symbol":"AAPL","side":"buy","dollar_amount":1000,"quantity":5,"estimated_cost":1050}}`), nil
		case "get_portfolio":
			return rawJSON(`{"portfolio":{"buying_power":1000}}`), nil
		default:
			return nil, fmt.Errorf("unexpected call %s", name)
		}
	}
	broker := newWithCaller(caller, "agent-1", false)
	broker.mode = ModeReview
	_, err := broker.ReviewOrder(context.Background(), domain.Order{
		Ticker: "AAPL", Side: domain.OrderSideBuy, Type: domain.OrderTypeMarket,
		Notional: 1000, ClientOrderID: "client-1", ReferencePrice: 200,
	})
	if err == nil {
		t.Fatal("review accepted an estimate above fresh buying power")
	}
}

func TestPlacementErrorAdoptsDiscoverableOrder(t *testing.T) {
	caller := &fakeCaller{schemas: testSchemas()}
	caller.handler = func(name string, _ map[string]any) (json.RawMessage, error) {
		switch name {
		case "place_equity_order":
			return nil, errors.New("connection reset")
		case "get_equity_orders":
			return rawJSON(`{"orders":[{"order_id":"order-1","client_order_id":"client-1","status":"queued","symbol":"AAPL","side":"buy","quantity":5,"created_at":"2026-09-08T16:00:00Z"}]}`), nil
		default:
			return nil, fmt.Errorf("unexpected call %s", name)
		}
	}
	broker := newWithCaller(caller, "agent-1", true)
	broker.now = func() time.Time { return time.Date(2026, 9, 8, 16, 0, 0, 0, time.UTC) }
	review := domain.OrderReview{
		Order: domain.Order{Ticker: "AAPL", Side: domain.OrderSideBuy, Type: domain.OrderTypeMarket,
			Duration: domain.OrderDurationDay, Shares: 5, CycleID: "cycle", ClientOrderID: "client-1"},
		Approved: true,
		BrokerData: reviewedPayload{fingerprint: orderFingerprint(domain.Order{Ticker: "AAPL", Side: domain.OrderSideBuy, Type: domain.OrderTypeMarket,
			Duration: domain.OrderDurationDay, Shares: 5, CycleID: "cycle", ClientOrderID: "client-1"}), reviewedAt: broker.now()},
	}
	placed, err := broker.PlaceOrder(context.Background(), review)
	if err != nil {
		t.Fatal(err)
	}
	if placed.ID != "order-1" {
		t.Fatalf("did not adopt broker order: %+v", placed)
	}
}

func TestNormalizeUnknownStatus(t *testing.T) {
	if got := normalizeStatus("future_state"); got != domain.BrokerOrderUnknown {
		t.Fatalf("got %q", got)
	}
}

func TestValidateMCPEndpointRejectsCredentialedOrPlainHTTPURLs(t *testing.T) {
	for _, endpoint := range []string{
		"http://agent.robinhood.com/mcp/trading",
		"https://token@agent.robinhood.com/mcp/trading",
		"not a URL",
	} {
		if err := validateMCPEndpoint(endpoint); err == nil {
			t.Fatalf("accepted unsafe endpoint %q", endpoint)
		}
	}
	if err := validateMCPEndpoint(DefaultMCPURL); err != nil {
		t.Fatalf("official endpoint rejected: %v", err)
	}
}
