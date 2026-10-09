package broker

import "testing"

// TestPriceDecimalsFor pins the per-symbol decimal precision GMO Forex
// requires in order prices. JPY-quote pairs use 3 (tickSize 0.001);
// USD-quote pairs (EUR_USD, GBP_USD) are quoted to 1/10 pip = 0.00001 = 5
// decimals. Sending 3 decimals for a USD-quote pair trips GMO's ERR-5114
// "Decimal digits of size is invalid", so live OCO/orders would be rejected.
func TestPriceDecimalsFor(t *testing.T) {
	cases := map[string]int{
		"USD_JPY": 3,
		"EUR_JPY": 3,
		"GBP_JPY": 3,
		"AUD_JPY": 3,
		"EUR_USD": 5,
		"GBP_USD": 5,
	}
	for sym, want := range cases {
		if got := priceDecimalsFor(sym); got != want {
			t.Errorf("priceDecimalsFor(%q) = %d, want %d", sym, got, want)
		}
	}
}

// TestFormatPrice_USDQuote proves a USD-quote price serializes with 5
// fractional digits (no float round-off artifact, trailing zeros kept) so
// GMO accepts it.
func TestFormatPrice_USDQuote(t *testing.T) {
	cases := []struct {
		price  float64
		symbol string
		want   string
	}{
		{1.08123, "EUR_USD", "1.08123"},
		{1.085, "EUR_USD", "1.08500"},
		{1.26543, "GBP_USD", "1.26543"},
		{145.123, "USD_JPY", "145.123"},
	}
	for _, tc := range cases {
		if got := formatPrice(tc.price, tc.symbol); got != tc.want {
			t.Errorf("formatPrice(%v, %q) = %q, want %q", tc.price, tc.symbol, got, tc.want)
		}
	}
}
