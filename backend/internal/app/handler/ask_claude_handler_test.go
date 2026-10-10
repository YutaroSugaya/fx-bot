package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"fx-bot/backend/internal/adapter/artifact"
	"fx-bot/backend/internal/usecase/query"
)

func mkAskHandler(runner query.PromptRunner) *AskClaudeHandler {
	return &AskClaudeHandler{
		Query: &query.AskClaudeQuery{
			// missing-artifact store — Execute falls back to {} JSON.
			SummaryStore: artifact.NewFileMarketSummaryStore("/non/existent"),
			Runner:       runner,
		},
	}
}

func TestAskClaudeHandler(t *testing.T) {
	cases := []struct {
		name        string
		method      string
		body        string
		runner      query.PromptRunner
		nilQuery    bool
		wantStatus  int
		wantAnswer  string
		wantErrPart string
	}{
		{
			name:       "happy path returns answer",
			method:     http.MethodPost,
			body:       `{"question":"トレンドは？"}`,
			runner:     func(context.Context, string) (string, error) { return "上昇トレンドです", nil },
			wantStatus: http.StatusOK,
			wantAnswer: "上昇トレンドです",
		},
		{
			name:       "GET returns 405",
			method:     http.MethodGet,
			body:       "",
			runner:     func(context.Context, string) (string, error) { return "", nil },
			wantStatus: http.StatusMethodNotAllowed,
		},
		{
			name:       "empty question returns 400",
			method:     http.MethodPost,
			body:       `{"question":""}`,
			runner:     func(context.Context, string) (string, error) { return "", nil },
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "nil query returns 503",
			method:     http.MethodPost,
			body:       `{"question":"x"}`,
			nilQuery:   true,
			wantStatus: http.StatusServiceUnavailable,
		},
		{
			name:        "runner error returns 500 with cause in body",
			method:      http.MethodPost,
			body:        `{"question":"x"}`,
			runner:      func(context.Context, string) (string, error) { return "", errors.New("cli boom") },
			wantStatus:  http.StatusInternalServerError,
			wantErrPart: "cli boom",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var h *AskClaudeHandler
			if tc.nilQuery {
				h = &AskClaudeHandler{Query: nil}
			} else {
				h = mkAskHandler(tc.runner)
			}
			rec := httptest.NewRecorder()
			h.Ask(rec, httptest.NewRequest(tc.method, "/api/ask-claude", bytes.NewReader([]byte(tc.body))))
			if rec.Code != tc.wantStatus {
				t.Fatalf("status: %d want %d body=%s", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if tc.wantAnswer == "" && tc.wantErrPart == "" {
				return
			}
			var body AskClaudeResponse
			_ = json.NewDecoder(rec.Body).Decode(&body)
			if tc.wantAnswer != "" && body.Answer != tc.wantAnswer {
				t.Errorf("answer: %q want %q", body.Answer, tc.wantAnswer)
			}
			if tc.wantErrPart != "" && !strings.Contains(body.Error, tc.wantErrPart) {
				t.Errorf("error %q should contain %q", body.Error, tc.wantErrPart)
			}
		})
	}
}
