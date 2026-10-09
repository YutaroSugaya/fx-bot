package broker

import (
	"context"
	"net/http"
	"testing"

	"fx-bot/backend/internal/domain/order"
)

// GMO Forex (forex-api.coin.z.com) returns positionId as a JSON **number**
// (not string) in /v1/openPositions and /v1/executions responses
// (90000001 below is a synthetic id):
//
//	"list":[{"positionId":90000001,"symbol":"USD_JPY",...}]
//
// A struct field declared `string` makes json.Unmarshal fail with:
//
//	json: cannot unmarshal number into Go struct field .list.positionId of type string
//
// which breaks startup reconcile in Live mode. Fixtures that pass positionId
// as `"123"` (string) would never surface this.
//
// These tests pin the contract: the parser must accept positionId
// as either a JSON number OR a JSON string and surface the textual ID to
// callers (BrokerPositionID is a string in our domain).

func TestGetOpenPositions_AcceptsPositionIdAsNumber(t *testing.T) {
	srv := gmoTestServer(t, map[string]http.HandlerFunc{
		"/v1/openPositions": func(w http.ResponseWriter, r *http.Request) {
			// Real GMO Forex response shape: positionId is an unquoted JSON
			// number (the value itself is synthetic).
			_, _ = w.Write([]byte(`{"status":0,"data":{"list":[
				{"positionId":90000001,"symbol":"USD_JPY","side":"BUY","size":"1000","price":"159.04","timestamp":"2020-01-06T01:55:19.997Z"}
			]}}`))
		},
	})
	defer srv.Close()

	b := newGmoForTest(srv.URL, srv.URL)
	ps, err := b.GetOpenPositions(context.Background(), "USD_JPY")
	if err != nil {
		t.Fatalf("GetOpenPositions must accept positionId as number; got %v", err)
	}
	if len(ps) != 1 {
		t.Fatalf("want 1 position, got %d", len(ps))
	}
	if ps[0].BrokerPositionID != "90000001" {
		t.Errorf("BrokerPositionID: got %q, want %q (numeric id rendered as decimal string)", ps[0].BrokerPositionID, "90000001")
	}
	if ps[0].Side != order.SideBuy || ps[0].Quantity != 1000 {
		t.Errorf("position: %+v", ps[0])
	}
}

// Backwards compatibility: keep accepting string positionId too, since
// some GMO endpoints (or future versions) may quote the value. This is
// what the existing fixtures and current tests use.
func TestGetOpenPositions_StillAcceptsPositionIdAsString(t *testing.T) {
	srv := gmoTestServer(t, map[string]http.HandlerFunc{
		"/v1/openPositions": func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"status":0,"data":{"list":[
				{"positionId":"123","symbol":"USD_JPY","side":"BUY","size":"100","price":"150.10","timestamp":"2026-05-15T00:50:00.000Z"}
			]}}`))
		},
	})
	defer srv.Close()

	b := newGmoForTest(srv.URL, srv.URL)
	ps, err := b.GetOpenPositions(context.Background(), "USD_JPY")
	if err != nil {
		t.Fatalf("GetOpenPositions: %v", err)
	}
	if len(ps) != 1 || ps[0].BrokerPositionID != "123" {
		t.Errorf("position: %+v", ps)
	}
}

// Executions endpoint has the same shape risk: positionId (and likely
// orderId / executionId) are returned as numbers in production responses.
// Pin acceptance of number-typed ids here so the close-saga's
// ResolveExecution path doesn't break the next time it's exercised live.
func TestGetExecutions_AcceptsPositionIdAsNumber(t *testing.T) {
	srv := gmoTestServer(t, map[string]http.HandlerFunc{
		"/v1/executions": func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"status":0,"data":{"list":[
				{"executionId":99,"orderId":42,"positionId":90000001,"symbol":"USD_JPY","side":"BUY","size":"1000","price":"159.05","timestamp":"2020-01-06T01:55:20.000Z"}
			]}}`))
		},
	})
	defer srv.Close()

	b := newGmoForTest(srv.URL, srv.URL)
	xs, err := b.GetExecutions(context.Background(), "42")
	if err != nil {
		t.Fatalf("GetExecutions must accept numeric ids; got %v", err)
	}
	if len(xs) != 1 {
		t.Fatalf("want 1 execution, got %d", len(xs))
	}
	if xs[0].PositionID != "90000001" || xs[0].OrderID != "42" || xs[0].ExecutionID != "99" {
		t.Errorf("execution: %+v", xs[0])
	}
}
