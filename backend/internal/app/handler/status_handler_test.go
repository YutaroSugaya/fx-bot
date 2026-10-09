package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"fx-bot/backend/internal/backtest"
	"fx-bot/backend/internal/config"
	"fx-bot/backend/internal/port"
	"fx-bot/backend/internal/usecase/query"
)

// positionsWith seeds an InMemoryPositionRepo (古典派 §4).
func positionsWith(recs ...port.PositionRecord) *backtest.InMemoryPositionRepo {
	repo := backtest.NewInMemoryPositionRepo()
	for i := range recs {
		_, _ = repo.Insert(context.Background(), port.PositionInsertInput{Position: recs[i]})
	}
	return repo
}

func botCfgFor(symbol string, mode config.Mode) *config.BotConfig {
	c := &config.BotConfig{Symbol: symbol}
	c.Bot.Mode = mode
	return c
}

func mkStatusQuery(t *testing.T, cfg *config.BotConfig, repo port.PositionRepository,
	active *config.StrategyConfig, emergency bool, tickerErrs int64) *query.GetBotStatusQuery {
	t.Helper()
	return &query.GetBotStatusQuery{
		BotConfig:       cfg,
		Positions:       repo,
		GetActiveConfig: func() *config.StrategyConfig { return active },
		EmergencyActive: func() bool { return emergency },
		TickerErrors:    func() int64 { return tickerErrs },
		StartedAt:       time.Now().Add(-time.Minute),
	}
}

func TestStatusHandler_Healthz_Returns200WithJSON(t *testing.T) {
	h := &StatusHandler{}
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	h.Healthz(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("status: got %d want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("content-type: got %q", ct)
	}
	var body map[string]string
	_ = json.NewDecoder(rec.Body).Decode(&body)
	if body["status"] != "ok" {
		t.Errorf("body: got %v", body)
	}
}

func TestStatusHandler_Status(t *testing.T) {
	activeCfg := &config.StrategyConfig{ConfigID: "cfg-1", Enabled: true}
	activeCfg.Strategy.Name = "momentum_pullback"

	cases := []struct {
		name              string
		pos               *backtest.InMemoryPositionRepo
		active            *config.StrategyConfig
		emergency         bool
		tickerErrs        int64
		wantMode          string
		wantOpenPositions int
		wantEmergency     bool
		wantTickerErrors  int
		wantConfigID      string
		wantStrategy      string
	}{
		{
			name:              "basic state, no active config",
			pos:               positionsWith(port.PositionRecord{Symbol: "USD_JPY", Status: port.PositionStatusOpen}),
			active:            nil,
			emergency:         false,
			tickerErrs:        7,
			wantMode:          string(config.ModePaperConfig),
			wantOpenPositions: 1,
			wantTickerErrors:  7,
		},
		{
			name:              "active config populates extra fields",
			pos:               backtest.NewInMemoryPositionRepo(),
			active:            activeCfg,
			emergency:         false,
			wantMode:          string(config.ModePaperConfig),
			wantOpenPositions: 0,
			wantConfigID:      "cfg-1",
			wantStrategy:      "momentum_pullback",
		},
		{
			name:              "emergency_stop reflects hook",
			pos:               backtest.NewInMemoryPositionRepo(),
			active:            nil,
			emergency:         true,
			wantMode:          string(config.ModePaperConfig),
			wantOpenPositions: 0,
			wantEmergency:     true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q := mkStatusQuery(t, botCfgFor("USD_JPY", config.ModePaperConfig), tc.pos, tc.active, tc.emergency, tc.tickerErrs)
			h := &StatusHandler{StatusQuery: q}
			rec := httptest.NewRecorder()
			h.Status(rec, httptest.NewRequest(http.MethodGet, "/api/status", nil))

			if rec.Code != http.StatusOK {
				t.Fatalf("status: %d", rec.Code)
			}
			var body map[string]any
			_ = json.NewDecoder(rec.Body).Decode(&body)
			if body["mode"] != tc.wantMode {
				t.Errorf("mode: got %v want %v", body["mode"], tc.wantMode)
			}
			if v, _ := body["open_positions"].(float64); int(v) != tc.wantOpenPositions {
				t.Errorf("open_positions: got %v want %d", body["open_positions"], tc.wantOpenPositions)
			}
			if got, _ := body["emergency_stop"].(bool); got != tc.wantEmergency {
				t.Errorf("emergency_stop: got %v want %v", got, tc.wantEmergency)
			}
			if tc.wantTickerErrors != 0 {
				if v, _ := body["ticker_errors"].(float64); int(v) != tc.wantTickerErrors {
					t.Errorf("ticker_errors: got %v want %d", body["ticker_errors"], tc.wantTickerErrors)
				}
			}
			if tc.wantConfigID != "" && body["active_config_id"] != tc.wantConfigID {
				t.Errorf("active_config_id: got %v want %s", body["active_config_id"], tc.wantConfigID)
			}
			if tc.wantStrategy != "" && body["strategy"] != tc.wantStrategy {
				t.Errorf("strategy: got %v want %s", body["strategy"], tc.wantStrategy)
			}
		})
	}
}

// Dashboard 拡張: /api/status の 4 つの観察用メトリクス
// (24h PnL / 累積 reject 件数 / 早期 exit 発火数 / advisor duration_ms) が
// fn-typed 依存性経由で正しく BotStatusView に乗ることを pin する。
// fn が nil の場合は省略 (omitempty) されることも確認。
func TestStatusHandler_Status_DashboardExtension(t *testing.T) {
	q := &query.GetBotStatusQuery{
		BotConfig:       botCfgFor("USD_JPY", config.ModePaperConfig),
		Positions:       backtest.NewInMemoryPositionRepo(),
		GetActiveConfig: func() *config.StrategyConfig { return nil },
		EmergencyActive: func() bool { return false },
		StartedAt:       time.Now().Add(-time.Minute),
		// New fn-types
		Pnl24hJpyFn:             func(ctx context.Context, symbol string) (float64, error) { return 1234.5, nil },
		RejectCount24hFn:        func(ctx context.Context) (int, error) { return 7, nil },
		EarlyExitCount24hFn:     func(ctx context.Context, symbol string) (int, error) { return 2, nil },
		LastAdvisorDurationMsFn: func(ctx context.Context) (int, error) { return 4200, nil },
	}
	h := &StatusHandler{StatusQuery: q}
	rec := httptest.NewRecorder()
	h.Status(rec, httptest.NewRequest(http.MethodGet, "/api/status", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d", rec.Code)
	}
	var body map[string]any
	_ = json.NewDecoder(rec.Body).Decode(&body)
	if v, _ := body["pnl_24h_jpy"].(float64); v != 1234.5 {
		t.Errorf("pnl_24h_jpy: got %v want 1234.5", body["pnl_24h_jpy"])
	}
	if v, _ := body["reject_count_24h"].(float64); int(v) != 7 {
		t.Errorf("reject_count_24h: got %v want 7", body["reject_count_24h"])
	}
	if v, _ := body["early_exit_count_24h"].(float64); int(v) != 2 {
		t.Errorf("early_exit_count_24h: got %v want 2", body["early_exit_count_24h"])
	}
	if v, _ := body["last_advisor_duration_ms"].(float64); int(v) != 4200 {
		t.Errorf("last_advisor_duration_ms: got %v want 4200", body["last_advisor_duration_ms"])
	}
}

// nil fn は omitempty で省略されることを確認 (= 後方互換: 旧 wiring でも壊れない)。
func TestStatusHandler_Status_DashboardExtension_NilFnsOmitted(t *testing.T) {
	q := &query.GetBotStatusQuery{
		BotConfig:       botCfgFor("USD_JPY", config.ModePaperConfig),
		Positions:       backtest.NewInMemoryPositionRepo(),
		GetActiveConfig: func() *config.StrategyConfig { return nil },
		EmergencyActive: func() bool { return false },
		StartedAt:       time.Now().Add(-time.Minute),
		// All new fn-types intentionally nil
	}
	h := &StatusHandler{StatusQuery: q}
	rec := httptest.NewRecorder()
	h.Status(rec, httptest.NewRequest(http.MethodGet, "/api/status", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d", rec.Code)
	}
	var body map[string]any
	_ = json.NewDecoder(rec.Body).Decode(&body)
	for _, key := range []string{"pnl_24h_jpy", "reject_count_24h", "early_exit_count_24h", "last_advisor_duration_ms"} {
		if _, ok := body[key]; ok {
			t.Errorf("nil fn must omit key %q from JSON, got %v", key, body[key])
		}
	}
}

func TestStatusHandler_Status_NilQuery_Returns503(t *testing.T) {
	h := &StatusHandler{}
	rec := httptest.NewRecorder()
	h.Status(rec, httptest.NewRequest(http.MethodGet, "/api/status", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status: %d want 503", rec.Code)
	}
}

// ActiveConfig endpoint shape:
//   - no holder wired → 404
//   - empty holder    → 404
//   - non-empty       → 200 with { configs: { sym: cfg } } over every entry
//
// Per-symbol query and unknown-symbol behaviour live in status_handler_multisymbol_test.go.
func TestStatusHandler_ActiveConfig(t *testing.T) {
	cases := []struct {
		name       string
		getConfigs func() map[string]*config.StrategyConfig
		wantStatus int
		wantSymbol string
		wantCfgID  string
	}{
		{"holder nil returns 404", nil, http.StatusNotFound, "", ""},
		{"empty holder returns 404", func() map[string]*config.StrategyConfig { return nil }, http.StatusNotFound, "", ""},
		{
			"returns configs map for one symbol",
			func() map[string]*config.StrategyConfig {
				return map[string]*config.StrategyConfig{"USD_JPY": {ConfigID: "cfg-2"}}
			},
			http.StatusOK, "USD_JPY", "cfg-2",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := &StatusHandler{GetActiveConfigs: tc.getConfigs}
			rec := httptest.NewRecorder()
			h.ActiveConfig(rec, httptest.NewRequest(http.MethodGet, "/api/active-config", nil))
			if rec.Code != tc.wantStatus {
				t.Errorf("status: %d want %d", rec.Code, tc.wantStatus)
			}
			if tc.wantStatus == http.StatusOK {
				var body struct {
					Configs map[string]struct {
						ConfigID string `json:"ConfigID"`
					} `json:"configs"`
				}
				_ = json.NewDecoder(rec.Body).Decode(&body)
				if body.Configs[tc.wantSymbol].ConfigID != tc.wantCfgID {
					t.Errorf("configs[%s]: got %v want %s", tc.wantSymbol, body.Configs, tc.wantCfgID)
				}
			}
		})
	}
}
