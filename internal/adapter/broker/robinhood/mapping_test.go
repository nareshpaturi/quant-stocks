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

	arguments, err := orderArguments(schema, "agentic-1", testOrderFields(), "")
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

	arguments, err := orderArguments(schema, "agentic-1", testOrderFields(), "")
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
	arguments, err := orderArguments(schema, "agentic-1", testOrderFields(), "")
	if err != nil {
		t.Fatal(err)
	}
	if got := arguments["time_in_force"]; got != "gfd" {
		t.Fatalf("time_in_force = %#v, want gfd", got)
	}
}

func testOrderFields() map[string]any {
	return map[string]any{
		"symbol":          "AAPL",
		"side":            "buy",
		"quantity":        2.0,
		"order_type":      "market",
		"duration":        "day",
		"client_order_id": "client-1",
	}
}
