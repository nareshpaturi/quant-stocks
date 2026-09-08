package domain

import (
	"crypto/sha256"
	"fmt"
	"math"
	"sort"
	"strings"
)

// CycleSnapshot is the complete broker and ranking state used to reconstruct a
// weekly rebalance. Orders must already be limited to the active cycle window.
type CycleSnapshot struct {
	CycleID   string
	Account   AccountSnapshot
	Positions []Position
	Rankings  []Rank
	Orders    []BrokerOrder
}

// ReconciliationBlock records work that was deliberately not submitted.
type ReconciliationBlock struct {
	Ticker string
	Side   OrderSide
	Reason string
}

// ReconciliationResult contains only orders that are missing from broker state.
type ReconciliationResult struct {
	Sells   []Order
	Buys    []Order
	Retains []string
	Blocked []ReconciliationBlock
}

// NewCycleID creates a stable, non-secret identifier for one profile's weekly
// ranking cycle. The account id is hashed rather than exposed in logs.
func NewCycleID(profile, accountID, index, rankingDate string) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{profile, accountID, index, rankingDate}, "\x00")))
	return fmt.Sprintf("%s:%x", rankingDate, sum[:8])
}

// NewClientOrderID creates a deterministic UUID-shaped idempotency key. A new
// attempt number is used only after a previous attempt reached a terminal state.
func NewClientOrderID(cycleID, ticker string, side OrderSide, slot, attempt int) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%s\x00%d\x00%d", cycleID, ticker, side, slot, attempt)))
	b := sum[:16]
	b[6] = (b[6] & 0x0f) | 0x50 // UUID version 5 shape
	b[8] = (b[8] & 0x3f) | 0x80 // RFC 4122 variant
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// Reconcile reconstructs the opening portfolio by reversing cycle fills, runs
// the existing strategy against that opening state, and returns only work that
// is neither complete nor active at the broker.
func Reconcile(cfg PortfolioConfig, snapshot CycleSnapshot) ReconciliationResult {
	positions := positionMap(snapshot.Positions)
	orders := relevantOrders(snapshot)
	opening := reconstructOpeningPositions(positions, snapshot.Rankings, orders)
	desired := Rebalance(cfg, opening, snapshot.Rankings, snapshot.Account.BuyingPower)

	result := ReconciliationResult{Retains: desired.Retains}
	byKey := groupOrders(orders)

	for slot, sell := range desired.Sells {
		history := byKey[orderKey(sell.Ticker, OrderSideSell)]
		if hasUnknown(history) {
			result.Blocked = append(result.Blocked, ReconciliationBlock{
				Ticker: sell.Ticker, Side: sell.Side, Reason: "unknown broker order status",
			})
			continue
		}
		if hasActive(history) {
			continue
		}
		current := positions[sell.Ticker]
		if current.Shares <= 0 {
			continue
		}
		sell.Shares = current.Shares
		sell.CycleID = snapshot.CycleID
		sell.ClientOrderID = NewClientOrderID(snapshot.CycleID, sell.Ticker, sell.Side, slot, terminalAttempts(history))
		result.Sells = append(result.Sells, sell)
	}

	// Replacement buys use actual sell fills. Active buy remainders reserve
	// their estimated value, while terminal unfilled remainders are retryable.
	replacement := make([]Order, 0, len(desired.Buys))
	organic := make([]Order, 0, len(desired.Buys))
	for _, buy := range desired.Buys {
		if buy.Reason == "acquire" {
			replacement = append(replacement, buy)
		} else {
			organic = append(organic, buy)
		}
	}

	realizedSellValue := 0.0
	sellProceedsUnknown := false
	for _, sell := range desired.Sells {
		for _, order := range byKey[orderKey(sell.Ticker, OrderSideSell)] {
			if order.FilledShares > 0 && order.AverageFillPrice <= 0 {
				sellProceedsUnknown = true
			}
			realizedSellValue += order.FilledValue()
		}
	}

	replacementCommitted := 0.0
	replacementCommitmentUnknown := false
	missingReplacement := make([]Order, 0, len(replacement))
	for _, buy := range replacement {
		history := byKey[orderKey(buy.Ticker, OrderSideBuy)]
		for _, order := range history {
			committed, known := committedValue(order, buy.ReferencePrice)
			replacementCommitted += committed
			if !known {
				replacementCommitmentUnknown = true
			}
		}
		if hasUnknown(history) {
			result.Blocked = append(result.Blocked, ReconciliationBlock{
				Ticker: buy.Ticker, Side: buy.Side, Reason: "unknown broker order status",
			})
			continue
		}
		if hasActive(history) || hasFilledCompletion(history) {
			continue
		}
		if len(history) == 0 && positions[buy.Ticker].Shares > 0 {
			continue
		}
		missingReplacement = append(missingReplacement, buy)
	}

	remainingReplacement := math.Max(0, realizedSellValue-replacementCommitted)
	buyingPower := math.Max(0, snapshot.Account.BuyingPower)
	if sellProceedsUnknown {
		for _, buy := range missingReplacement {
			result.Blocked = append(result.Blocked, ReconciliationBlock{
				Ticker: buy.Ticker, Side: buy.Side, Reason: "filled sell proceeds cannot be valued",
			})
		}
	} else if replacementCommitmentUnknown {
		for _, buy := range missingReplacement {
			result.Blocked = append(result.Blocked, ReconciliationBlock{
				Ticker: buy.Ticker, Side: buy.Side, Reason: "active replacement commitment cannot be valued",
			})
		}
	} else if len(missingReplacement) > 0 && remainingReplacement > 0 {
		perOrder := remainingReplacement / float64(len(missingReplacement))
		for slot, buy := range missingReplacement {
			history := byKey[orderKey(buy.Ticker, OrderSideBuy)]
			buy.Notional = math.Min(perOrder, buyingPower)
			if buy.Notional <= 0 {
				result.Blocked = append(result.Blocked, ReconciliationBlock{
					Ticker: buy.Ticker, Side: buy.Side, Reason: "insufficient buying power",
				})
				continue
			}
			buyingPower -= buy.Notional
			buy.CycleID = snapshot.CycleID
			buy.ClientOrderID = NewClientOrderID(snapshot.CycleID, buy.Ticker, buy.Side, slot, terminalAttempts(history))
			result.Buys = append(result.Buys, buy)
		}
	}

	for slot, buy := range organic {
		history := byKey[orderKey(buy.Ticker, OrderSideBuy)]
		if hasUnknown(history) {
			result.Blocked = append(result.Blocked, ReconciliationBlock{
				Ticker: buy.Ticker, Side: buy.Side, Reason: "unknown broker order status",
			})
			continue
		}
		if hasActive(history) || hasFilledCompletion(history) {
			continue
		}
		if len(history) == 0 && positions[buy.Ticker].Shares > 0 {
			continue
		}
		spent := 0.0
		commitmentUnknown := false
		for _, order := range history {
			committed, known := committedValue(order, buy.ReferencePrice)
			spent += committed
			if !known {
				commitmentUnknown = true
			}
		}
		if commitmentUnknown {
			result.Blocked = append(result.Blocked, ReconciliationBlock{
				Ticker: buy.Ticker, Side: buy.Side, Reason: "buy commitment cannot be valued",
			})
			continue
		}
		buy.Notional = math.Min(math.Max(0, buy.Notional-spent), buyingPower)
		if buy.Notional <= 0 {
			result.Blocked = append(result.Blocked, ReconciliationBlock{
				Ticker: buy.Ticker, Side: buy.Side, Reason: "insufficient buying power",
			})
			continue
		}
		buyingPower -= buy.Notional
		buy.CycleID = snapshot.CycleID
		buy.ClientOrderID = NewClientOrderID(snapshot.CycleID, buy.Ticker, buy.Side, len(replacement)+slot, terminalAttempts(history))
		result.Buys = append(result.Buys, buy)
	}

	return result
}

func relevantOrders(snapshot CycleSnapshot) []BrokerOrder {
	result := make([]BrokerOrder, 0, len(snapshot.Orders))
	for _, order := range snapshot.Orders {
		if snapshot.Account.AccountID != "" && order.AccountID != "" && order.AccountID != snapshot.Account.AccountID {
			continue
		}
		if order.CycleID != "" && snapshot.CycleID != "" && order.CycleID != snapshot.CycleID {
			continue
		}
		result = append(result, order)
	}
	return result
}

func positionMap(positions []Position) map[string]Position {
	result := make(map[string]Position, len(positions))
	for _, position := range positions {
		position.Ticker = strings.ToUpper(strings.TrimSpace(position.Ticker))
		result[position.Ticker] = position
	}
	return result
}

func reconstructOpeningPositions(current map[string]Position, rankings []Rank, orders []BrokerOrder) []Position {
	quantities := make(map[string]float64, len(current))
	prices := make(map[string]float64, len(current))
	for ticker, position := range current {
		quantities[ticker] = position.Shares
		prices[ticker] = position.CurrentPrice
	}
	for _, rank := range rankings {
		if rank.Price > 0 && prices[rank.Ticker] <= 0 {
			prices[rank.Ticker] = rank.Price
		}
	}
	for _, order := range orders {
		if order.FilledShares <= 0 {
			continue
		}
		if order.Side == OrderSideSell {
			quantities[order.Ticker] += order.FilledShares
		} else if order.Side == OrderSideBuy {
			quantities[order.Ticker] -= order.FilledShares
		}
		if prices[order.Ticker] <= 0 {
			prices[order.Ticker] = order.AverageFillPrice
		}
	}

	tickers := make([]string, 0, len(quantities))
	for ticker, quantity := range quantities {
		if quantity > 1e-9 {
			tickers = append(tickers, ticker)
		}
	}
	sort.Strings(tickers)
	result := make([]Position, 0, len(tickers))
	for _, ticker := range tickers {
		result = append(result, Position{Ticker: ticker, Shares: quantities[ticker], CurrentPrice: prices[ticker]})
	}
	return result
}

func groupOrders(orders []BrokerOrder) map[string][]BrokerOrder {
	result := make(map[string][]BrokerOrder)
	for _, order := range orders {
		result[orderKey(order.Ticker, order.Side)] = append(result[orderKey(order.Ticker, order.Side)], order)
	}
	return result
}

func orderKey(ticker string, side OrderSide) string {
	return strings.ToUpper(strings.TrimSpace(ticker)) + ":" + string(side)
}

func hasActive(orders []BrokerOrder) bool {
	for _, order := range orders {
		if order.Active() {
			return true
		}
	}
	return false
}

func hasUnknown(orders []BrokerOrder) bool {
	for _, order := range orders {
		if order.Status == BrokerOrderUnknown {
			return true
		}
	}
	return false
}

func hasFilledCompletion(orders []BrokerOrder) bool {
	for _, order := range orders {
		if order.Status == BrokerOrderFilled {
			return true
		}
	}
	return false
}

func terminalAttempts(orders []BrokerOrder) int {
	attempts := 0
	for _, order := range orders {
		if order.TerminalFailure() {
			attempts++
		}
	}
	return attempts
}

func committedValue(order BrokerOrder, fallbackPrice float64) (float64, bool) {
	if order.FilledShares > 0 && order.AverageFillPrice <= 0 {
		return 0, false
	}
	value := order.FilledValue()
	if !order.Active() {
		return value, true
	}
	if order.RemainingShares <= 0 {
		return value, false
	}
	price := order.EstimatedPrice
	if price <= 0 {
		price = fallbackPrice
	}
	if price <= 0 {
		price = order.AverageFillPrice
	}
	if price <= 0 {
		if order.EstimatedNotional > value {
			return order.EstimatedNotional, true
		}
		return value, false
	}
	value += order.RemainingShares * price
	if order.EstimatedNotional > value {
		value = order.EstimatedNotional
	}
	return value, true
}
