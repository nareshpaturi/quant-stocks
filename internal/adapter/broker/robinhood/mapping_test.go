package robinhood

import "testing"

func TestOrderArgumentsSupportsNestedAccountSelector(t *testing.T) {
	schema := map[string]any{
		"properties": map[string]any{
			"order": map[string]any{
				"properties": map[string]any{
					"account_number": map[string]any{"type": "string"},
					"symbol":         map[string]any{"type": "string"},
					"side":           map[string]any{"type": "string"},
					"quantity":       map[string]any{"type": "number"},
					"order_type":     map[string]any{"type": "string"},
					"time_in_force":  map[string]any{"type": "string"},
				},
			},
		},
	}

	arguments, err := orderArguments(schema, "agentic-1", testOrderFields(), "", false)
	if err != nil {
		t.Fatalf("orderArguments() error = %v", err)
	}
	order, ok := arguments["order"].(map[string]any)
	if !ok {
		t.Fatalf("nested order = %#v", arguments["order"])
	}
	if got := order["account_number"]; got != "agentic-1" {
		t.Fatalf("account_number = %#v, want agentic-1", got)
	}
}

func TestOrderArgumentsAllowsServerBoundAccount(t *testing.T) {
	schema := map[string]any{
		"properties": map[string]any{
			"symbol":        map[string]any{"type": "string"},
			"side":          map[string]any{"type": "string"},
			"quantity":      map[string]any{"type": "number"},
			"order_type":    map[string]any{"type": "string"},
			"time_in_force": map[string]any{"type": "string"},
		},
	}

	arguments, err := orderArguments(schema, "agentic-1", testOrderFields(), "", false)
	if err != nil {
		t.Fatalf("orderArguments() error = %v", err)
	}
	for _, alias := range accountAliases {
		if _, exists := arguments[alias]; exists {
			t.Fatalf("unsupported account selector %q was injected", alias)
		}
	}
}

func TestOrderArgumentsMapsDayToSchemaGFDEnum(t *testing.T) {
	schema := map[string]any{"properties": map[string]any{
		"symbol": map[string]any{"type": "string"}, "side": map[string]any{"type": "string"},
		"quantity": map[string]any{"type": "number"}, "order_type": map[string]any{"type": "string"},
		"time_in_force": map[string]any{"type": "string", "enum": []any{"gfd", "gtc"}},
	}}
	arguments, err := orderArguments(schema, "agentic-1", testOrderFields(), "", false)
	if err != nil {
		t.Fatal(err)
	}
	if got := arguments["time_in_force"]; got != "gfd" {
		t.Fatalf("time_in_force = %#v, want gfd", got)
	}
}

func TestOrderArgumentsPrefersDollarAmount(t *testing.T) {
	schema := map[string]any{"properties": map[string]any{
		"symbol": map[string]any{"type": "string"}, "side": map[string]any{"type": "string"},
		"quantity": map[string]any{"type": "string"}, "dollar_amount": map[string]any{"type": "string"},
		"type": map[string]any{"type": "string"}, "time_in_force": map[string]any{"type": "string"},
		"market_hours": map[string]any{"type": "string"},
	}}
	arguments, err := orderArguments(schema, "agentic-1", testOrderFields(), "", true)
	if err != nil {
		t.Fatal(err)
	}
	if got := arguments["dollar_amount"]; got != "1000" {
		t.Fatalf("dollar_amount = %#v, want 1000", got)
	}
	if _, exists := arguments["quantity"]; exists {
		t.Fatalf("dollar order also contained quantity: %#v", arguments)
	}
	if got := arguments["market_hours"]; got != "regular_hours" {
		t.Fatalf("market_hours = %#v, want regular_hours", got)
	}
}

func testOrderFields() map[string]any {
	return map[string]any{
		"symbol":          "AAPL",
		"side":            "buy",
		"quantity":        2.0,
		"dollar_amount":   1000.0,
		"order_type":      "market",
		"duration":        "day",
		"market_hours":    "regular_hours",
		"client_order_id": "client-1",
	}
}
