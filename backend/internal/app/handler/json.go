package handler

import (
	"encoding/json"
	"net/http"
)

// WriteJSON writes v as JSON with the given HTTP status code.
// Encode errors are intentionally swallowed: clients receive a partial body
// rather than a 500 they can't act on. Real errors should be caught by tests.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// WriteError writes a {"error":"<msg>"} JSON body with the given status.
func WriteError(w http.ResponseWriter, status int, msg string) {
	WriteJSON(w, status, map[string]string{"error": msg})
}

// requirePOST writes a 405 and returns false when the request is not a POST,
// so write handlers can guard with `if !requirePOST(w, r) { return }`.
func requirePOST(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodPost {
		WriteError(w, http.StatusMethodNotAllowed, "method not allowed")
		return false
	}
	return true
}
