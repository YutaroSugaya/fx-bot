package repository

import (
	"strings"
	"testing"
)

// SafeBacktestDSN mirrors SafeIntegrationTestDSN: the offline backfill/backtest DB must NEVER
// be the live money DB. Two walls — must differ from DATABASE_URL, and the db name must end
// in "_backtest". This is the last line of defence before histdata-ingest writes ~15M rows.
func TestSafeBacktestDSN(t *testing.T) {
	const live = "postgres://u:p@localhost:5432/fxbot?sslmode=disable"
	cases := []struct {
		name    string
		dsn     string
		live    string
		wantErr bool
	}{
		{"empty", "", live, true},
		{"equals live dsn", live, live, true},
		{"non _backtest name", "postgres://u:p@localhost:5432/fxbot?sslmode=disable", "", true},
		{"live name without suffix", "postgres://u:p@localhost:5432/fxbot", live, true},
		{"valid backtest db", "postgres://u:p@localhost:5432/fxbot_backtest?sslmode=disable", live, false},
		{"valid backtest db no live set", "postgres://u:p@localhost:5432/fxbot_backtest", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := SafeBacktestDSN(tc.dsn, tc.live)
			if (err != nil) != tc.wantErr {
				t.Errorf("SafeBacktestDSN(%q,%q) err=%v, wantErr=%v", tc.dsn, tc.live, err, tc.wantErr)
			}
		})
	}
}

// The rejection error is printed to stderr / logs, so it must never echo the DSN's password.
func TestSafeBacktestDSN_ErrorDoesNotLeakPassword(t *testing.T) {
	const secret = "s3cr3t-pw"
	err := SafeBacktestDSN("postgres://u:"+secret+"@h:5432/fxbot?sslmode=disable", "")
	if err == nil {
		t.Fatal("want rejection for a non-_backtest DSN")
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("error message leaks the DSN password: %v", err)
	}
}
