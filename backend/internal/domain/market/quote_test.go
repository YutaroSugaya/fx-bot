package market

import (
	"math"
	"testing"
)

func TestQuoteCurrency(t *testing.T) {
	tests := []struct {
		symbol string
		want   string
	}{
		{"USD_JPY", "JPY"},
		{"EUR_JPY", "JPY"},
		{"GBP_JPY", "JPY"},
		{"AUD_JPY", "JPY"},
		{"EUR_USD", "USD"},
		{"GBP_USD", "USD"},
		{"AUD_USD", "USD"},
		{"NZD_USD", "USD"},
		// 不正フォーマット (アンダースコア無し) は空文字。
		{"EURUSD", ""},
		{"", ""},
	}
	for _, tc := range tests {
		t.Run(tc.symbol, func(t *testing.T) {
			if got := QuoteCurrency(tc.symbol); got != tc.want {
				t.Errorf("QuoteCurrency(%q) = %q, want %q", tc.symbol, got, tc.want)
			}
		})
	}
}

func TestPipSize_USDQuotePairs(t *testing.T) {
	// EUR_USD など USD-quote / 4-decimal ペアは 0.0001。
	for _, sym := range []string{"EUR_USD", "GBP_USD", "AUD_USD", "NZD_USD"} {
		if got := PipSize(sym); math.Abs(got-0.0001) > 1e-12 {
			t.Errorf("PipSize(%q) = %v, want 0.0001", sym, got)
		}
	}
	// JPY-quote は 0.01 のまま (回帰防止)。
	for _, sym := range []string{"USD_JPY", "EUR_JPY", "GBP_JPY", "AUD_JPY"} {
		if got := PipSize(sym); math.Abs(got-0.01) > 1e-12 {
			t.Errorf("PipSize(%q) = %v, want 0.01", sym, got)
		}
	}
}

func TestQuoteJPYRate(t *testing.T) {
	tests := []struct {
		name       string
		symbol     string
		usdJPYRate float64
		want       float64
		wantErr    bool
	}{
		{
			name:   "JPY quote は usdJPYRate を無視して 1.0",
			symbol: "USD_JPY", usdJPYRate: 0, want: 1.0,
		},
		{
			name:   "JPY cross も 1.0",
			symbol: "GBP_JPY", usdJPYRate: 190.0, want: 1.0,
		},
		{
			name:   "USD quote は usdJPYRate をそのまま倍率に",
			symbol: "EUR_USD", usdJPYRate: 157.5, want: 157.5,
		},
		{
			name:   "USD quote で usdJPYRate 欠損 (<=0) は error",
			symbol: "EUR_USD", usdJPYRate: 0, wantErr: true,
		},
		{
			name:   "USD quote で負レートは error",
			symbol: "GBP_USD", usdJPYRate: -1, wantErr: true,
		},
		{
			name:   "未対応 quote (JPY/USD 以外) は error",
			symbol: "EUR_GBP", usdJPYRate: 157.5, wantErr: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := QuoteJPYRate(tc.symbol, tc.usdJPYRate)
			if (err != nil) != tc.wantErr {
				t.Fatalf("QuoteJPYRate(%q,%v) err=%v wantErr=%v", tc.symbol, tc.usdJPYRate, err, tc.wantErr)
			}
			if tc.wantErr {
				return
			}
			if math.Abs(got-tc.want) > 1e-9 {
				t.Errorf("QuoteJPYRate(%q,%v) = %v, want %v", tc.symbol, tc.usdJPYRate, got, tc.want)
			}
		})
	}
}
