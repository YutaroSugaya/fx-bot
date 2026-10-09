package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"fx-bot/backend/internal/backtest"
	"fx-bot/backend/internal/port"
	"fx-bot/backend/internal/usecase/query"
)

// seedRecentRepo returns an InMemoryStrategyConfigRepo where ListRecent will
// emit `rows` in their declared order (rows[0] first). Achieved by inserting
// in REVERSE order, because ListRecent returns most-recent-first.
func seedRecentRepo(t *testing.T, rows []port.StrategyConfigRecordWithMeta) *backtest.InMemoryStrategyConfigRepo {
	t.Helper()
	repo := backtest.NewInMemoryStrategyConfigRepo()
	for i := len(rows) - 1; i >= 0; i-- {
		if err := repo.Insert(context.Background(), rows[i].StrategyConfigRecord); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	return repo
}

func TestAdvisorHandler_Recent(t *testing.T) {
	seeded := []port.StrategyConfigRecordWithMeta{
		{StrategyConfigRecord: port.StrategyConfigRecord{
			ConfigID: "cfg-1", Status: "active", Source: "auto", StrategyName: "momentum_pullback",
			Enabled: true, RawYAML: "config_id: cfg-1\n",
		}},
		{StrategyConfigRecord: port.StrategyConfigRecord{
			ConfigID: "cfg-2", Status: "rejected", Source: "manual", RawYAML: "",
		}},
	}
	cases := []struct {
		name       string
		query      *query.ListRecentDecisionsQuery
		urlPath    string
		wantStatus int
		wantRows   int
		wantFirst  string
	}{
		{
			name:       "returns seeded rows",
			query:      &query.ListRecentDecisionsQuery{StrategyConfigs: seedRecentRepo(t, seeded)},
			urlPath:    "/api/advisor/recent",
			wantStatus: http.StatusOK, wantRows: 2, wantFirst: "cfg-1",
		},
		{
			name:       "limit query param is honored / clamped",
			query:      &query.ListRecentDecisionsQuery{StrategyConfigs: backtest.NewInMemoryStrategyConfigRepo()},
			urlPath:    "/api/advisor/recent?limit=99999",
			wantStatus: http.StatusOK, wantRows: 0,
		},
		{
			name:       "nil query returns 503",
			query:      nil,
			urlPath:    "/api/advisor/recent",
			wantStatus: http.StatusServiceUnavailable,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := &AdvisorHandler{RecentQuery: tc.query}
			rec := httptest.NewRecorder()
			h.Recent(rec, httptest.NewRequest(http.MethodGet, tc.urlPath, nil))
			if rec.Code != tc.wantStatus {
				t.Fatalf("status: %d want %d", rec.Code, tc.wantStatus)
			}
			if tc.wantStatus != http.StatusOK {
				return
			}
			var got []map[string]any
			_ = json.NewDecoder(rec.Body).Decode(&got)
			if len(got) != tc.wantRows {
				t.Errorf("rows: got %d want %d", len(got), tc.wantRows)
			}
			if tc.wantFirst != "" && len(got) > 0 && got[0]["config_id"] != tc.wantFirst {
				t.Errorf("first row config_id: %v want %q", got[0]["config_id"], tc.wantFirst)
			}
		})
	}
}

func TestAdvisorHandler_TriggerNow(t *testing.T) {
	cases := []struct {
		name           string
		method         string
		trigger        TriggerAdvisorFunc
		wantStatus     int
		wantPromoted   bool
		wantErrorEmpty bool
	}{
		{"nil trigger returns 503", http.MethodPost, nil, http.StatusServiceUnavailable, false, true},
		{"GET returns 405", http.MethodGet,
			func(context.Context, string) (TriggerResult, error) { return TriggerResult{}, nil },
			http.StatusMethodNotAllowed, false, true},
		{"happy path 200 + promoted=true", http.MethodPost,
			func(context.Context, string) (TriggerResult, error) {
				return TriggerResult{Promoted: true, ConfigID: "cfg-new"}, nil
			},
			http.StatusOK, true, true},
		{"trigger error 500 with error body", http.MethodPost,
			func(context.Context, string) (TriggerResult, error) {
				return TriggerResult{}, errors.New("claude crashed")
			},
			http.StatusInternalServerError, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := &AdvisorHandler{Trigger: tc.trigger, CLITimeout: time.Second}
			rec := httptest.NewRecorder()
			h.TriggerNow(rec, httptest.NewRequest(tc.method, "/api/advisor/trigger", nil))
			if rec.Code != tc.wantStatus {
				t.Fatalf("status: %d want %d", rec.Code, tc.wantStatus)
			}
			if tc.wantStatus != http.StatusOK && tc.wantStatus != http.StatusInternalServerError {
				return
			}
			var res TriggerResult
			_ = json.NewDecoder(rec.Body).Decode(&res)
			if res.Promoted != tc.wantPromoted {
				t.Errorf("Promoted: got %v want %v", res.Promoted, tc.wantPromoted)
			}
			if tc.wantErrorEmpty && res.Error != "" {
				t.Errorf("Error should be empty; got %q", res.Error)
			}
			if !tc.wantErrorEmpty && res.Error == "" {
				t.Errorf("Error should be populated for failure path")
			}
		})
	}
}
