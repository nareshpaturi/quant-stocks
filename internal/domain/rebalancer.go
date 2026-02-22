package domain

import (
	"math"
	"sort"
)

// Rebalance is the pure domain function implementing the slack-based momentum strategy.
//
// It takes a snapshot of the world and returns a RebalanceResult describing
// exactly what to trade. It has zero I/O and zero side effects.
//
// Strategy rules (N = cfg.MaxStocks, S = cfg.SlackValue, threshold = N+S):
//
//  1. SELL: held stock whose rank > threshold, or is no longer in the index.
//  2. RETAIN: held stock whose rank <= threshold (both top-N and slack zone).
//  3. BUY: top-ranked non-held stocks (rank <= N only) until portfolio = MaxStocks.
//
// Cash sizing:
//
//	projectedCash   = availableCash + sum(each sell's market value)
//	cashPerPosition = projectedCash / len(buyCandidates)
//	shares          = math.Floor(cashPerPosition / candidate.Price)
//
// The rankings slice is assumed to be sorted ascending by Position (best rank first),
// but this function re-sorts defensively to guarantee correctness.
func Rebalance(
	cfg PortfolioConfig,
	positions []Position,
	rankings []Rank,
	availableCash float64,
) RebalanceResult {

	// ── Build lookup maps ─────────────────────────────────────────────────────
	heldByTicker := make(map[string]Position, len(positions))
	for _, p := range positions {
		heldByTicker[p.Ticker] = p
	}

	rankByTicker := make(map[string]Rank, len(rankings))
	for _, r := range rankings {
		rankByTicker[r.Ticker] = r
	}

	// ── Step 1: Determine SELLS using retention/slack logic ───────────────────
	threshold := cfg.MaxStocks + cfg.SlackValue

	var sells []Order
	retains := make([]string, 0)

	for _, pos := range positions {
		r, inIndex := rankByTicker[pos.Ticker]
		if !inIndex || r.Position > threshold {
			// Not in index at all, or rank exceeds slack zone → liquidate.
			reason := "rank-exceeded-slack"
			if !inIndex {
				reason = "not-in-index"
			}
			sells = append(sells, Order{
				Ticker: pos.Ticker,
				Side:   OrderSideSell,
				Shares: pos.Shares,
				Reason: reason,
			})
		} else {
			// rank <= threshold → retain (covers both top-N and slack zone).
			retains = append(retains, pos.Ticker)
		}
	}

	// ── Step 2: Project cash after sells ─────────────────────────────────────
	projectedCash := availableCash
	for _, sell := range sells {
		if pos, ok := heldByTicker[sell.Ticker]; ok {
			projectedCash += pos.MarketValue()
		}
	}

	// ── Step 3: Determine retained set (holdings after sells) ─────────────────
	retainedSet := make(map[string]bool, len(retains))
	for _, t := range retains {
		retainedSet[t] = true
	}

	// ── Step 4: Determine open slots ─────────────────────────────────────────
	slotsAvailable := cfg.MaxStocks - len(retainedSet)
	if slotsAvailable <= 0 || projectedCash <= 0 {
		return RebalanceResult{Sells: sells, Retains: retains}
	}

	// ── Step 5: Sort rankings by position ascending (best rank first) ─────────
	sorted := make([]Rank, len(rankings))
	copy(sorted, rankings)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].Position < sorted[j].Position
	})

	// ── Step 6: Collect buy candidates (top-N only, not already held) ─────────
	candidates := make([]Rank, 0, slotsAvailable)
	for _, r := range sorted {
		if r.Position > cfg.MaxStocks {
			break // Only acquire within the top-N tier
		}
		if !retainedSet[r.Ticker] {
			candidates = append(candidates, r)
		}
		if len(candidates) == slotsAvailable {
			break
		}
	}

	if len(candidates) == 0 {
		return RebalanceResult{Sells: sells, Retains: retains}
	}

	// ── Step 7: Size each buy equally from projected cash ─────────────────────
	cashPerPosition := projectedCash / float64(len(candidates))

	buys := make([]Order, 0, len(candidates))
	for _, c := range candidates {
		shares := sharesToBuy(cashPerPosition, c.Price)
		if shares < 1 {
			// Not enough cash to buy even one share; skip this position.
			continue
		}
		buys = append(buys, Order{
			Ticker: c.Ticker,
			Side:   OrderSideBuy,
			Shares: shares,
			Reason: "acquire",
		})
	}

	return RebalanceResult{
		Sells:   sells,
		Buys:    buys,
		Retains: retains,
	}
}

// sharesToBuy returns the number of whole shares purchasable for the given
// notional amount at the given price. Uses math.Floor (not Round) to guarantee
// the spend never exceeds the allocated notional. Returns 0 if price <= 0.
func sharesToBuy(notional, price float64) float64 {
	if price <= 0 {
		return 0
	}
	return math.Floor(notional / price)
}
