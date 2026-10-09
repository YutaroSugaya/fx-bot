package histdata

import "testing"

// HistData names files like DAT_ASCII_USDJPY_M1_2020.csv (yearly) or ..._M1_201501.csv (monthly).
// We map the 6-letter pair token to this project's DB symbol, restricted to the 5 supported pairs —
// an unexpected pair must be rejected, not silently ingested under a wrong symbol.
func TestSymbolFromFilename(t *testing.T) {
	cases := []struct {
		file   string
		want   string
		wantOK bool
	}{
		{"DAT_ASCII_USDJPY_M1_2020.csv", "USD_JPY", true},
		{"DAT_ASCII_EURUSD_M1_2018.csv", "EUR_USD", true},
		{"DAT_ASCII_EURJPY_M1_201501.csv", "EUR_JPY", true},
		{"DAT_ASCII_GBPJPY_M1_2022.csv", "GBP_JPY", true},
		{"DAT_ASCII_GBPUSD_M1_2016.csv", "GBP_USD", true},
		{"/some/path/DAT_ASCII_usdjpy_m1_2020.csv", "USD_JPY", true}, // path + lowercase
		{"DAT_ASCII_AUDUSD_M1_2020.csv", "", false},                  // not one of the 5 supported pairs
		{"unrelated.csv", "", false},
		{"", "", false},
	}
	for _, tc := range cases {
		got, ok := SymbolFromFilename(tc.file)
		if got != tc.want || ok != tc.wantOK {
			t.Errorf("SymbolFromFilename(%q) = (%q,%v), want (%q,%v)", tc.file, got, ok, tc.want, tc.wantOK)
		}
	}
}
