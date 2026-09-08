package robinhood

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

var accountAliases = []string{"account_id", "accountId", "account_number", "accountNumber"}

type schemaArgs struct {
	properties map[string]any
	values     map[string]any
}

func newSchemaArgs(schema map[string]any) *schemaArgs {
	properties, _ := schema["properties"].(map[string]any)
	return &schemaArgs{properties: properties, values: make(map[string]any)}
}

func (a *schemaArgs) setRequired(aliases []string, value any) error {
	if !a.setOptional(aliases, value) {
		return fmt.Errorf("tool schema has none of the required properties %v", aliases)
	}
	return nil
}

func (a *schemaArgs) setOptional(aliases []string, value any) bool {
	for _, alias := range aliases {
		property, ok := a.properties[alias]
		if !ok {
			continue
		}
		a.values[alias] = coerceSchemaValue(property, value)
		return true
	}
	return false
}

func coerceSchemaValue(property, value any) any {
	definition, _ := property.(map[string]any)
	typeName, _ := definition["type"].(string)
	switch typeName {
	case "string":
		var result string
		switch typed := value.(type) {
		case []string:
			result = strings.Join(typed, ",")
		default:
			result = fmt.Sprint(value)
		}
		return coerceStringEnum(definition, result)
	case "number", "integer":
		if parsed, ok := asFloat(value); ok {
			return parsed
		}
	}
	return value
}

func coerceStringEnum(definition map[string]any, value string) string {
	allowed, _ := definition["enum"].([]any)
	for _, candidate := range allowed {
		if strings.EqualFold(fmt.Sprint(candidate), value) {
			return fmt.Sprint(candidate)
		}
	}
	equivalents := []string{value}
	if strings.EqualFold(value, "day") {
		equivalents = append(equivalents, "gfd", "good_for_day")
	}
	for _, equivalent := range equivalents {
		for _, candidate := range allowed {
			if strings.EqualFold(fmt.Sprint(candidate), equivalent) {
				return fmt.Sprint(candidate)
			}
		}
	}
	return value
}

func orderArguments(schema map[string]any, accountID string, orderFields map[string]any, reviewID string) (map[string]any, error) {
	args := newSchemaArgs(schema)
	accountSet := args.setOptional(accountAliases, accountID)
	reviewIDSet := false
	if reviewID != "" {
		reviewIDSet = args.setOptional([]string{"review_id", "reviewId", "order_review_id", "orderReviewId"}, reviewID)
	}

	for _, containerName := range []string{"order", "order_config", "orderConfig"} {
		property, ok := args.properties[containerName].(map[string]any)
		if !ok {
			continue
		}
		nested := newSchemaArgs(property)
		if nested.properties == nil {
			continue
		}
		if !accountSet {
			accountSet = nested.setOptional(accountAliases, accountID)
		}
		if err := populateOrderFields(nested, orderFields); err != nil {
			return nil, err
		}
		args.values[containerName] = nested.values
		return args.values, nil
	}
	if reviewIDSet && !hasAnyProperty(args.properties, []string{"symbol", "ticker"}) {
		return args.values, nil
	}
	if err := populateOrderFields(args, orderFields); err != nil {
		return nil, err
	}
	return args.values, nil
}

func hasAnyProperty(properties map[string]any, aliases []string) bool {
	for _, alias := range aliases {
		if _, ok := properties[alias]; ok {
			return true
		}
	}
	return false
}

func populateOrderFields(args *schemaArgs, fields map[string]any) error {
	required := []struct {
		aliases []string
		key     string
	}{
		{[]string{"symbol", "ticker"}, "symbol"},
		{[]string{"side"}, "side"},
		{[]string{"quantity", "qty", "shares"}, "quantity"},
		{[]string{"order_type", "orderType", "type"}, "order_type"},
		{[]string{"time_in_force", "timeInForce", "duration"}, "duration"},
	}
	for _, field := range required {
		if err := args.setRequired(field.aliases, fields[field.key]); err != nil {
			return err
		}
	}
	args.setOptional([]string{"client_order_id", "clientOrderId", "client_order_identifier"}, fields["client_order_id"])
	return nil
}

func decodeAny(raw json.RawMessage) (any, error) {
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, err
	}
	return value, nil
}

func records(raw json.RawMessage, containers ...string) ([]map[string]any, error) {
	value, err := decodeAny(raw)
	if err != nil {
		return nil, err
	}
	return recordsFrom(value, containers), nil
}

func recordsFrom(value any, containers []string) []map[string]any {
	switch typed := value.(type) {
	case []any:
		result := make([]map[string]any, 0, len(typed))
		for _, item := range typed {
			if record, ok := item.(map[string]any); ok {
				result = append(result, record)
			}
		}
		return result
	case map[string]any:
		for _, container := range containers {
			if nested, ok := typed[container]; ok {
				if result := recordsFrom(nested, containers); len(result) > 0 {
					return result
				}
			}
		}
		for _, wrapper := range []string{"result", "data", "items"} {
			if nested, ok := typed[wrapper]; ok {
				if result := recordsFrom(nested, containers); len(result) > 0 {
					return result
				}
			}
		}
		return []map[string]any{typed}
	default:
		return nil
	}
}

func lookup(record map[string]any, aliases ...string) (any, bool) {
	for _, alias := range aliases {
		if value, ok := record[alias]; ok {
			return value, true
		}
	}
	for _, value := range record {
		nested, ok := value.(map[string]any)
		if !ok {
			continue
		}
		if found, ok := lookup(nested, aliases...); ok {
			return found, true
		}
	}
	return nil, false
}

func stringField(record map[string]any, aliases ...string) string {
	value, ok := lookup(record, aliases...)
	if !ok || value == nil {
		return ""
	}
	return strings.TrimSpace(fmt.Sprint(value))
}

func floatField(record map[string]any, aliases ...string) float64 {
	value, ok := lookup(record, aliases...)
	if !ok {
		return 0
	}
	parsed, _ := asFloat(value)
	return parsed
}

func asFloat(value any) (float64, bool) {
	switch typed := value.(type) {
	case float64:
		return typed, true
	case float32:
		return float64(typed), true
	case int:
		return float64(typed), true
	case int64:
		return float64(typed), true
	case json.Number:
		parsed, err := typed.Float64()
		return parsed, err == nil
	case string:
		clean := strings.ReplaceAll(strings.ReplaceAll(strings.TrimSpace(typed), "$", ""), ",", "")
		parsed, err := strconv.ParseFloat(clean, 64)
		return parsed, err == nil
	default:
		return 0, false
	}
}

func boolField(record map[string]any, aliases ...string) (bool, bool) {
	value, ok := lookup(record, aliases...)
	if !ok {
		return false, false
	}
	switch typed := value.(type) {
	case bool:
		return typed, true
	case string:
		parsed, err := strconv.ParseBool(typed)
		return parsed, err == nil
	default:
		return false, false
	}
}

func timeField(record map[string]any, aliases ...string) time.Time {
	value := stringField(record, aliases...)
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02 15:04:05", "2006-01-02"} {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed
		}
	}
	return time.Time{}
}

func containsAgentic(value any) bool {
	switch typed := value.(type) {
	case string:
		tokens := strings.FieldsFunc(strings.ToLower(typed), func(r rune) bool {
			return (r < 'a' || r > 'z') && (r < '0' || r > '9')
		})
		for i, token := range tokens {
			if token == "agentic" && (i == 0 || (tokens[i-1] != "non" && tokens[i-1] != "not")) {
				return true
			}
		}
		return false
	case map[string]any:
		for _, nested := range typed {
			if containsAgentic(nested) {
				return true
			}
		}
	case []any:
		for _, nested := range typed {
			if containsAgentic(nested) {
				return true
			}
		}
	}
	return false
}

func stringSliceField(record map[string]any, aliases ...string) []string {
	value, ok := lookup(record, aliases...)
	if !ok {
		return nil
	}
	switch typed := value.(type) {
	case []any:
		result := make([]string, 0, len(typed))
		for _, item := range typed {
			result = append(result, fmt.Sprint(item))
		}
		return result
	case string:
		if typed == "" {
			return nil
		}
		return []string{typed}
	default:
		return nil
	}
}
