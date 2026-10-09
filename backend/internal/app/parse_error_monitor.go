package app

import (
	"sync"

	"fx-bot/backend/internal/port"
)

// ParseErrorMonitor tracks whether each symbol's most-recent advisor run was
// a FAILURE (parse_error / timeout / cli_error). cmd/bot wires it both to
// AdvisorCycle (via OnRunComplete) and to nextScheduleInterval (= read
// AnyRecentFailure to shorten cadence).
//
// When any symbol's last run failed, the scheduler picks
// ParseErrorRetryInterval (10 min) instead of the default. 対象は全失敗
// (parse_error / timeout / cli_error)。失敗は種類を問わず「次の判断が古いまま
// 放置される」リスクが同じ (cli_error 後に判断が更新されないと急変の初動を
// 逃す) なので、速い再走で空白を潰す。
//
// (型名は履歴経緯で ParseErrorMonitor のままだが、意味は「直近失敗 monitor」。)
type ParseErrorMonitor struct {
	mu    sync.Mutex
	state map[string]bool // symbol → last run failed (retryable)
}

// NewParseErrorMonitor returns a ready-to-use monitor with an empty state.
func NewParseErrorMonitor() *ParseErrorMonitor {
	return &ParseErrorMonitor{state: make(map[string]bool)}
}

// Record updates the per-symbol last-run state.
//   - success → flag cleared
//   - usageLimited (Claude session/usage limit) → flag cleared. The limit
//     resets hours later, so a 10-min fast-retry just hammers the exhausted
//     quota and amplifies consumption (repeated fires across the reset
//     window, all mislabeled "event"). Fall back to the default cadence instead.
//   - それ以外 (parse_error / timeout / 非 usage の cli_error) → flag set
//     (= 速い再走対象。stale config の再発防止)
func (m *ParseErrorMonitor) Record(symbol string, status port.AdvisorRunStatus, usageLimited bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if status == port.AdvisorRunStatusSuccess || usageLimited {
		delete(m.state, symbol)
	} else {
		m.state[symbol] = true
	}
}

// AnyRecentFailure reports whether any tracked symbol's last advisor run
// failed. Used by nextScheduleInterval to decide between default cadence and
// ParseErrorRetryInterval (fast retry).
func (m *ParseErrorMonitor) AnyRecentFailure() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.state) > 0
}
