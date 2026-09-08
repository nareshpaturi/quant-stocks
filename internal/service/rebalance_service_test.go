package service

import (
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/nareshpaturi/quant-stocks/internal/domain"
)

func TestLastRankingDayUsesNewYorkBoundary(t *testing.T) {
	// Sunday at 02:00 UTC is still Saturday evening in New York.
	now := time.Date(2026, 9, 6, 2, 0, 0, 0, time.UTC)
	if got, want := lastRankingDayAt(now), "2026-08-30"; got != want {
		t.Fatalf("lastRankingDayAt()=%s, want %s", got, want)
	}
}

func TestRankingCycleWindowUsesMarketTimezone(t *testing.T) {
	start, end, err := rankingCycleWindow("2026-09-06")
	if err != nil {
		t.Fatal(err)
	}
	_, startOffset := start.Zone()
	if startOffset != -4*60*60 {
		t.Fatalf("September offset=%d, want EDT", startOffset)
	}
	if end.Sub(start) != 7*24*time.Hour {
		t.Fatalf("cycle duration=%s", end.Sub(start))
	}
}

func TestDecisionTickersLimitsQuotesToThreshold(t *testing.T) {
	rankings := []domain.Rank{
		{Ticker: "D", Position: 4}, {Ticker: "A", Position: 1},
		{Ticker: "C", Position: 3}, {Ticker: "B", Position: 2},
	}
	got := decisionTickers(rankings, 3)
	if len(got) != 3 || got[0] != "A" || got[2] != "C" {
		t.Fatalf("unexpected decision tickers: %v", got)
	}
}

func TestPrintRebalanceSummaryIncludesBroker(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	original := os.Stdout
	os.Stdout = writer
	defer func() {
		os.Stdout = original
		reader.Close()
		writer.Close()
	}()

	printRebalanceSummary(
		domain.PortfolioConfig{IndexName: "ndx", BrokerName: "robinhood"},
		domain.RebalanceResult{}, nil, nil, nil, nil, 0,
	)
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	os.Stdout = original
	output, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(output), "  Broker: robinhood\n") {
		t.Fatalf("summary omitted broker:\n%s", output)
	}
}
