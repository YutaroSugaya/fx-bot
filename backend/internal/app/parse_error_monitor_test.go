package app

import (
	"testing"

	"fx-bot/backend/internal/port"
)

// ParseErrorMonitor は per-symbol で「最新 advisor run が失敗 (parse_error /
// timeout / cli_error) だったか」を保持する。
// cmd/bot の nextScheduleInterval がこれを読んで「失敗中なら 10 分後再走」する。
// parse_error 限定だと cli_error 後に長時間判断が更新されず初動を逃すため、
// 対象は全失敗。
func TestParseErrorMonitor_RecordsAndReportsFailureState(t *testing.T) {
	m := NewParseErrorMonitor()
	if m.AnyRecentFailure() {
		t.Fatal("empty monitor should report no failure")
	}
	m.Record("USD_JPY", port.AdvisorRunStatusParseError, false)
	if !m.AnyRecentFailure() {
		t.Error("after ParseError record, AnyRecentFailure should be true")
	}
}

func TestParseErrorMonitor_SuccessClearsSymbol(t *testing.T) {
	m := NewParseErrorMonitor()
	m.Record("USD_JPY", port.AdvisorRunStatusParseError, false)
	m.Record("USD_JPY", port.AdvisorRunStatusSuccess, false)
	if m.AnyRecentFailure() {
		t.Error("Success record should clear the symbol's failure flag")
	}
}

// Per-symbol: USD_JPY 失敗の間に EUR_JPY が success しても、
// USD_JPY の flag は残る (= まだ短い cadence が必要)。
func TestParseErrorMonitor_PerSymbolIsolation(t *testing.T) {
	m := NewParseErrorMonitor()
	m.Record("USD_JPY", port.AdvisorRunStatusParseError, false)
	m.Record("EUR_JPY", port.AdvisorRunStatusSuccess, false)
	if !m.AnyRecentFailure() {
		t.Error("EUR_JPY success must not clear USD_JPY failure flag")
	}
}

// timeout / cli_error も「失敗」として flag を立て、10 分後再走の
// 対象にする (flag をクリアしない)。
func TestParseErrorMonitor_TimeoutAndCLIErrorSetFlag(t *testing.T) {
	for _, st := range []port.AdvisorRunStatus{
		port.AdvisorRunStatusTimeout,
		port.AdvisorRunStatusCLIError,
	} {
		m := NewParseErrorMonitor()
		m.Record("USD_JPY", st, false)
		if !m.AnyRecentFailure() {
			t.Errorf("status %q should set the failure flag (fast retry)", st)
		}
	}
}

// A usage/session limit is a cli_error, but it resets HOURS later —
// fast-retrying every 10 min just hammers the exhausted quota (dozens of
// fires across the window, all mislabeled "event"). usageLimited=true must
// NOT set the fast-retry flag, so the scheduler falls back to default cadence.
func TestParseErrorMonitor_UsageLimitDoesNotSetFlag(t *testing.T) {
	m := NewParseErrorMonitor()
	m.Record("USD_JPY", port.AdvisorRunStatusCLIError, true)
	if m.AnyRecentFailure() {
		t.Error("usage-limited cli_error must NOT set the fast-retry flag")
	}
}

// A prior real failure followed by a usage-limited run should CLEAR the flag:
// once we know the CLI is rate-limited, fast-retry is pointless until reset.
func TestParseErrorMonitor_UsageLimitClearsPriorFailure(t *testing.T) {
	m := NewParseErrorMonitor()
	m.Record("USD_JPY", port.AdvisorRunStatusTimeout, false)
	m.Record("USD_JPY", port.AdvisorRunStatusCLIError, true)
	if m.AnyRecentFailure() {
		t.Error("usage-limited run must clear a prior failure flag (no fast-retry until reset)")
	}
}
