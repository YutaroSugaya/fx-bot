package command

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"fx-bot/backend/internal/backtest"
	"fx-bot/backend/internal/config"
	"fx-bot/backend/internal/domain/market"
	"fx-bot/backend/internal/port"
)

// logSink wraps a slog.Logger writing to an in-memory buffer so tests can
// assert that specific keywords (e.g. "spread_spike_observed") appear in
// the log output.
type logSink struct {
	mu     sync.Mutex
	buf    *bytes.Buffer
	logger *slog.Logger
}

func newLogSink() *logSink {
	buf := &bytes.Buffer{}
	s := &logSink{buf: buf}
	s.logger = slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return s
}

func (s *logSink) entries() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

func (s *logSink) contains(needle string) bool {
	return strings.Contains(s.entries(), needle)
}

// parse_error が連続 3 回起きたら notifier に Error-level alert を上げる。
// 連続 parse_error は dashboard だけでは気づきにくいため。
//
// 仕様:
//   - status=parse_error は streak++
//   - status=success は streak=0 (回復で reset)
//   - その他 (timeout / cli_error 等) も streak=0 (parse_error 限定の streak)
//   - streak >= 3 で LevelError, title "advisor_parse_error_streak" を 1 回通知

type recordingNotifier struct {
	mu     sync.Mutex
	events []port.Event
}

func (n *recordingNotifier) Notify(_ context.Context, e port.Event) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.events = append(n.events, e)
	return nil
}

func (n *recordingNotifier) byTitle(title string) []port.Event {
	n.mu.Lock()
	defer n.mu.Unlock()
	var out []port.Event
	for _, e := range n.events {
		if e.Title == title {
			out = append(out, e)
		}
	}
	return out
}

// statusAdvisor は status を sequence で返す stub。ParsedYAML は空。
type statusAdvisor struct {
	statuses []port.AdvisorRunStatus
	i        int
}

func (a *statusAdvisor) Generate(_ context.Context, _ *market.MarketSummary) (*port.AdvisorRun, error) {
	st := a.statuses[a.i]
	a.i++
	return &port.AdvisorRun{
		RunID:      "run-" + string(st),
		Status:     st,
		ParsedYAML: nil,
		StartedAt:  time.Now(),
		FinishedAt: time.Now(),
	}, nil
}

func newParseErrorStreakCycle(t *testing.T, advisor port.Advisor, notifier port.Notifier) *AdvisorCycle {
	t.Helper()
	strat := backtest.NewInMemoryStrategyConfigRepo()
	val := backtest.NewInMemoryValidationEventRepo()
	validator := config.NewValidator(validHardLimits())
	now := time.Date(2026, 5, 27, 16, 0, 0, 0, time.UTC)
	validator.Now = func() time.Time { return now }
	p := NewPromoter(validator, strat, val, nil, config.ModePaperConfig, "auto")
	p.Clock = func() time.Time { return now }
	p.ExpectedSymbol = "USD_JPY"
	return &AdvisorCycle{
		Symbol:       "USD_JPY",
		Advisor:      advisor,
		Promoter:     p,
		Notifier:     notifier,
		BotConfig:    &config.BotConfig{Bot: config.BotSection{Mode: config.ModePaperConfig}},
		Logger:       silentLogger(),
		BuildSummary: func() *market.MarketSummary { return &market.MarketSummary{} },
		GetAccountState: func() (config.AccountState, error) {
			return config.AccountState{MaxDailyLossJPY: 1000, MaxConsecutiveLosses: 3, MaxOpenPositions: 2}, nil
		},
	}
}

func TestAdvisorCycle_ParseErrorStreak_AlertsOnThird(t *testing.T) {
	adv := &statusAdvisor{statuses: []port.AdvisorRunStatus{
		port.AdvisorRunStatusParseError,
		port.AdvisorRunStatusParseError,
		port.AdvisorRunStatusParseError,
	}}
	notifier := &recordingNotifier{}
	cycle := newParseErrorStreakCycle(t, adv, notifier)

	for i := 0; i < 3; i++ {
		if _, err := cycle.Run(context.Background(), port.AdvisorRunSourceAuto); err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
	}
	alerts := notifier.byTitle("advisor_parse_error_streak")
	if len(alerts) != 1 {
		t.Fatalf("expected 1 streak alert after 3 parse_errors, got %d", len(alerts))
	}
	if alerts[0].Level != port.LevelError {
		t.Errorf("alert level: got %s want %s", alerts[0].Level, port.LevelError)
	}
}

func TestAdvisorCycle_ParseErrorStreak_NoAlertBeforeThird(t *testing.T) {
	adv := &statusAdvisor{statuses: []port.AdvisorRunStatus{
		port.AdvisorRunStatusParseError,
		port.AdvisorRunStatusParseError,
	}}
	notifier := &recordingNotifier{}
	cycle := newParseErrorStreakCycle(t, adv, notifier)

	for i := 0; i < 2; i++ {
		if _, err := cycle.Run(context.Background(), port.AdvisorRunSourceAuto); err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
	}
	if got := notifier.byTitle("advisor_parse_error_streak"); len(got) != 0 {
		t.Errorf("should NOT alert before 3 consecutive parse_errors, got %d", len(got))
	}
}

// 成功が間に挟まると streak は 0 にリセット。
func TestAdvisorCycle_ParseErrorStreak_SuccessResets(t *testing.T) {
	// PE, PE, Success, PE, PE → streak は最終的に 2 で alert 出ない
	yaml := validYAML("cfg-d4", time.Date(2026, 5, 27, 16, 0, 0, 0, time.UTC))
	adv := &sequenceAdvisor{steps: []sequenceStep{
		{status: port.AdvisorRunStatusParseError},
		{status: port.AdvisorRunStatusParseError},
		{status: port.AdvisorRunStatusSuccess, yaml: yaml},
		{status: port.AdvisorRunStatusParseError},
		{status: port.AdvisorRunStatusParseError},
	}}
	notifier := &recordingNotifier{}
	cycle := newParseErrorStreakCycle(t, adv, notifier)

	for i := 0; i < 5; i++ {
		if _, err := cycle.Run(context.Background(), port.AdvisorRunSourceAuto); err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
	}
	if got := notifier.byTitle("advisor_parse_error_streak"); len(got) != 0 {
		t.Errorf("success should reset streak; got %d alerts", len(got))
	}
}

// timeout / cli_error も streak をリセット (parse_error 限定の streak)。
func TestAdvisorCycle_ParseErrorStreak_OtherFailuresReset(t *testing.T) {
	adv := &statusAdvisor{statuses: []port.AdvisorRunStatus{
		port.AdvisorRunStatusParseError,
		port.AdvisorRunStatusParseError,
		port.AdvisorRunStatusTimeout, // 違う failure mode → reset
		port.AdvisorRunStatusParseError,
	}}
	notifier := &recordingNotifier{}
	cycle := newParseErrorStreakCycle(t, adv, notifier)

	for i := 0; i < 4; i++ {
		if _, err := cycle.Run(context.Background(), port.AdvisorRunSourceAuto); err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
	}
	if got := notifier.byTitle("advisor_parse_error_streak"); len(got) != 0 {
		t.Errorf("timeout should break parse_error streak; got %d alerts", len(got))
	}
}

// AdvisorCycle.OnRunComplete callback が run.Status を毎回受け取る。
// cmd/bot 側で app.ParseErrorMonitor.Record(symbol, status) を closure で
// 配線して、parse_error 中は scheduler を 10 分 cadence に切り替える。
func TestAdvisorCycle_OnRunComplete_FiresWithRunStatus(t *testing.T) {
	yaml := validYAML("cfg-d1", time.Date(2026, 5, 27, 16, 0, 0, 0, time.UTC))
	adv := &sequenceAdvisor{steps: []sequenceStep{
		{status: port.AdvisorRunStatusParseError},
		{status: port.AdvisorRunStatusSuccess, yaml: yaml},
		{status: port.AdvisorRunStatusTimeout},
	}}
	cycle := newParseErrorStreakCycle(t, adv, &recordingNotifier{})

	var observed []port.AdvisorRunStatus
	cycle.OnRunComplete = func(s port.AdvisorRunStatus, _ bool) {
		observed = append(observed, s)
	}
	for i := 0; i < 3; i++ {
		if _, err := cycle.Run(context.Background(), port.AdvisorRunSourceAuto); err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
	}
	want := []port.AdvisorRunStatus{
		port.AdvisorRunStatusParseError,
		port.AdvisorRunStatusSuccess,
		port.AdvisorRunStatusTimeout,
	}
	if len(observed) != len(want) {
		t.Fatalf("observed count: got %d want %d", len(observed), len(want))
	}
	for i := range want {
		if observed[i] != want[i] {
			t.Errorf("observed[%d]: got %s want %s", i, observed[i], want[i])
		}
	}
}

// sequenceAdvisor は step ごとに status と (Success 時) YAML を返す stub。
type sequenceStep struct {
	status port.AdvisorRunStatus
	yaml   []byte
}

type sequenceAdvisor struct {
	steps []sequenceStep
	i     int
}

func (a *sequenceAdvisor) Generate(_ context.Context, _ *market.MarketSummary) (*port.AdvisorRun, error) {
	s := a.steps[a.i]
	a.i++
	return &port.AdvisorRun{
		RunID:      "run-seq",
		Status:     s.status,
		ParsedYAML: s.yaml,
		StartedAt:  time.Now(),
		FinishedAt: time.Now(),
	}, nil
}

// スプレッド 2 倍以上のとき audit log を
// 出す。BuildSummary 時点で spike が観測されたら必ず Warn を 1 件、Claude が
// それでも enabled config を返したら更に Error を 1 件 (= 通り抜け疑い)。
func TestAdvisorCycle_SpreadSpike_ObservedLog(t *testing.T) {
	yaml := validYAML("cfg-spike", time.Date(2026, 5, 27, 16, 0, 0, 0, time.UTC))
	adv := &sequenceAdvisor{steps: []sequenceStep{
		{status: port.AdvisorRunStatusSuccess, yaml: yaml},
	}}
	notifier := &recordingNotifier{}
	cycle := newParseErrorStreakCycle(t, adv, notifier)
	cycle.BuildSummary = func() *market.MarketSummary {
		// current=1.5pips, 1h avg=0.5pips → 3x = spike
		return &market.MarketSummary{
			Symbol:      "USD_JPY",
			CurrentRate: market.CurrentRate{SpreadPips: 1.5},
			Summary1h:   market.WindowSummary{AvgSpreadPips: 0.5},
		}
	}
	// AdvisorCycle.Logger をキャプチャ用 sink に差し替える。
	sink := newLogSink()
	cycle.Logger = sink.logger

	if _, err := cycle.Run(context.Background(), port.AdvisorRunSourceAuto); err != nil {
		t.Fatalf("run: %v", err)
	}
	if !sink.contains("spread_spike_observed") {
		t.Errorf("spike observed log missing; got entries: %v", sink.entries())
	}
}

// spike なし → 観測 log は出ない (false positive 防止)。
func TestAdvisorCycle_NoSpreadSpike_NoLog(t *testing.T) {
	yaml := validYAML("cfg-normal", time.Date(2026, 5, 27, 16, 0, 0, 0, time.UTC))
	adv := &sequenceAdvisor{steps: []sequenceStep{
		{status: port.AdvisorRunStatusSuccess, yaml: yaml},
	}}
	cycle := newParseErrorStreakCycle(t, adv, &recordingNotifier{})
	cycle.BuildSummary = func() *market.MarketSummary {
		return &market.MarketSummary{
			Symbol:      "USD_JPY",
			CurrentRate: market.CurrentRate{SpreadPips: 0.5},
			Summary1h:   market.WindowSummary{AvgSpreadPips: 0.5},
		}
	}
	sink := newLogSink()
	cycle.Logger = sink.logger

	if _, err := cycle.Run(context.Background(), port.AdvisorRunSourceAuto); err != nil {
		t.Fatalf("run: %v", err)
	}
	if sink.contains("spread_spike_observed") {
		t.Errorf("normal spread must not log spike: %v", sink.entries())
	}
}
