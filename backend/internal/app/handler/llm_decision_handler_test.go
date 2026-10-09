package handler

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TriggerNow drives the dashboard's manual "全ペア再判断" button. It must:
//   - reject non-POST,
//   - 503 when no trigger is wired,
//   - 202 + {"started":true} when the cycle is kicked off,
//   - 409 + {"started":false} when a cycle is already running (the trigger errored).
func TestLLMDecisionHandler_TriggerNow(t *testing.T) {
	t.Run("GET is rejected", func(t *testing.T) {
		h := &LLMDecisionHandler{Trigger: func() error { return nil }}
		rec := httptest.NewRecorder()
		h.TriggerNow(rec, httptest.NewRequest(http.MethodGet, "/api/llm-decision/trigger", nil))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("GET: got %d want 405", rec.Code)
		}
	})

	t.Run("no trigger wired → 503", func(t *testing.T) {
		h := &LLMDecisionHandler{}
		rec := httptest.NewRecorder()
		h.TriggerNow(rec, httptest.NewRequest(http.MethodPost, "/api/llm-decision/trigger", nil))
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("no trigger: got %d want 503", rec.Code)
		}
	})

	t.Run("started → 202 started:true", func(t *testing.T) {
		var called bool
		h := &LLMDecisionHandler{Trigger: func() error { called = true; return nil }}
		rec := httptest.NewRecorder()
		h.TriggerNow(rec, httptest.NewRequest(http.MethodPost, "/api/llm-decision/trigger", nil))
		if rec.Code != http.StatusAccepted {
			t.Errorf("started: got %d want 202", rec.Code)
		}
		if !called {
			t.Error("Trigger was not invoked")
		}
		if !strings.Contains(rec.Body.String(), `"started":true`) {
			t.Errorf("body should report started:true, got %s", rec.Body.String())
		}
	})

	t.Run("already running → 409 started:false", func(t *testing.T) {
		h := &LLMDecisionHandler{Trigger: func() error { return errors.New("判断サイクルを実行中です") }}
		rec := httptest.NewRecorder()
		h.TriggerNow(rec, httptest.NewRequest(http.MethodPost, "/api/llm-decision/trigger", nil))
		if rec.Code != http.StatusConflict {
			t.Errorf("busy: got %d want 409", rec.Code)
		}
		if !strings.Contains(rec.Body.String(), `"started":false`) {
			t.Errorf("body should report started:false, got %s", rec.Body.String())
		}
	})
}
