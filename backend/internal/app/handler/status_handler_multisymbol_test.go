package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"fx-bot/backend/internal/config"
)

// --- /api/active-config multi-symbol ---
//
// Multi-symbol contract:
//   - no query           → JSON map { configs: { "USD_JPY": {...}, "EUR_JPY": {...} } }
//   - ?symbol=USD_JPY    → JSON single config (back-compat shape)
//   - ?symbol=unknown    → 404
//   - empty holder       → 404 for any shape

func TestStatusHandler_ActiveConfig_NoQueryReturnsAllSymbols(t *testing.T) {
	configs := map[string]*config.StrategyConfig{
		"USD_JPY": {ConfigID: "u-1"},
		"EUR_JPY": {ConfigID: "e-1"},
	}
	h := &StatusHandler{GetActiveConfigs: func() map[string]*config.StrategyConfig { return configs }}
	rec := httptest.NewRecorder()
	h.ActiveConfig(rec, httptest.NewRequest(http.MethodGet, "/api/active-config", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d, body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Configs map[string]struct {
			ConfigID string `json:"ConfigID"`
		} `json:"configs"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Configs["USD_JPY"].ConfigID != "u-1" || body.Configs["EUR_JPY"].ConfigID != "e-1" {
		t.Errorf("configs map: %+v", body.Configs)
	}
}

func TestStatusHandler_ActiveConfig_SymbolQueryReturnsSingleConfig(t *testing.T) {
	configs := map[string]*config.StrategyConfig{
		"USD_JPY": {ConfigID: "u-1"},
		"EUR_JPY": {ConfigID: "e-1"},
	}
	h := &StatusHandler{GetActiveConfigs: func() map[string]*config.StrategyConfig { return configs }}
	rec := httptest.NewRecorder()
	h.ActiveConfig(rec, httptest.NewRequest(http.MethodGet, "/api/active-config?symbol=EUR_JPY", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d", rec.Code)
	}
	var body map[string]any
	_ = json.NewDecoder(rec.Body).Decode(&body)
	if body["ConfigID"] != "e-1" {
		t.Errorf("single config response: got %v want e-1", body["ConfigID"])
	}
}

func TestStatusHandler_ActiveConfig_UnknownSymbolReturns404(t *testing.T) {
	configs := map[string]*config.StrategyConfig{"USD_JPY": {ConfigID: "u-1"}}
	h := &StatusHandler{GetActiveConfigs: func() map[string]*config.StrategyConfig { return configs }}
	rec := httptest.NewRecorder()
	h.ActiveConfig(rec, httptest.NewRequest(http.MethodGet, "/api/active-config?symbol=GBP_JPY", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("status: %d want 404", rec.Code)
	}
}

func TestStatusHandler_ActiveConfig_EmptyHolderReturns404(t *testing.T) {
	h := &StatusHandler{GetActiveConfigs: func() map[string]*config.StrategyConfig { return map[string]*config.StrategyConfig{} }}
	rec := httptest.NewRecorder()
	h.ActiveConfig(rec, httptest.NewRequest(http.MethodGet, "/api/active-config", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("status: %d want 404", rec.Code)
	}
}
