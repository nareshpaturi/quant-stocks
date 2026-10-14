package alpaca

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/nareshpaturi/quant-stocks/internal/domain"
)

func newTestBroker(t *testing.T, handler http.HandlerFunc) *Broker {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	broker, err := New("key", "secret", true, "iex")
	if err != nil {
		t.Fatal(err)
	}
	broker.tradingURL, broker.dataURL = server.URL, server.URL
	return broker
}

func TestGetSessionParsesNewYorkTimes(t *testing.T) {
	broker := newTestBroker(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/calendar" || r.URL.Query().Get("start") != "2026-11-27" || r.URL.Query().Get("end") != "2026-11-27" {
			t.Fatalf("unexpected request %s", r.URL)
		}
		if r.Header.Get("APCA-API-KEY-ID") != "key" || r.Header.Get("APCA-API-SECRET-KEY") != "secret" {
			t.Fatalf("missing auth headers")
		}
		io.WriteString(w, `[{"date":"2026-11-27","open":"09:30","close":"13:00"}]`)
	})

	session, ok, err := broker.GetSession(context.Background(), "2026-11-27")
	if err != nil || !ok {
		t.Fatalf("GetSession() ok=%v err=%v", ok, err)
	}
	if got := session.Close.UTC().Format(time.RFC3339); got != "2026-11-27T18:00:00Z" {
		t.Fatalf("Close=%s, want 13:00 EST", got)
	}
	if got := session.Open.UTC().Format(time.RFC3339); got != "2026-11-27T14:30:00Z" {
		t.Fatalf("Open=%s, want 09:30 EST", got)
	}
}

func TestGetSessionReportsHoliday(t *testing.T) {
	broker := newTestBroker(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `[]`)
	})
	if _, ok, err := broker.GetSession(context.Background(), "2026-11-26"); err != nil || ok {
		t.Fatalf("GetSession() ok=%v err=%v, want closed", ok, err)
	}
}

func TestGetPositionSharesTreatsAlpaca404AsNoPosition(t *testing.T) {
	broker := newTestBroker(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/positions/SOXL" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, `{"code":40410000,"message":"position does not exist"}`)
	})
	shares, err := broker.GetPositionShares(context.Background(), "SOXL")
	if err != nil || shares != 0 {
		t.Fatalf("GetPositionShares()=%v, %v; want 0, nil", shares, err)
	}
}

func TestGetPositionSharesRejectsNonAlpaca404(t *testing.T) {
	broker := newTestBroker(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, `<html>not found</html>`)
	})
	if _, err := broker.GetPositionShares(context.Background(), "SOXL"); err == nil {
		t.Fatal("GetPositionShares() succeeded on a 404 that did not come from Alpaca")
	}
}

func TestGetAccountRejectsBlockedAccount(t *testing.T) {
	broker := newTestBroker(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"account_number":"PA1","status":"ACTIVE","cash":"100","buying_power":"200","trading_blocked":true}`)
	})
	if _, err := broker.GetAccount(context.Background()); err == nil {
		t.Fatal("GetAccount() accepted a trading-blocked account")
	}
}

func TestGetQuoteReadsSnapshot(t *testing.T) {
	broker := newTestBroker(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/stocks/SOXL/snapshot" || r.URL.Query().Get("feed") != "iex" {
			t.Fatalf("unexpected request %s", r.URL)
		}
		io.WriteString(w, `{"latestTrade":{"p":40.01},"latestQuote":{"ap":40.1,"bp":39.9}}`)
	})
	quote, err := broker.GetQuote(context.Background(), "SOXL")
	if err != nil {
		t.Fatal(err)
	}
	if quote != (domain.Quote{Bid: 39.9, Ask: 40.1, Last: 40.01}) {
		t.Fatalf("GetQuote()=%+v", quote)
	}
}

func TestPlaceOrderSendsMarketOrderOnce(t *testing.T) {
	calls := 0
	broker := newTestBroker(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != http.MethodPost || r.URL.Path != "/v2/orders" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL)
		}
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		want := map[string]string{
			"symbol": "SOXL", "qty": "246", "side": "sell", "type": "market",
			"time_in_force": "opg", "client_order_id": "overnight-SOXL-sell-open-2026-10-14",
		}
		for key, value := range want {
			if body[key] != value {
				t.Fatalf("%s=%q, want %q", key, body[key], value)
			}
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	})

	_, err := broker.PlaceOrder(context.Background(), domain.Order{
		Ticker: "SOXL", Side: domain.OrderSideSell, Type: domain.OrderTypeMarket,
		Duration: domain.OrderDurationOPG, Shares: 246, ClientOrderID: "overnight-SOXL-sell-open-2026-10-14",
	})
	if err == nil {
		t.Fatal("PlaceOrder() succeeded on a 503")
	}
	if calls != 1 {
		t.Fatalf("PlaceOrder sent %d requests, want 1 (no retry)", calls)
	}
}

func TestPlaceOrderSendsDollarAmountWithoutQty(t *testing.T) {
	broker := newTestBroker(t, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["notional"] != "990.55" || body["time_in_force"] != "day" {
			t.Fatalf("body=%v, want notional 990.55 day", body)
		}
		if _, ok := body["qty"]; ok {
			t.Fatalf("body=%v carries qty alongside notional", body)
		}
		io.WriteString(w, `{"id":"o1","client_order_id":"cid","symbol":"SOXL","side":"buy","type":"market",
			"time_in_force":"day","qty":null,"notional":"990.55","filled_qty":"0","status":"accepted"}`)
	})
	order, err := broker.PlaceOrder(context.Background(), domain.Order{
		Ticker: "SOXL", Side: domain.OrderSideBuy, Type: domain.OrderTypeMarket,
		Duration: domain.OrderDurationDay, Notional: 990.55, ClientOrderID: "cid",
	})
	if err != nil {
		t.Fatal(err)
	}
	if order.EstimatedNotional != 990.55 || order.RequestedShares != 0 || order.RemainingShares != 0 || !order.Active() {
		t.Fatalf("order=%+v", order)
	}
}

func TestPlaceOrderRejectsAmbiguousSize(t *testing.T) {
	broker := newTestBroker(t, func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("request sent for an invalid order")
	})
	for _, order := range []domain.Order{
		{Shares: 1, Notional: 100, Duration: domain.OrderDurationDay},
		{Notional: 100, Duration: domain.OrderDurationOPG},
		{Duration: domain.OrderDurationDay},
	} {
		order.Ticker, order.Side, order.Type, order.ClientOrderID = "SOXL", domain.OrderSideBuy, domain.OrderTypeMarket, "cid"
		if _, err := broker.PlaceOrder(context.Background(), order); err == nil {
			t.Fatalf("PlaceOrder(%+v) succeeded", order)
		}
	}
}

func TestIsFractionableReadsAsset(t *testing.T) {
	broker := newTestBroker(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/assets/SOXL" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		io.WriteString(w, `{"symbol":"SOXL","tradable":true,"fractionable":true}`)
	})
	if ok, err := broker.IsFractionable(context.Background(), "SOXL"); err != nil || !ok {
		t.Fatalf("IsFractionable()=%v, %v", ok, err)
	}
}

func TestGetOrderByClientIDMapsOrder(t *testing.T) {
	broker := newTestBroker(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/orders:by_client_order_id" || r.URL.Query().Get("client_order_id") != "cid" {
			t.Fatalf("unexpected request %s", r.URL)
		}
		io.WriteString(w, `{
			"id":"abc","client_order_id":"cid","symbol":"SOXL","side":"buy","type":"market",
			"time_in_force":"day","qty":"246","filled_qty":"100","filled_avg_price":"40.05",
			"status":"partially_filled","submitted_at":"2026-10-13T15:58:00.5-04:00"
		}`)
	})
	order, found, err := broker.GetOrderByClientID(context.Background(), "cid")
	if err != nil || !found {
		t.Fatalf("GetOrderByClientID() found=%v err=%v", found, err)
	}
	if order.Status != domain.BrokerOrderPartiallyFilled || order.FilledShares != 100 || order.RemainingShares != 146 || order.AverageFillPrice != 40.05 {
		t.Fatalf("order=%+v", order)
	}
}

func TestGetOrderByClientIDNotFound(t *testing.T) {
	broker := newTestBroker(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, `{"code":40410000,"message":"order not found"}`)
	})
	if _, found, err := broker.GetOrderByClientID(context.Background(), "cid"); err != nil || found {
		t.Fatalf("found=%v err=%v, want not found", found, err)
	}
}

func TestReadsRetryServerErrors(t *testing.T) {
	calls := 0
	broker := newTestBroker(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		io.WriteString(w, `{"timestamp":"2026-10-13T15:55:00-04:00","is_open":true,"next_open":"2026-10-14T09:30:00-04:00","next_close":"2026-10-13T16:00:00-04:00"}`)
	})
	clock, err := broker.GetClock(context.Background())
	if err != nil || !clock.IsOpen || calls != 2 {
		t.Fatalf("GetClock()=%+v err=%v after %d calls", clock, err, calls)
	}
}

func TestCalculatedStatusDependsOnFill(t *testing.T) {
	qty := "10"
	filled := orderResponse{ID: "a", Qty: &qty, FilledQty: "10", Status: "calculated"}
	partial := orderResponse{ID: "b", Qty: &qty, FilledQty: "4", Status: "calculated"}
	amount := "990"
	dollars := orderResponse{ID: "c", Notional: &amount, FilledQty: "8.95", Status: "calculated"}
	if order, _ := dollars.toDomain(); order.Status != domain.BrokerOrderFilled {
		t.Fatalf("filled dollar order mapped to %s", order.Status)
	}
	if order, _ := filled.toDomain(); order.Status != domain.BrokerOrderFilled {
		t.Fatalf("fully filled calculated order mapped to %s", order.Status)
	}
	if order, _ := partial.toDomain(); order.Status != domain.BrokerOrderExpired {
		t.Fatalf("partially filled calculated order mapped to %s", order.Status)
	}
}
