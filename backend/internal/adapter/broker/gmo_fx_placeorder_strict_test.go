package broker

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"fx-bot/backend/internal/domain/order"
)

// PlaceOrder 内の switch req.Type が未知の型を黙って MARKET に fallback すると、
// LIMIT / STOP / OCO / IFD / "" (zero value) などを誤って渡したとき意図せず
// 成行発注が走る。またレスポンスの orderID が空のまま
// `&order.Order{OrderID: ""}` を返すと、後段の cancel/close が UUID なしで動く。
// 両方とも error で弾く。

func TestPlaceOrder_RejectsUnknownOrderType(t *testing.T) {
	tests := []struct {
		name      string
		orderType order.OrderType
	}{
		{"limit (not implemented yet)", order.OrderTypeLimit},
		{"stop (not implemented yet)", order.OrderTypeStop},
		{"oco (not implemented as entry)", order.OrderTypeOCO},
		{"ifd (not implemented)", order.OrderTypeIFD},
		{"empty zero value", order.OrderType("")},
		{"garbage", order.OrderType("FOOBAR")},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// テスト用 server は呼ばれないことを期待 (validation 段階で reject)。
			calls := 0
			srv := gmoTestServer(t, map[string]http.HandlerFunc{
				"/v1/order": func(w http.ResponseWriter, _ *http.Request) {
					calls++
					_, _ = w.Write([]byte(`{"status":0,"data":"1"}`))
				},
			})
			defer srv.Close()

			b := newGmoForTest(srv.URL, srv.URL)
			_, err := b.PlaceOrder(context.Background(), order.PlaceOrderRequest{
				Symbol:   "USD_JPY",
				Side:     order.SideBuy,
				Type:     tc.orderType,
				Quantity: 100,
			})
			if err == nil {
				t.Fatalf("expected error for type=%q (was silently treated as MARKET)", tc.orderType)
			}
			if !strings.Contains(err.Error(), "unsupported order type") &&
				!strings.Contains(err.Error(), string(tc.orderType)) {
				t.Errorf("error should name the type or say unsupported; got %v", err)
			}
			if calls != 0 {
				t.Errorf("HTTP should NOT have been called for invalid type; got %d calls", calls)
			}
		})
	}
}

func TestPlaceOrder_MarketStillWorks(t *testing.T) {
	// regression guard: explicit OrderTypeMarket continues to succeed
	srv := gmoTestServer(t, map[string]http.HandlerFunc{
		"/v1/order": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"status":0,"data":"12345"}`))
		},
	})
	defer srv.Close()

	b := newGmoForTest(srv.URL, srv.URL)
	ord, err := b.PlaceOrder(context.Background(), order.PlaceOrderRequest{
		Symbol:   "USD_JPY",
		Side:     order.SideBuy,
		Type:     order.OrderTypeMarket,
		Quantity: 100,
	})
	if err != nil {
		t.Fatalf("PlaceOrder MARKET: %v", err)
	}
	if ord.OrderID != "12345" {
		t.Errorf("OrderID: got %q want 12345", ord.OrderID)
	}
}

func TestPlaceOrder_RejectsEmptyOrderIDInResponse(t *testing.T) {
	// GMO が data なし (or 空 orderId) で 200 を返した場合は error で弾く。
	// 旧実装は OrderID="" の order.Order を返してしまい、その後の
	// CancelOrder / ResolveExecution が UUID なしで走り爆発していた。
	tests := []struct {
		name string
		body string
	}{
		{"empty data array", `{"status":0,"data":[]}`},
		{"data with empty orderId", `{"status":0,"data":{"orderId":""}}`},
		{"data is null", `{"status":0,"data":null}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := gmoTestServer(t, map[string]http.HandlerFunc{
				"/v1/order": func(w http.ResponseWriter, _ *http.Request) {
					_, _ = w.Write([]byte(tc.body))
				},
			})
			defer srv.Close()

			b := newGmoForTest(srv.URL, srv.URL)
			_, err := b.PlaceOrder(context.Background(), order.PlaceOrderRequest{
				Symbol:   "USD_JPY",
				Side:     order.SideBuy,
				Type:     order.OrderTypeMarket,
				Quantity: 100,
			})
			if err == nil {
				t.Fatalf("expected error when response has empty orderId; body=%s", tc.body)
			}
			if !strings.Contains(err.Error(), "orderId") && !strings.Contains(err.Error(), "order id") {
				t.Errorf("error should mention orderId; got %v", err)
			}
		})
	}
}
