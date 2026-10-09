package broker

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

// gmo_fx.go の各レスポンスパース箇所で strconv / time.Parse のエラーを
// 握り潰すと、GMO が壊れた値を返したとき price=0 / timestamp=zero の
// ticker / position / order が下流に流れてしまう。パースは fail-loud にする。

func TestGetTicker_MalformedAskReturnsError(t *testing.T) {
	srv := gmoTestServer(t, map[string]http.HandlerFunc{
		"/v1/ticker": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{
				"status":0,
				"data":[{"symbol":"USD_JPY","ask":"not-a-number","bid":"150.121","timestamp":"2026-05-15T01:00:00.000Z"}],
				"responsetime":"2026-05-15T01:00:00.000Z"
			}`))
		},
	})
	defer srv.Close()

	b := newGmoForTest(srv.URL, srv.URL)
	tk, err := b.GetTicker(context.Background(), "USD_JPY")
	if err == nil {
		t.Fatalf("expected parse error, got ticker=%+v", tk)
	}
	if !strings.Contains(err.Error(), "ask") {
		t.Errorf("error should mention field 'ask'; got %v", err)
	}
}

func TestGetTicker_MalformedBidReturnsError(t *testing.T) {
	srv := gmoTestServer(t, map[string]http.HandlerFunc{
		"/v1/ticker": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{
				"status":0,
				"data":[{"symbol":"USD_JPY","ask":"150.123","bid":"oops","timestamp":"2026-05-15T01:00:00.000Z"}],
				"responsetime":"2026-05-15T01:00:00.000Z"
			}`))
		},
	})
	defer srv.Close()

	b := newGmoForTest(srv.URL, srv.URL)
	_, err := b.GetTicker(context.Background(), "USD_JPY")
	if err == nil {
		t.Fatal("expected parse error for malformed bid")
	}
	if !strings.Contains(err.Error(), "bid") {
		t.Errorf("error should mention field 'bid'; got %v", err)
	}
}

func TestGetTicker_MalformedTimestampReturnsError(t *testing.T) {
	srv := gmoTestServer(t, map[string]http.HandlerFunc{
		"/v1/ticker": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{
				"status":0,
				"data":[{"symbol":"USD_JPY","ask":"150.123","bid":"150.121","timestamp":"not-rfc3339"}],
				"responsetime":"2026-05-15T01:00:00.000Z"
			}`))
		},
	})
	defer srv.Close()

	b := newGmoForTest(srv.URL, srv.URL)
	_, err := b.GetTicker(context.Background(), "USD_JPY")
	if err == nil {
		t.Fatal("expected parse error for malformed timestamp")
	}
	if !strings.Contains(err.Error(), "timestamp") {
		t.Errorf("error should mention field 'timestamp'; got %v", err)
	}
}

func TestGetOpenPositions_MalformedRowReturnsError(t *testing.T) {
	srv := gmoTestServer(t, map[string]http.HandlerFunc{
		"/v1/openPositions": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{
				"status":0,
				"data":{"list":[
					{"positionId":"111","symbol":"USD_JPY","side":"BUY","size":"100","price":"150.0","timestamp":"2026-05-15T01:00:00.000Z"},
					{"positionId":"222","symbol":"USD_JPY","side":"SELL","size":"NOT_A_NUMBER","price":"150.5","timestamp":"2026-05-15T01:01:00.000Z"}
				]},
				"responsetime":"2026-05-15T01:01:00.000Z"
			}`))
		},
	})
	defer srv.Close()

	b := newGmoForTest(srv.URL, srv.URL)
	_, err := b.GetOpenPositions(context.Background(), "USD_JPY")
	if err == nil {
		t.Fatal("expected parse error for malformed size in 2nd row")
	}
	if !strings.Contains(err.Error(), "size") {
		t.Errorf("error should mention field 'size'; got %v", err)
	}
}

func TestGetExecutions_MalformedPriceReturnsError(t *testing.T) {
	srv := gmoTestServer(t, map[string]http.HandlerFunc{
		"/v1/executions": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{
				"status":0,
				"data":{"list":[
					{"executionId":"1","orderId":"100","positionId":"10","symbol":"USD_JPY","side":"BUY","size":"100","price":"BROKEN","timestamp":"2026-05-15T01:00:00.000Z"}
				]},
				"responsetime":"2026-05-15T01:00:00.000Z"
			}`))
		},
	})
	defer srv.Close()

	b := newGmoForTest(srv.URL, srv.URL)
	_, err := b.GetExecutions(context.Background(), "100")
	if err == nil {
		t.Fatal("expected parse error for malformed price")
	}
	if !strings.Contains(err.Error(), "price") {
		t.Errorf("error should mention field 'price'; got %v", err)
	}
}

func TestGetActiveOrders_MalformedTimestampReturnsError(t *testing.T) {
	srv := gmoTestServer(t, map[string]http.HandlerFunc{
		"/v1/activeOrders": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{
				"status":0,
				"data":{"list":[
					{"orderId":"1","symbol":"USD_JPY","side":"BUY","executionType":"MARKET","size":"100","price":"0","status":"NEW","timestamp":"garbage"}
				]},
				"responsetime":"2026-05-15T01:00:00.000Z"
			}`))
		},
	})
	defer srv.Close()

	b := newGmoForTest(srv.URL, srv.URL)
	_, err := b.GetActiveOrders(context.Background(), "USD_JPY")
	if err == nil {
		t.Fatal("expected parse error for malformed timestamp")
	}
	if !strings.Contains(err.Error(), "timestamp") {
		t.Errorf("error should mention field 'timestamp'; got %v", err)
	}
}

func TestGetKlines_MalformedRowReturnsError(t *testing.T) {
	srv := gmoTestServer(t, map[string]http.HandlerFunc{
		"/v1/klines": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{
				"status":0,
				"data":[
					{"openTime":"1747270800000","open":"150.0","high":"150.5","low":"149.9","close":"150.3","volume":"100"},
					{"openTime":"bad","open":"150.3","high":"150.7","low":"150.1","close":"150.5","volume":"100"}
				],
				"responsetime":"2026-05-15T01:00:00.000Z"
			}`))
		},
	})
	defer srv.Close()

	b := newGmoForTest(srv.URL, srv.URL)
	_, err := b.GetKlines(context.Background(), "USD_JPY", "5min", "20260515")
	if err == nil {
		t.Fatal("expected parse error for malformed openTime")
	}
	if !strings.Contains(err.Error(), "openTime") {
		t.Errorf("error should mention field 'openTime'; got %v", err)
	}
}

func TestGetAccountMargin_MalformedAvailableReturnsError(t *testing.T) {
	srv := gmoTestServer(t, map[string]http.HandlerFunc{
		"/v1/account/assets": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{
				"status":0,
				"data":{"availableAmount":"NaN!","marginRatio":"500","equity":"100000"},
				"responsetime":"2026-05-15T01:00:00.000Z"
			}`))
		},
	})
	defer srv.Close()

	b := newGmoForTest(srv.URL, srv.URL)
	_, err := b.GetAccountMargin(context.Background())
	if err == nil {
		t.Fatal("expected parse error for malformed availableAmount")
	}
	if !strings.Contains(err.Error(), "availableAmount") {
		t.Errorf("error should mention field 'availableAmount'; got %v", err)
	}
}
