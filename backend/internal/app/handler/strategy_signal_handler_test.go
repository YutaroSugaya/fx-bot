package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"fx-bot/backend/internal/app/livesignal"
)

func TestStrategySignalHandler_RequiresSymbol(t *testing.T) {
	h := &StrategySignalHandler{Get: func(string) *livesignal.Snapshot { return nil }}
	rr := httptest.NewRecorder()
	h.Signal(rr, httptest.NewRequest(http.MethodGet, "/api/strategy/signal", nil))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d want 400", rr.Code)
	}
}

func TestStrategySignalHandler_NotFoundWhenNoSnapshot(t *testing.T) {
	h := &StrategySignalHandler{Get: func(string) *livesignal.Snapshot { return nil }}
	rr := httptest.NewRecorder()
	h.Signal(rr, httptest.NewRequest(http.MethodGet, "/api/strategy/signal?symbol=USD_JPY", nil))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status: got %d want 404", rr.Code)
	}
}

func TestStrategySignalHandler_ReturnsSnapshot(t *testing.T) {
	want := &livesignal.Snapshot{Symbol: "USD_JPY", Decision: "enter", Side: "SELL"}
	h := &StrategySignalHandler{Get: func(sym string) *livesignal.Snapshot {
		if sym != "USD_JPY" {
			return nil
		}
		return want
	}}
	rr := httptest.NewRecorder()
	h.Signal(rr, httptest.NewRequest(http.MethodGet, "/api/strategy/signal?symbol=USD_JPY", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200", rr.Code)
	}
	var got livesignal.Snapshot
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Decision != "enter" || got.Side != "SELL" {
		t.Errorf("body: %+v", got)
	}
}
