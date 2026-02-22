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
// Buy sizing — two modes based on how the slot was opened:
//
//	Sell-funded slot (one slot per sell, capped by candidates available):
//	  cashPerSlot = sellProceeds / numSellFunded
//	  sellProceeds = sum(each sell's Shares * CurrentPrice)
//
//	Organic slot (pre-existing empty slot not freed by a sell this cycle):
//	  cashPerSlot = cfg.InitialAmountPerStock
//	  Caller (config.Validate) must ensure InitialAmountPerStock > 0.
//
// In both cases: shares = math.Floor(cashPerSlot / candidate.Price)
//
// The availableCash parameter is accepted for interface compatibility but is not
// used in buy sizing — sell proceeds and InitialAmountPerStock drive all orders.
//
// The rankings slice is assumed to be sorted ascending by Position (best rank first),
// but this function re-sorts defensively to guarantee correctness.
func Rebalance(
	cfg PortfolioConfig,
	positions []Position,
	rankings []Rank,
	availableCash float64,
) RebalanceResult {
	_ = availableCash // not used in sizing; caller still fetches for broker balance checks

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

	// ── Step 2: Compute sell proceeds (drives sell-funded buy sizing) ─────────
	sellProceeds := 0.0
	for _, sell := range sells {
		if pos, ok := heldByTicker[sell.Ticker]; ok {
			sellProceeds += pos.MarketValue()
		}
	}

	// ── Step 3: Determine retained set (holdings after sells) ─────────────────
	retainedSet := make(map[string]bool, len(retains))
	for _, t := range retains {
		retainedSet[t] = true
	}

	// ── Step 4: Determine open slots ─────────────────────────────────────────
	slotsAvailable := cfg.MaxStocks - len(retainedSet)
	if slotsAvailable <= 0 {
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

	// ── Step 7: Size buys by funding source ───────────────────────────────────
	//
	// Sell-funded slots (one per sell, capped to available candidates):
	//   each buy gets an equal share of the sell proceeds.
	// Organic slots (pre-existing empty slots, not freed by a sell this cycle):
	//   each buy gets cfg.InitialAmountPerStock.
	numSellFunded := len(sells)
	if numSellFunded > len(candidates) {
		numSellFunded = len(candidates)
	}

	buys := make([]Order, 0, len(candidates))

	// Sell-funded buys: split sell proceeds equally.
	if numSellFunded > 0 {
		cashPerSlot := sellProceeds / float64(numSellFunded)
		for _, c := range candidates[:numSellFunded] {
			shares := sharesToBuy(cashPerSlot, c.Price)
			if shares < 1 {
				continue
			}
			buys = append(buys, Order{
				Ticker: c.Ticker,
				Side:   OrderSideBuy,
				Shares: shares,
				Reason: "acquire",
			})
		}
	}

	// Organic buys: fixed initial amount per slot.
	for _, c := range candidates[numSellFunded:] {
		shares := sharesToBuy(cfg.InitialAmountPerStock, c.Price)
		if shares < 1 {
			continue
		}
		buys = append(buys, Order{
			Ticker: c.Ticker,
			Side:   OrderSideBuy,
			Shares: shares,
			Reason: "initial-fill",
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
