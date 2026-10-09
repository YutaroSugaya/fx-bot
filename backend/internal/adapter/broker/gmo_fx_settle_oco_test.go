package broker

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"fx-bot/backend/internal/domain/order"
	"fx-bot/backend/internal/port"
)

// PlaceSettleOCO posts to /v1/closeOrder with executionType=OCO. This is the
// MARKET+OCO unified path's second leg: after a MARKET entry fills, the
// caller attaches an OCO (take-profit-LIMIT + stop-loss-STOP pair) tied to
// the freshly opened broker position.
//
// Why this path exists: GMO Forex IFDOCO (/v1/ifoOrder)
// rejects sizes below 10,000 (= 1 万通貨 lot). The /v1/closeOrder OCO path
// accepts the symbols-API minimum (100 currency). The trade-off is a
// 1-2 second naked window between the MARKET fill and the OCO place —
// if the OCO fails the usecase immediately closes the position
// (compensating close); emergency_stop fires only if that close also fails.

func TestGmoBroker_PlaceSettleOCO_PostsCloseOrderWithOCOBody(t *testing.T) {
	var capturedBody []byte
	var capturedPath string
	srv := gmoTestServer(t, map[string]http.HandlerFunc{
		"/v1/closeOrder": func(w http.ResponseWriter, r *http.Request) {
			capturedBody = readBody(t, r)
			capturedPath = r.URL.Path
			verifyPrivateHeaders(t, r, http.MethodPost, "/v1/closeOrder", capturedBody)
			// Per docs the response data is the new root order id.
			// (Empirically /v1/closeOrder returns either a scalar or
			// {rootOrderId}; the broker's flexString-based extractor
			// handles both — we test the object form here.)
			w.Write([]byte(`{"status":0,"data":{"rootOrderId":987654321}}`))
		},
	})
	defer srv.Close()

	b := newGmoForTest(srv.URL, srv.URL)
	rootID, err := b.PlaceSettleOCO(context.Background(), port.OCOCloseOrderInput{
		Symbol:           "USD_JPY",
		BrokerPositionID: 1000042,
		Side:             order.SideSell, // close side for a BUY position
		Size:             1000,
		TPPrice:          159.060,
		SLPrice:          158.710,
	})
	if err != nil {
		t.Fatalf("PlaceSettleOCO: %v", err)
	}
	if rootID != "987654321" {
		t.Errorf("rootID: got %q, want 987654321 (rootOrderId rendered as decimal string)", rootID)
	}
	if capturedPath != "/v1/closeOrder" {
		t.Errorf("path: got %q, want /v1/closeOrder", capturedPath)
	}

	var payload map[string]any
	if err := json.Unmarshal(capturedBody, &payload); err != nil {
		t.Fatalf("payload not json: %v", err)
	}
	if payload["symbol"] != "USD_JPY" {
		t.Errorf("symbol: %v", payload["symbol"])
	}
	if payload["side"] != "SELL" {
		t.Errorf("side must be the close direction (SELL for a BUY position); got %v", payload["side"])
	}
	if payload["executionType"] != "OCO" {
		t.Errorf("executionType must be OCO; got %v", payload["executionType"])
	}
	// GMO Forex /v1/closeOrder rejects prices whose decimal-string
	// representation exceeds the per-symbol tickSize with ERR-5114
	// "Decimal digits of size is invalid". For USD_JPY the tick is
	// 0.001 → exactly 3 decimal places. We therefore format prices
	// with fixed 3 decimals; trailing zeros must be preserved
	// ("159.060", not "159.06") and float64 round-off artifacts must
	// not bleed into the payload (see the artifact test below).
	if payload["limitPrice"] != "159.060" {
		t.Errorf("limitPrice (TP): got %v, want 159.060 (fixed 3-decimal per USD_JPY tickSize 0.001)", payload["limitPrice"])
	}
	if payload["stopPrice"] != "158.710" {
		t.Errorf("stopPrice (SL): got %v, want 158.710 (fixed 3-decimal per USD_JPY tickSize 0.001)", payload["stopPrice"])
	}
	sp, ok := payload["settlePosition"].([]any)
	if !ok || len(sp) != 1 {
		t.Fatalf("settlePosition: must be a 1-element array, got %v", payload["settlePosition"])
	}
	entry := sp[0].(map[string]any)
	idNum, ok := entry["positionId"].(float64)
	if !ok || int64(idNum) != 1000042 {
		t.Errorf("settlePosition.positionId: got %v (type %T), want 1000042 as JSON number", entry["positionId"], entry["positionId"])
	}
	if entry["size"] != "1000" {
		t.Errorf("settlePosition.size: got %v, want '1000'", entry["size"])
	}
}

// TestGmoBroker_PlaceSettleOCO_NoFloat64Artifacts pins the price formatter
// against float64 round-off bleed. The bot derives TP/SL via
// entry +/- N*pip where pip = 0.01 for USD_JPY; 0.01 is not exactly
// representable in float64 so naive `FormatFloat(x, 'f', -1, 64)`
// can emit e.g. "158.87899999999998" which GMO rejects with ERR-5114
// "Decimal digits of size is invalid" (a live order gets rejected this way).
func TestGmoBroker_PlaceSettleOCO_NoFloat64Artifacts(t *testing.T) {
	var capturedBody []byte
	srv := gmoTestServer(t, map[string]http.HandlerFunc{
		"/v1/closeOrder": func(w http.ResponseWriter, r *http.Request) {
			capturedBody = readBody(t, r)
			w.Write([]byte(`{"status":0,"data":{"rootOrderId":1}}`))
		},
	})
	defer srv.Close()

	// tp/sl computed the same way the usecase derives them: a base
	// price plus an integer pip count times 0.01. The arithmetic is
	// exact in math but produces float64 round-off in the runtime.
	tp := 158.829 + 5*0.01 // 158.879 mathematically
	sl := 158.829 - 5*0.01 // 158.779 mathematically

	b := newGmoForTest(srv.URL, srv.URL)
	_, err := b.PlaceSettleOCO(context.Background(), port.OCOCloseOrderInput{
		Symbol:           "USD_JPY",
		BrokerPositionID: 1,
		Side:             order.SideSell,
		Size:             1000,
		TPPrice:          tp,
		SLPrice:          sl,
	})
	if err != nil {
		t.Fatalf("PlaceSettleOCO: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(capturedBody, &payload); err != nil {
		t.Fatalf("payload not json: %v", err)
	}
	if payload["limitPrice"] != "158.879" {
		t.Errorf("limitPrice must be rounded to USD_JPY tickSize (0.001); got %v (float64 artifact bled through)", payload["limitPrice"])
	}
	if payload["stopPrice"] != "158.779" {
		t.Errorf("stopPrice must be rounded to USD_JPY tickSize (0.001); got %v (float64 artifact bled through)", payload["stopPrice"])
	}
}

func TestGmoBroker_PlaceSettleOCO_RejectsInvalidInput(t *testing.T) {
	b := newGmoForTest("http://unused", "http://unused")
	bad := []port.OCOCloseOrderInput{
		{Symbol: "USD_JPY", BrokerPositionID: 1, Side: order.SideSell, Size: 0, TPPrice: 1, SLPrice: 1},    // zero size
		{Symbol: "USD_JPY", BrokerPositionID: 1, Side: order.SideSell, Size: 1000, TPPrice: 0, SLPrice: 1}, // zero TP
		{Symbol: "USD_JPY", BrokerPositionID: 1, Side: order.SideSell, Size: 1000, TPPrice: 1, SLPrice: 0}, // zero SL
		{Symbol: "USD_JPY", BrokerPositionID: 0, Side: order.SideSell, Size: 1000, TPPrice: 1, SLPrice: 1}, // zero positionID
		{Symbol: "", BrokerPositionID: 1, Side: order.SideSell, Size: 1000, TPPrice: 1, SLPrice: 1},        // empty symbol
	}
	for i, in := range bad {
		_, err := b.PlaceSettleOCO(context.Background(), in)
		if err == nil || !strings.Contains(err.Error(), "OCO") {
			t.Errorf("case %d (input=%+v): expected validation err, got %v", i, in, err)
		}
	}
}
