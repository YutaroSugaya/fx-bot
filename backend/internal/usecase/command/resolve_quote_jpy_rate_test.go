package command

import (
	"context"
	"errors"
	"math"
	"testing"

	"fx-bot/backend/internal/domain/market"
)

// resolveQuoteJPYRate は close 時の quote→JPY 換算倍率を解決する。
//   - JPY quote (USD_JPY 等) → 1.0、broker は呼ばない (無駄な API を避ける)
//   - USD quote (EUR_USD 等) → broker から USD/JPY を引き、その mid を倍率に
//   - broker error → 伝播 (誤った損益を記録しないよう fail-close)
func TestResolveQuoteJPYRate_JPYQuoteSkipsBroker(t *testing.T) {
	b := &fakeBroker{}
	rate, err := resolveQuoteJPYRate(context.Background(), b, "USD_JPY")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if math.Abs(rate-1.0) > 1e-9 {
		t.Errorf("rate = %v, want 1.0", rate)
	}
	if b.tickerCalls != 0 {
		t.Errorf("broker GetTicker called %d times for JPY quote; want 0", b.tickerCalls)
	}
}

func TestResolveQuoteJPYRate_USDQuoteFetchesUSDJPY(t *testing.T) {
	b := &fakeBroker{ticker: &market.Ticker{Symbol: "USD_JPY", Bid: 157.0, Ask: 158.0}}
	rate, err := resolveQuoteJPYRate(context.Background(), b, "EUR_USD")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if math.Abs(rate-157.5) > 1e-9 { // mid of 157/158
		t.Errorf("rate = %v, want 157.5 (USD/JPY mid)", rate)
	}
	if b.tickerCalls != 1 {
		t.Errorf("broker GetTicker called %d times; want 1", b.tickerCalls)
	}
}

func TestResolveQuoteJPYRate_BrokerErrorPropagates(t *testing.T) {
	b := &fakeBroker{tickerErr: errors.New("boom")}
	_, err := resolveQuoteJPYRate(context.Background(), b, "EUR_USD")
	if err == nil {
		t.Fatal("expected error when broker GetTicker fails, got nil")
	}
}
