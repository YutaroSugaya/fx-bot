// httptest を使った GMO API record-replay テスト。外部 (実 GMO) には繋がない
// ため build tag なしで `make test` で実行する。
//
// 検証ポイント:
//   - PlaceOrder (IFDOCO) が GMO 形式の "status:0, data:{rootOrderId}" を正しく解析
//   - ResolveExecution が executions[] から positionId / fillPrice を抽出
//   - ClosePosition が opposing side の closeOrder ペイロードを送る
//   - status != 0 のエラー envelope が error として伝播

package broker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"fx-bot/backend/internal/domain/order"
	"fx-bot/backend/internal/domain/position"
)

// fixtureResponse は GMO /private/v1/<endpoint> がよく返す JSON 形式の最小例。
func fixtureResponse(data any) string {
	body := map[string]any{
		"status":       0,
		"data":         data,
		"responsetime": "2026-05-18T12:00:00.000Z",
	}
	b, _ := json.Marshal(body)
	return string(b)
}

// fakeGMOServer は GMO API を模した httptest server。
// path → response の static map で fixture-replay を実現する。
func fakeGMOServer(t *testing.T, routes map[string]string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if body, ok := routes[r.URL.Path]; ok {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(body))
			return
		}
		http.Error(w, "no fixture for "+r.URL.Path, http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestGmoBroker_PlaceOrder_IFDOCO_ParsesAcceptedOrderID は IFDOCO エントリ送信
// → "status: 0, data: {rootOrderId: <num>}" レスポンスを解析できることを検証。
// IFDOCO uses /v1/ifoOrder (not /v1/order); the success payload is an object
// with rootOrderId rather than a bare orderId string.
func TestGmoBroker_PlaceOrder_IFDOCO_ParsesAcceptedOrderID(t *testing.T) {
	srv := fakeGMOServer(t, map[string]string{
		"/v1/ifoOrder": fixtureResponse(map[string]any{"rootOrderId": 1234512345}),
	})
	br := NewGmoBroker(GmoBrokerConfig{
		PublicBaseURL:  srv.URL,
		PrivateBaseURL: srv.URL,
		APIKey:         "k", APISecret: "s",
	})

	ord, err := br.PlaceOrder(context.Background(), order.PlaceOrderRequest{
		Symbol: "USD_JPY", Side: order.SideBuy, Type: order.OrderTypeIFDOCO,
		Quantity: 100, Price: 150.00, TakeProfit: 150.20, StopLoss: 149.85,
	})
	if err != nil {
		t.Fatalf("PlaceOrder: %v", err)
	}
	if ord.OrderID != "1234512345" {
		t.Errorf("OrderID: got %q want 1234512345 (rootOrderId)", ord.OrderID)
	}
	if ord.Status != "ACCEPTED" {
		t.Errorf("Status: got %q want ACCEPTED", ord.Status)
	}
}

// TestGmoBroker_ResolveExecution_PollsAndReturnsPositionID は
// GetExecutions → ResolveExecution が positionId と fillPx を抽出することを検証。
// Real GMO Forex /v1/executions returns positionId/orderId/
// executionId as JSON NUMBERs, and the bot's flexString
// type accepts both. Fixtures here send numbers to match the real API.
func TestGmoBroker_ResolveExecution_PollsAndReturnsPositionID(t *testing.T) {
	executionsData := map[string]any{
		"list": []map[string]any{{
			"executionId": 1,
			"orderId":     12345,
			"positionId":  9876,
			"symbol":      "USD_JPY",
			"side":        "BUY",
			"size":        "100",
			"price":       "150.10",
			"timestamp":   "2026-05-18T12:00:01Z",
		}},
	}
	srv := fakeGMOServer(t, map[string]string{
		"/v1/executions": fixtureResponse(executionsData),
	})
	br := NewGmoBroker(GmoBrokerConfig{
		PublicBaseURL:  srv.URL,
		PrivateBaseURL: srv.URL,
		APIKey:         "k", APISecret: "s",
	})

	res, err := br.ResolveExecution(context.Background(), "12345")
	if err != nil {
		t.Fatalf("ResolveExecution: %v", err)
	}
	posID, fillPx := res.PositionID, res.Price
	if posID != "9876" {
		t.Errorf("positionID: got %q want 9876", posID)
	}
	if fillPx != 150.10 {
		t.Errorf("fillPrice: got %v want 150.10", fillPx)
	}
}

// TestGmoBroker_ClosePosition_SendsOpposingSide は ClosePosition が
// 正しい closeSide / size / positionId を /v1/closeOrder に POST することを検証。
func TestGmoBroker_ClosePosition_SendsOpposingSide(t *testing.T) {
	var receivedBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/closeOrder" {
			http.Error(w, "wrong path", http.StatusNotFound)
			return
		}
		buf := make([]byte, 1024)
		n, _ := r.Body.Read(buf)
		receivedBody = string(buf[:n])
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(fixtureResponse("close-orderid-99")))
	}))
	t.Cleanup(srv.Close)

	br := NewGmoBroker(GmoBrokerConfig{
		PublicBaseURL:  srv.URL,
		PrivateBaseURL: srv.URL,
		APIKey:         "k", APISecret: "s",
	})

	pos := position.Position{
		BrokerPositionID: "9876", Symbol: "USD_JPY",
		Side: order.SideBuy, Quantity: 100, EntryPrice: 150.00,
	}
	ord, err := br.ClosePosition(context.Background(), pos)
	if err != nil {
		t.Fatalf("ClosePosition: %v", err)
	}
	if ord.OrderID != "close-orderid-99" {
		t.Errorf("OrderID: got %q", ord.OrderID)
	}
	// BUY position → close request must send opposing side SELL
	if !strings.Contains(receivedBody, `"side":"SELL"`) {
		t.Errorf("body should request opposing side SELL; got %s", receivedBody)
	}
	// GMO Forex requires positionId as a JSON NUMBER (no quotes)
	if !strings.Contains(receivedBody, `"positionId":9876`) {
		t.Errorf("body should send positionId as a JSON number (no quotes); got %s", receivedBody)
	}
}

// TestGmoBroker_ErrorEnvelope_PropagatesAsError は GMO API が status!=0 を
// 返したときに errorFromEnvelope がエラーとして上位に伝播することを検証。
func TestGmoBroker_ErrorEnvelope_PropagatesAsError(t *testing.T) {
	errBody := map[string]any{
		"status": 5,
		"messages": []map[string]string{
			{"message_code": "ERR-201", "message_string": "Invalid parameter"},
		},
		"responsetime": "2026-05-18T12:00:00Z",
	}
	b, _ := json.Marshal(errBody)
	srv := fakeGMOServer(t, map[string]string{"/v1/order": string(b)})
	br := NewGmoBroker(GmoBrokerConfig{
		PublicBaseURL:  srv.URL,
		PrivateBaseURL: srv.URL,
		APIKey:         "k", APISecret: "s",
	})

	_, err := br.PlaceOrder(context.Background(), order.PlaceOrderRequest{
		Symbol: "USD_JPY", Side: order.SideBuy, Type: order.OrderTypeMarket, Quantity: 100,
	})
	if err == nil {
		t.Fatal("expected error from non-zero status envelope")
	}
	if !strings.Contains(err.Error(), "ERR-201") || !strings.Contains(err.Error(), "Invalid parameter") {
		t.Errorf("err should include GMO message_code + string; got %v", err)
	}
}
