package histdata

import (
	"path/filepath"
	"strings"
)

// histDataPairToDBSymbol maps HistData's 6-letter pair token to this project's DB symbol,
// restricted to the 5 supported pairs. Anything else is rejected so a stray file (e.g. AUDUSD)
// can never be ingested under a wrong/absent symbol.
var histDataPairToDBSymbol = map[string]string{
	"USDJPY": "USD_JPY",
	"EURUSD": "EUR_USD",
	"EURJPY": "EUR_JPY",
	"GBPJPY": "GBP_JPY",
	"GBPUSD": "GBP_USD",
}

// SymbolFromFilename extracts the DB symbol from a HistData filename such as
// "DAT_ASCII_USDJPY_M1_2020.csv" or ".../DAT_ASCII_gbpjpy_m1_201501.csv". Returns ok=false
// for unknown pairs or unparseable names.
func SymbolFromFilename(path string) (string, bool) {
	base := strings.ToUpper(filepath.Base(path))
	for token, dbSym := range histDataPairToDBSymbol {
		if strings.Contains(base, token) {
			return dbSym, true
		}
	}
	return "", false
}
