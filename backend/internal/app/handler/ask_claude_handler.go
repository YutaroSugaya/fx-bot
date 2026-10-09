package handler

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"fx-bot/backend/internal/usecase/query"
)

// AskClaudeHandler exposes POST /api/ask-claude.
//
// Usecase へ委譲する thin HTTP adapter。Logger は I/O ログ用、timeout は
// caller (cmd/bot/main.go) が context.WithTimeout でラップ済みの想定なので
// ここでは追加 timeout を持たない。
type AskClaudeHandler struct {
	Query  *query.AskClaudeQuery
	Logger *slog.Logger
}

func (h *AskClaudeHandler) Ask(w http.ResponseWriter, r *http.Request) {
	if !requirePOST(w, r) {
		return
	}
	if h.Query == nil {
		WriteError(w, http.StatusServiceUnavailable, "ask-claude not configured")
		return
	}
	var body AskClaudeRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Question == "" {
		WriteError(w, http.StatusBadRequest, "question is required")
		return
	}

	start := time.Now()
	out, err := h.Query.Execute(r.Context(), query.AskClaudeInput{Question: body.Question})
	durationMs := time.Since(start).Milliseconds()
	if err != nil {
		if h.Logger != nil {
			h.Logger.Error("ask_claude_failed", "err", err)
		}
		WriteJSON(w, http.StatusInternalServerError, AskClaudeResponse{
			Error: err.Error(), DurationMs: durationMs,
		})
		return
	}
	WriteJSON(w, http.StatusOK, AskClaudeResponse{
		Answer: out.Answer, DurationMs: durationMs,
	})
}
