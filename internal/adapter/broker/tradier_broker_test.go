package broker

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestTradierGetAccountUsesAccountTypeBuyingPower(t *testing.T) {
	tests := []struct {
		name            string
		balances        string
		wantCash        float64
		wantBuyingPower float64
	}{
		{
			name: "margin uses stock buying power",
			balances: `{
				"account_type":"margin",
				"total_cash":2500.25,
				"margin":{"stock_buying_power":12727.72}
			}`,
			wantCash: 2500.25, wantBuyingPower: 12727.72,
		},
		{
			name: "PDT uses PDT stock buying power",
			balances: `{
				"account_type":"pdt",
				"total_cash":4000,
				"pdt":{"stock_buying_power":18000}
			}`,
			wantCash: 4000, wantBuyingPower: 18000,
		},
		{
			name: "cash uses available cash",
			balances: `{
				"account_type":"cash",
				"total_cash":5653.38,
				"cash":{"cash_available":4343.38,"unsettled_funds":1310}
			}`,
			wantCash: 5653.38, wantBuyingPower: 4343.38,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.Path != "/accounts/VA000001/balances" {
					t.Fatalf("unexpected request path %q", r.URL.Path)
				}
				if got := r.Header.Get("Authorization"); got != "Bearer token" {
					t.Fatalf("Authorization=%q, want Bearer token", got)
				}
				return jsonResponse(fmt.Sprintf(`{"balances":%s}`, tt.balances)), nil
			})}

			broker := &TradierBroker{
				httpClient: client, token: "token", accountID: "VA000001", baseURL: "https://tradier.test",
			}
			account, err := broker.GetAccount(context.Background())
			if err != nil {
				t.Fatalf("GetAccount() error: %v", err)
			}
			if account.AccountID != "VA000001" || account.Cash != tt.wantCash || account.BuyingPower != tt.wantBuyingPower {
				t.Fatalf("GetAccount()=%+v, want cash %.2f and buying power %.2f", account, tt.wantCash, tt.wantBuyingPower)
			}
		})
	}
}

func TestTradierGetAccountRejectsMissingAccountTypeBranch(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		return jsonResponse(`{"balances":{"account_type":"margin","total_cash":1000}}`), nil
	})}
	broker := &TradierBroker{httpClient: client, accountID: "VA000001", baseURL: "https://tradier.test"}
	if _, err := broker.GetAccount(context.Background()); err == nil {
		t.Fatal("GetAccount() accepted a margin response without margin balances")
	}
}

func jsonResponse(body string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}
