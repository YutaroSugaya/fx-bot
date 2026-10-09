package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"fx-bot/backend/internal/backtest"
	"fx-bot/backend/internal/domain/clock"
	"fx-bot/backend/internal/port"
	"fx-bot/backend/internal/usecase/command"
)

// seedOpen は ExtendMaxHold ハンドラテスト用に OPEN position を 1 件作る。
func seedOpen(t *testing.T, repo *backtest.InMemoryPositionRepo, openedAt time.Time, maxHold int) int64 {
	t.Helper()
	id, err := repo.Insert(context.Background(), port.PositionInsertInput{
		Position: port.PositionRecord{
			Symbol: "USD_JPY", Side: "BUY", Quantity: 1000, EntryPrice: 159.996,
			TakeProfitPips: 20, StopLossPips: 15, MaxHoldMinutes: maxHold,
			StrategyConfigID: "cfg-test", Status: port.PositionStatusOpen, OpenedAt: openedAt,
		},
		Manual: true,
	})
	if err != nil {
		t.Fatalf("seed insert: %v", err)
	}
	return id
}

func mkExtendHandler(repo port.PositionRepository, now time.Time) *PositionsHandler {
	return &PositionsHandler{
		ExtendCommand: &command.ExtendMaxHoldCommand{Positions: repo, Clock: clock.NewFake(now)},
	}
}

func TestPositionsHandler_Extend_Success(t *testing.T) {
	now := time.Date(2026, 6, 3, 3, 22, 0, 0, time.UTC)
	repo := backtest.NewInMemoryPositionRepo()
	id := seedOpen(t, repo, now.Add(-100*time.Minute), 240)
	h := mkExtendHandler(repo, now)

	body, _ := json.Marshal(ExtendPositionRequest{ID: id, Symbol: "USD_JPY", AddMinutes: 240})
	req := httptest.NewRequest(http.MethodPost, "/api/positions/extend", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	h.Extend(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	var got ExtendPositionResponse
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.PositionID != id {
		t.Errorf("PositionID: got %d want %d", got.PositionID, id)
	}
	if got.MaxHoldMinutes != 480 {
		t.Errorf("MaxHoldMinutes: got %d want 480", got.MaxHoldMinutes)
	}
	if got.AddedMinutes != 240 {
		t.Errorf("AddedMinutes: got %d want 240", got.AddedMinutes)
	}
	// OpenedAt+480 = now+380 → remaining 380。
	if got.RemainingMinutes != 380 {
		t.Errorf("RemainingMinutes: got %v want 380", got.RemainingMinutes)
	}
}

func TestPositionsHandler_Extend_Errors(t *testing.T) {
	now := time.Now()

	t.Run("GET is 405", func(t *testing.T) {
		repo := backtest.NewInMemoryPositionRepo()
		h := mkExtendHandler(repo, now)
		req := httptest.NewRequest(http.MethodGet, "/api/positions/extend", nil)
		rec := httptest.NewRecorder()
		h.Extend(rec, req)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("got %d want 405", rec.Code)
		}
	})

	t.Run("nil command is 503", func(t *testing.T) {
		h := &PositionsHandler{}
		req := httptest.NewRequest(http.MethodPost, "/api/positions/extend", bytes.NewReader([]byte(`{"id":1,"add_minutes":60}`)))
		rec := httptest.NewRecorder()
		h.Extend(rec, req)
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("got %d want 503", rec.Code)
		}
	})

	t.Run("missing id is 400", func(t *testing.T) {
		repo := backtest.NewInMemoryPositionRepo()
		h := mkExtendHandler(repo, now)
		req := httptest.NewRequest(http.MethodPost, "/api/positions/extend", bytes.NewReader([]byte(`{"add_minutes":60}`)))
		rec := httptest.NewRecorder()
		h.Extend(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("got %d want 400", rec.Code)
		}
	})

	t.Run("invalid add_minutes is 400", func(t *testing.T) {
		repo := backtest.NewInMemoryPositionRepo()
		id := seedOpen(t, repo, now, 240)
		h := mkExtendHandler(repo, now)
		body, _ := json.Marshal(ExtendPositionRequest{ID: id, AddMinutes: 0})
		req := httptest.NewRequest(http.MethodPost, "/api/positions/extend", bytes.NewReader(body))
		rec := httptest.NewRecorder()
		h.Extend(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("got %d want 400 (body=%s)", rec.Code, rec.Body.String())
		}
	})

	t.Run("unknown id is 404", func(t *testing.T) {
		repo := backtest.NewInMemoryPositionRepo()
		h := mkExtendHandler(repo, now)
		body, _ := json.Marshal(ExtendPositionRequest{ID: 999, AddMinutes: 60})
		req := httptest.NewRequest(http.MethodPost, "/api/positions/extend", bytes.NewReader(body))
		rec := httptest.NewRecorder()
		h.Extend(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Errorf("got %d want 404 (body=%s)", rec.Code, rec.Body.String())
		}
	})
}
