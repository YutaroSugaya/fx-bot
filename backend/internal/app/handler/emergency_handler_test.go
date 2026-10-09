package handler

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestEmergencyHandler_Stop(t *testing.T) {
	cases := []struct {
		name          string
		method        string
		flagPathFn    func(t *testing.T) string
		wantStatus    int
		wantFileExist bool
	}{
		{
			name:          "POST writes flag",
			method:        http.MethodPost,
			flagPathFn:    func(t *testing.T) string { return filepath.Join(t.TempDir(), "emg.flag") },
			wantStatus:    http.StatusOK,
			wantFileExist: true,
		},
		{
			name:       "GET returns 405",
			method:     http.MethodGet,
			flagPathFn: func(t *testing.T) string { return filepath.Join(t.TempDir(), "emg.flag") },
			wantStatus: http.StatusMethodNotAllowed,
		},
		{
			name:       "empty flag path returns 500",
			method:     http.MethodPost,
			flagPathFn: func(t *testing.T) string { return "" },
			wantStatus: http.StatusInternalServerError,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := tc.flagPathFn(t)
			h := &EmergencyHandler{FlagPath: path}
			rec := httptest.NewRecorder()
			h.Stop(rec, httptest.NewRequest(tc.method, "/api/emergency-stop", nil))
			if rec.Code != tc.wantStatus {
				t.Fatalf("status: %d want %d", rec.Code, tc.wantStatus)
			}
			if tc.wantFileExist {
				if _, err := os.Stat(path); err != nil {
					t.Errorf("flag should exist; stat err=%v", err)
				}
			}
		})
	}
}

func TestEmergencyHandler_Resume(t *testing.T) {
	cases := []struct {
		name            string
		method          string
		preCreateFlag   bool
		emptyPath       bool
		wantStatus      int
		wantFileMissing bool
	}{
		{"POST removes existing flag", http.MethodPost, true, false, http.StatusOK, true},
		{"POST on absent flag still 200 (idempotent)", http.MethodPost, false, false, http.StatusOK, true},
		{"GET returns 405", http.MethodGet, true, false, http.StatusMethodNotAllowed, false},
		{"empty path returns 500", http.MethodPost, false, true, http.StatusInternalServerError, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var path string
			if !tc.emptyPath {
				path = filepath.Join(t.TempDir(), "emg.flag")
				if tc.preCreateFlag {
					if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
						t.Fatalf("setup: %v", err)
					}
				}
			}
			h := &EmergencyHandler{FlagPath: path}
			rec := httptest.NewRecorder()
			h.Resume(rec, httptest.NewRequest(tc.method, "/api/emergency-resume", nil))
			if rec.Code != tc.wantStatus {
				t.Fatalf("status: %d want %d", rec.Code, tc.wantStatus)
			}
			if tc.wantFileMissing && !tc.emptyPath {
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Errorf("flag should be removed; err=%v", err)
				}
			}
		})
	}
}
