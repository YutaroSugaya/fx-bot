package handler

import (
	"log/slog"
	"net/http"
	"os"

	"fx-bot/backend/internal/safety"
)

// EmergencyHandler exposes:
//
//	POST /api/emergency-stop   — writes runtime/emergency_stop.flag
//	POST /api/emergency-resume — removes the flag
type EmergencyHandler struct {
	FlagPath string
	Logger   *slog.Logger
}

// Stop は POST /api/emergency-stop。
func (h *EmergencyHandler) Stop(w http.ResponseWriter, r *http.Request) {
	if !requirePOST(w, r) {
		return
	}
	if h.FlagPath == "" {
		WriteError(w, http.StatusInternalServerError, "emergency flag path not configured")
		return
	}
	if err := safety.TripFor(h.FlagPath, safety.ReasonManualViaAPI); err != nil {
		WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if h.Logger != nil {
		h.Logger.Warn("emergency_stop_engaged_via_api", "remote", r.RemoteAddr)
	}
	WriteJSON(w, http.StatusOK, map[string]string{"emergency_stop": "active"})
}

// Resume は POST /api/emergency-resume。flag を削除する。
func (h *EmergencyHandler) Resume(w http.ResponseWriter, r *http.Request) {
	if !requirePOST(w, r) {
		return
	}
	if h.FlagPath == "" {
		WriteError(w, http.StatusInternalServerError, "emergency flag path not configured")
		return
	}
	_ = os.Remove(h.FlagPath)
	if h.Logger != nil {
		h.Logger.Info("emergency_resume_via_api", "remote", r.RemoteAddr)
	}
	WriteJSON(w, http.StatusOK, map[string]string{"emergency_stop": "cleared"})
}
