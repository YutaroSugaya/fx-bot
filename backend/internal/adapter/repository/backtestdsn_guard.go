package repository

import (
	"fmt"
	"strings"
)

// SafeBacktestDSN validates that an offline backfill/backtest DSN can NEVER point at the live
// money DB — the same defence as SafeIntegrationTestDSN, applied to the histdata-ingest path
// that loads ~15M pre-2023 history rows. The 2015-2023 dataset MUST land in an isolated
// fxbot_backtest DB (CLAUDE.md: "live DB は read-only"), not the live candles table the
// running bot queries every tick.
//
// Two independent walls, either failing is fatal:
//  1. it must differ from DATABASE_URL (the live DSN), when that is set;
//  2. the DSN's database name must end in "_backtest" (naming convention).
func SafeBacktestDSN(targetDSN, databaseURL string) error {
	if targetDSN == "" {
		return fmt.Errorf("backtest DSN is empty")
	}
	if databaseURL != "" && targetDSN == databaseURL {
		return fmt.Errorf("backtest DSN must differ from DATABASE_URL — refusing to write history into the live DB")
	}
	db := dbNameFromDSN(targetDSN)
	if !strings.HasSuffix(db, "_backtest") {
		// The DSN itself is not echoed: it may carry a password.
		return fmt.Errorf("backtest DSN database name %q must end in \"_backtest\" — refusing to write into a non-_backtest DB", db)
	}
	return nil
}
