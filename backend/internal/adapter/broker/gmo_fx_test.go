package broker

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"fx-bot/backend/internal/domain/order"
	"fx-bot/backend/internal/domain/position"
)

// gmoTestServer returns a httptest server that responds with given handlers
// keyed by HTTP path. Any unregistered path returns 404.
func gmoTestServer(t *testing.T, handlers map[string]http.HandlerFunc) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	for p, h := range handlers {
		mux.HandleFunc(p, h)
	}
	return httptest.NewServer(mux)
}

// newGmoForTest wires a broker against the given public/private base URLs.
// Rate limiter caps are bumped high to avoid slowing tests.
func newGmoForTest(publicBase, privateBase string) *GmoBroker {
	return NewGmoBroker(GmoBrokerConfig{
		PublicBaseURL:     publicBase,
		PrivateBaseURL:    privateBase,
		APIKey:            "test-api-key",
		APISecret:         "test-api-secret",
		PublicGetPerSec:   1000,
		PrivateGetPerSec:  1000,
		PrivatePostPerSec: 1000,
		Logger:            slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now: func() time.Time {
			return time.Date(2026, 5, 15, 1, 0, 0, 0, time.UTC)
		},
	})
}

// verifyPrivateHeaders re-computes the signature inside the test server to
// ensure GmoBroker's Sign call is identical to a fresh HMAC computation.
// Pass the already-read body so this helper doesn't consume r.Body.
func verifyPrivateHeaders(t *testing.T, r *http.Request, method, path string, body []byte) {
	t.Helper()
	got := r.Header.Get("API-SIGN")
	ts := r.Header.Get("API-TIMESTAMP")
	if got == "" || ts == "" {
		t.Errorf("missing API-SIGN or API-TIMESTAMP")
		return
	}
	mac := hmac.New(sha256.New, []byte("test-api-secret"))
	mac.Write([]byte(ts + method + path + string(body)))
	want := hex.EncodeToString(mac.Sum(nil))
	if got != want {
		t.Errorf("signature mismatch: got %s want %s", got, want)
	}
	if r.Header.Get("API-KEY") != "test-api-key" {
		t.Errorf("API-KEY missing")
	}
}

// readBody drains r.Body and returns its bytes; safe to call once per handler.
func readBody(t *testing.T, r *http.Request) []byte {
	t.Helper()
	bs, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return bs
}

// ---------------------------------------------------------------------------
// Public endpoints
// ---------------------------------------------------------------------------

func TestGetTicker_OK(t *testing.T) {
	srv := gmoTestServer(t, map[string]http.HandlerFunc{
		"/v1/ticker": func(w http.ResponseWriter, r *http.Request) {
			if got := r.URL.Query().Get("symbol"); got != "USD_JPY" {
				t.Errorf("symbol query: %q", got)
			}
			w.Write([]byte(`{
				"status":0,
				"data":[{"symbol":"USD_JPY","ask":"150.123","bid":"150.121","timestamp":"2026-05-15T01:00:00.000Z"}],
				"responsetime":"2026-05-15T01:00:00.000Z"
			}`))
		},
	})
	defer srv.Close()

	b := newGmoForTest(srv.URL, srv.URL)
	tk, err := b.GetTicker(context.Background(), "USD_JPY")
	if err != nil {
		t.Fatalf("GetTicker: %v", err)
	}
	if tk.Bid != 150.121 || tk.Ask != 150.123 {
		t.Errorf("bid/ask: %+v", tk)
	}
	if tk.Symbol != "USD_JPY" {
		t.Errorf("symbol: %s", tk.Symbol)
	}
}

func TestGetTicker_NonZeroStatus(t *testing.T) {
	srv := gmoTestServer(t, map[string]http.HandlerFunc{
		"/v1/ticker": func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`{"status":1,"messages":[{"message_code":"ERR-X","message_string":"bad"}]}`))
		},
	})
	defer srv.Close()

	b := newGmoForTest(srv.URL, srv.URL)
	_, err := b.GetTicker(context.Background(), "USD_JPY")
	if err == nil || !strings.Contains(err.Error(), "gmo api status=1") {
		t.Errorf("expected status err, got %v", err)
	}
}

func TestGetTicker_Non2xxHTTP(t *testing.T) {
	srv := gmoTestServer(t, map[string]http.HandlerFunc{
		"/v1/ticker": func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "boom", http.StatusInternalServerError)
		},
	})
	defer srv.Close()

	b := newGmoForTest(srv.URL, srv.URL)
	_, err := b.GetTicker(context.Background(), "USD_JPY")
	if err == nil || !strings.Contains(err.Error(), "gmo http 500") {
		t.Errorf("expected 500 err, got %v", err)
	}
}

// TestGetTicker_RetriesOnRateLimit verifies that a transient GMO rate-limit
// envelope (HTTP 200 + status=4 ERR-5003) on the PUBLIC ticker endpoint is
// retried transparently — mirroring the existing callPrivate behaviour — so a
// momentary server-side throttle does not surface as a "ticker err" / forced
// no_trade. The first attempt returns ERR-5003; the second returns a valid
// quote, so GetTicker must ultimately succeed.
func TestGetTicker_RetriesOnRateLimit(t *testing.T) {
	prev := publicRateLimitBackoffBase
	publicRateLimitBackoffBase = time.Millisecond // keep the test fast
	defer func() { publicRateLimitBackoffBase = prev }()

	var calls int
	srv := gmoTestServer(t, map[string]http.HandlerFunc{
		"/v1/ticker": func(w http.ResponseWriter, r *http.Request) {
			calls++
			if calls == 1 {
				w.Write([]byte(`{"status":4,"messages":[{"message_code":"ERR-5003","message_string":"Requests are too many."}]}`))
				return
			}
			w.Write([]byte(`{
				"status":0,
				"data":[{"symbol":"USD_JPY","ask":"150.123","bid":"150.121","timestamp":"2026-05-15T01:00:00.000Z"}],
				"responsetime":"2026-05-15T01:00:00.000Z"
			}`))
		},
	})
	defer srv.Close()

	b := newGmoForTest(srv.URL, srv.URL)
	tk, err := b.GetTicker(context.Background(), "USD_JPY")
	if err != nil {
		t.Fatalf("GetTicker after one rate-limit retry: %v", err)
	}
	if tk.Bid != 150.121 || tk.Ask != 150.123 {
		t.Errorf("bid/ask after retry: %+v", tk)
	}
	if calls != 2 {
		t.Errorf("expected 2 public calls (1 throttled + 1 OK), got %d", calls)
	}
}

// TestGetTicker_RateLimitRetry_LogsAtDebugNotWarn pins the log severity of the
// transparent public rate-limit retry. The retry RECOVERS the ticker (no
// dropped price/LLM cycle), so it is an expected, handled event — not a WARN.
// The price loop polls every second per symbol, so a WARN here would spam the
// log. The retry logic itself stays; only its severity drops to Debug
// (invisible at LOG_LEVEL=warn or above).
func TestGetTicker_RateLimitRetry_LogsAtDebugNotWarn(t *testing.T) {
	prev := publicRateLimitBackoffBase
	publicRateLimitBackoffBase = time.Millisecond
	defer func() { publicRateLimitBackoffBase = prev }()

	var calls int
	srv := gmoTestServer(t, map[string]http.HandlerFunc{
		"/v1/ticker": func(w http.ResponseWriter, r *http.Request) {
			calls++
			if calls == 1 {
				w.Write([]byte(`{"status":4,"messages":[{"message_code":"ERR-5003","message_string":"Requests are too many."}]}`))
				return
			}
			w.Write([]byte(`{"status":0,"data":[{"symbol":"USD_JPY","ask":"150.123","bid":"150.121","timestamp":"2026-05-15T01:00:00.000Z"}],"responsetime":"2026-05-15T01:00:00.000Z"}`))
		},
	})
	defer srv.Close()

	var logbuf strings.Builder
	b := NewGmoBroker(GmoBrokerConfig{
		PublicBaseURL:   srv.URL,
		PrivateBaseURL:  srv.URL,
		APIKey:          "test-api-key",
		APISecret:       "test-api-secret",
		PublicGetPerSec: 1000, PrivateGetPerSec: 1000, PrivatePostPerSec: 1000,
		Logger: slog.New(slog.NewTextHandler(&logbuf, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Now:    func() time.Time { return time.Date(2026, 5, 15, 1, 0, 0, 0, time.UTC) },
	})
	if _, err := b.GetTicker(context.Background(), "USD_JPY"); err != nil {
		t.Fatalf("GetTicker after one rate-limit retry: %v", err)
	}

	out := logbuf.String()
	if !strings.Contains(out, "gmo_public_rate_limit_retry") {
		t.Fatalf("expected the retry to still be logged (at debug): %q", out)
	}
	if strings.Contains(out, "level=WARN msg=gmo_public_rate_limit_retry") {
		t.Errorf("retry must NOT log at WARN (spams live log 4x/min); want DEBUG: %q", out)
	}
	if !strings.Contains(out, "level=DEBUG msg=gmo_public_rate_limit_retry") {
		t.Errorf("retry should log at DEBUG: %q", out)
	}
}

func TestGetKlines_OK(t *testing.T) {
	srv := gmoTestServer(t, map[string]http.HandlerFunc{
		"/v1/klines": func(w http.ResponseWriter, r *http.Request) {
			q := r.URL.Query()
			if q.Get("symbol") != "USD_JPY" || q.Get("interval") != "1min" || q.Get("date") != "20260515" {
				t.Errorf("query: %v", q)
			}
			w.Write([]byte(`{
				"status":0,
				"data":[
					{"openTime":"1778806800000","open":"150.10","high":"150.20","low":"150.05","close":"150.15","volume":"100"},
					{"openTime":"1778806860000","open":"150.15","high":"150.25","low":"150.12","close":"150.20","volume":"110"}
				]
			}`))
		},
	})
	defer srv.Close()

	b := newGmoForTest(srv.URL, srv.URL)
	ks, err := b.GetKlines(context.Background(), "USD_JPY", "1min", "20260515")
	if err != nil {
		t.Fatalf("GetKlines: %v", err)
	}
	if len(ks) != 2 {
		t.Fatalf("len=%d", len(ks))
	}
	if ks[0].Open != 150.10 || ks[0].Close != 150.15 || ks[0].High != 150.20 {
		t.Errorf("kline[0]: %+v", ks[0])
	}
	if ks[0].Interval != "1min" || ks[0].Symbol != "USD_JPY" {
		t.Errorf("meta: %+v", ks[0])
	}
}

// ---------------------------------------------------------------------------
// Private endpoints — verifies signature round-trip
// ---------------------------------------------------------------------------

// The endpoint is /v1/account/assets per the fxdocs — NOT /v1/account/margin
// (a crypto API path; returns 404 on forex-api.coin.z.com). The
// response field is `availableAmount` (not `available`).
func TestGetAccountMargin_HitsAssetsEndpointWithCorrectFieldNames(t *testing.T) {
	var capturedPath string
	srv := gmoTestServer(t, map[string]http.HandlerFunc{
		"/v1/account/assets": func(w http.ResponseWriter, r *http.Request) {
			capturedPath = r.URL.Path
			verifyPrivateHeaders(t, r, http.MethodGet, "/v1/account/assets", readBody(t, r))
			// Real response shape from forex-api (values are synthetic):
			// equity / availableAmount / balance / margin / marginRatio /
			// positionLossGain / totalSwap / transferableAmount / estimatedTradeFee
			w.Write([]byte(`{"status":0,"data":{"equity":"1050000","availableAmount":"1000000","balance":"1000000","margin":"50000","marginRatio":"50.5","positionLossGain":"0","totalSwap":"0","transferableAmount":"1000000","estimatedTradeFee":"0"}}`))
		},
	})
	defer srv.Close()

	b := newGmoForTest(srv.URL, srv.URL)
	m, err := b.GetAccountMargin(context.Background())
	if err != nil {
		t.Fatalf("GetAccountMargin: %v", err)
	}
	if capturedPath != "/v1/account/assets" {
		t.Errorf("path: got %q, want /v1/account/assets", capturedPath)
	}
	if m.AvailableJPY != 1_000_000 || m.MarginRatio != 50.5 || m.Equity != 1_050_000 {
		t.Errorf("margin: %+v", m)
	}
}

func TestGetOpenPositions_OK(t *testing.T) {
	srv := gmoTestServer(t, map[string]http.HandlerFunc{
		"/v1/openPositions": func(w http.ResponseWriter, r *http.Request) {
			verifyPrivateHeaders(t, r, http.MethodGet, "/v1/openPositions", readBody(t, r))
			w.Write([]byte(`{"status":0,"data":{"list":[
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
	if len(ps) != 1 || ps[0].BrokerPositionID != "123" || ps[0].Side != order.SideBuy || ps[0].Quantity != 100 {
		t.Errorf("position: %+v", ps)
	}
}

func TestGetActiveOrders_Empty(t *testing.T) {
	srv := gmoTestServer(t, map[string]http.HandlerFunc{
		"/v1/activeOrders": func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`{"status":0,"data":{"list":[]}}`))
		},
	})
	defer srv.Close()

	b := newGmoForTest(srv.URL, srv.URL)
	os, err := b.GetActiveOrders(context.Background(), "USD_JPY")
	if err != nil {
		t.Fatalf("GetActiveOrders: %v", err)
	}
	if len(os) != 0 {
		t.Errorf("expected empty, got %d", len(os))
	}
}

func TestGetExecutions_OK(t *testing.T) {
	srv := gmoTestServer(t, map[string]http.HandlerFunc{
		"/v1/executions": func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("orderId") != "ord-1" {
				t.Errorf("orderId query: %s", r.URL.Query().Get("orderId"))
			}
			w.Write([]byte(`{"status":0,"data":{"list":[
				{"executionId":"e-1","orderId":"ord-1","positionId":"p-1","symbol":"USD_JPY","side":"BUY","size":"100","price":"150.123","timestamp":"2026-05-15T01:00:00.000Z"}
			]}}`))
		},
	})
	defer srv.Close()

	b := newGmoForTest(srv.URL, srv.URL)
	xs, err := b.GetExecutions(context.Background(), "ord-1")
	if err != nil {
		t.Fatalf("GetExecutions: %v", err)
	}
	if len(xs) != 1 || xs[0].ExecutionID != "e-1" || xs[0].Price != 150.123 {
		t.Errorf("execution: %+v", xs)
	}
}

// /v1/order with executionType=IFDOCO returns ERR-5106
// "Invalid request parameter. executionType". GMO Forex
// IFDOCO is a SEPARATE endpoint (/v1/ifoOrder) with a different body shape
// — `firstSide` / `firstExecutionType` / `firstPrice` / `secondLimitPrice`
// / `secondStopPrice` instead of `executionType: IFDOCO` + `settlePosition[]`.
// The fxdocs sample also shows `firstExecutionType` only accepts LIMIT or
// STOP — MARKET entry through IFDOCO is not supported. The production bot
// currently uses MARKET+OCO instead; this test keeps the legacy adapter path
// pinned in case we re-enable IFDOCO for larger sizes later.
func TestPlaceOrder_IFDOCO_Payload(t *testing.T) {
	var capturedBody []byte
	var capturedPath string
	srv := gmoTestServer(t, map[string]http.HandlerFunc{
		"/v1/ifoOrder": func(w http.ResponseWriter, r *http.Request) {
			capturedBody = readBody(t, r)
			capturedPath = r.URL.Path
			verifyPrivateHeaders(t, r, http.MethodPost, "/v1/ifoOrder", capturedBody)
			w.Write([]byte(`{"status":0,"data":{"rootOrderId":123456789}}`))
		},
	})
	defer srv.Close()

	b := newGmoForTest(srv.URL, srv.URL)
	req := order.PlaceOrderRequest{
		Symbol:     "USD_JPY",
		Side:       order.SideBuy,
		Type:       order.OrderTypeIFDOCO,
		Quantity:   100,
		Price:      158.860, // entry LIMIT at current ask (or better)
		TakeProfit: 158.920,
		StopLoss:   158.810,
	}
	ord, err := b.PlaceOrder(context.Background(), req)
	if err != nil {
		t.Fatalf("PlaceOrder: %v", err)
	}
	if capturedPath != "/v1/ifoOrder" {
		t.Errorf("path: got %q, want /v1/ifoOrder", capturedPath)
	}
	if ord.OrderID != "123456789" {
		t.Errorf("orderID: got %q, want 123456789 (rootOrderId rendered as decimal string)", ord.OrderID)
	}

	var payload map[string]any
	if err := json.Unmarshal(capturedBody, &payload); err != nil {
		t.Fatalf("payload not json: %v", err)
	}
	// New /v1/ifoOrder body shape per GMO fxdocs.
	if payload["symbol"] != "USD_JPY" {
		t.Errorf("symbol: %v", payload["symbol"])
	}
	if payload["firstSide"] != "BUY" {
		t.Errorf("firstSide: %v", payload["firstSide"])
	}
	if payload["firstExecutionType"] != "LIMIT" {
		t.Errorf("firstExecutionType must be LIMIT (MARKET unsupported by /v1/ifoOrder); got %v", payload["firstExecutionType"])
	}
	if payload["firstSize"] != "100" {
		t.Errorf("firstSize: %v", payload["firstSize"])
	}
	// USD_JPY tickSize 0.001 → fixed 3-decimal payload (GMO ERR-5114
	// otherwise). Trailing zeros must be preserved.
	if payload["firstPrice"] != "158.860" {
		t.Errorf("firstPrice: got %v, want 158.860 (fixed 3-decimal)", payload["firstPrice"])
	}
	if payload["secondSize"] != "100" {
		t.Errorf("secondSize: %v", payload["secondSize"])
	}
	if payload["secondLimitPrice"] != "158.920" {
		t.Errorf("secondLimitPrice (TP): got %v, want 158.920 (fixed 3-decimal)", payload["secondLimitPrice"])
	}
	if payload["secondStopPrice"] != "158.810" {
		t.Errorf("secondStopPrice (SL): got %v, want 158.810 (fixed 3-decimal)", payload["secondStopPrice"])
	}
	// The legacy fields must NOT be present in the new body.
	if _, present := payload["executionType"]; present {
		t.Errorf("legacy executionType must not appear in /v1/ifoOrder body")
	}
	if _, present := payload["settlePosition"]; present {
		t.Errorf("legacy settlePosition must not appear in /v1/ifoOrder body")
	}
}

func TestPlaceOrder_IFDOCO_RequiresPriceAndTPAndSL(t *testing.T) {
	b := newGmoForTest("http://unused", "http://unused")
	cases := []order.PlaceOrderRequest{
		{Symbol: "USD_JPY", Side: order.SideBuy, Type: order.OrderTypeIFDOCO, Quantity: 100, Price: 158.86, TakeProfit: 158.92},    // no SL
		{Symbol: "USD_JPY", Side: order.SideBuy, Type: order.OrderTypeIFDOCO, Quantity: 100, Price: 158.86, StopLoss: 158.81},      // no TP
		{Symbol: "USD_JPY", Side: order.SideBuy, Type: order.OrderTypeIFDOCO, Quantity: 100, TakeProfit: 158.92, StopLoss: 158.81}, // no Price (LIMIT entry needs it)
	}
	for i, req := range cases {
		_, err := b.PlaceOrder(context.Background(), req)
		if err == nil || !strings.Contains(err.Error(), "IFDOCO requires") {
			t.Errorf("case %d: expected validation err, got %v", i, err)
		}
	}
}

func TestPlaceOrder_Market(t *testing.T) {
	var capturedBody []byte
	srv := gmoTestServer(t, map[string]http.HandlerFunc{
		"/v1/order": func(w http.ResponseWriter, r *http.Request) {
			capturedBody, _ = io.ReadAll(r.Body)
			w.Write([]byte(`{"status":0,"data":"market-order-id"}`))
		},
	})
	defer srv.Close()

	b := newGmoForTest(srv.URL, srv.URL)
	req := order.PlaceOrderRequest{
		Symbol:   "USD_JPY",
		Side:     order.SideSell,
		Type:     order.OrderTypeMarket,
		Quantity: 100,
	}
	ord, err := b.PlaceOrder(context.Background(), req)
	if err != nil {
		t.Fatalf("PlaceOrder: %v", err)
	}
	if ord.OrderID != "market-order-id" {
		t.Errorf("orderID: %q", ord.OrderID)
	}
	var payload map[string]any
	json.Unmarshal(capturedBody, &payload)
	if payload["executionType"] != "MARKET" || payload["side"] != "SELL" {
		t.Errorf("payload: %v", payload)
	}
	if _, ok := payload["settlePosition"]; ok {
		t.Errorf("market should not include settlePosition")
	}
}

func TestClosePosition_BuildsCloseRequest(t *testing.T) {
	var capturedBody []byte
	srv := gmoTestServer(t, map[string]http.HandlerFunc{
		"/v1/closeOrder": func(w http.ResponseWriter, r *http.Request) {
			capturedBody, _ = io.ReadAll(r.Body)
			w.Write([]byte(`{"status":0,"data":"close-order-id"}`))
		},
	})
	defer srv.Close()

	b := newGmoForTest(srv.URL, srv.URL)
	// GMO Forex requires positionId as a NUMBER, so fixture
	// ids must be numeric. Real broker positionIds are integers (e.g. the
	// synthetic numeric id 90000001 used in gmo_fx_positionid_number_test.go).
	pos := position.Position{
		BrokerPositionID: "1000042",
		Symbol:           "USD_JPY",
		Side:             order.SideBuy,
		Quantity:         100,
	}
	ord, err := b.ClosePosition(context.Background(), pos)
	if err != nil {
		t.Fatalf("ClosePosition: %v", err)
	}
	if ord.OrderID != "close-order-id" {
		t.Errorf("orderID: %q", ord.OrderID)
	}
	if ord.Side != order.SideSell {
		t.Errorf("close side should be opposite (SELL), got %s", ord.Side)
	}
	var payload map[string]any
	json.Unmarshal(capturedBody, &payload)
	if payload["side"] != "SELL" {
		t.Errorf("payload side: %v", payload["side"])
	}
	sp := payload["settlePosition"].([]any)
	entry := sp[0].(map[string]any)
	// positionId on the wire must be a JSON NUMBER (= float64 after Unmarshal)
	// not a quoted string — that's the GMO Forex API contract.
	idNum, ok := entry["positionId"].(float64)
	if !ok || int64(idNum) != 1000042 {
		t.Errorf("settlePosition.positionId: got %v (type %T), want 1000042 as JSON number", entry["positionId"], entry["positionId"])
	}
	if entry["size"] != "100" {
		t.Errorf("settlePosition.size: %v", entry["size"])
	}
}

func TestClosePosition_MissingBrokerID(t *testing.T) {
	b := newGmoForTest("http://unused", "http://unused")
	_, err := b.ClosePosition(context.Background(), position.Position{Symbol: "USD_JPY"})
	if err == nil || !strings.Contains(err.Error(), "broker position id") {
		t.Errorf("expected err, got %v", err)
	}
}

// Per GMO Forex fxdocs the endpoint is /v1/cancelOrders (plural)
// and the body shape is `{"rootOrderIds": [<num>, ...]}` (array of NUMBER
// order ids), NOT /v1/cancelOrder with `{"orderId": "<str>"}`. The wrong
// path 404s on every cancel attempt, so the Live close saga (cancel
// TP/SL legs → market close) would fail at the first leg cancel — and
// paper mode never exercises it.
func TestCancelOrder_OK_PostsToCancelOrdersWithRootOrderIdsArray(t *testing.T) {
	var capturedBody []byte
	var capturedPath string
	srv := gmoTestServer(t, map[string]http.HandlerFunc{
		"/v1/cancelOrders": func(w http.ResponseWriter, r *http.Request) {
			capturedBody, _ = io.ReadAll(r.Body)
			capturedPath = r.URL.Path
			w.Write([]byte(`{"status":0}`))
		},
	})
	defer srv.Close()

	b := newGmoForTest(srv.URL, srv.URL)
	err := b.CancelOrder(context.Background(), "1234567890")
	if err != nil {
		t.Fatalf("CancelOrder: %v", err)
	}
	if capturedPath != "/v1/cancelOrders" {
		t.Errorf("path: got %q, want /v1/cancelOrders", capturedPath)
	}
	var payload map[string]any
	json.Unmarshal(capturedBody, &payload)
	ids, ok := payload["rootOrderIds"].([]any)
	if !ok {
		t.Fatalf("rootOrderIds: not an array, got %v", payload["rootOrderIds"])
	}
	if len(ids) != 1 {
		t.Fatalf("rootOrderIds length: got %d, want 1", len(ids))
	}
	// JSON numbers unmarshal as float64 by default.
	idNum, ok := ids[0].(float64)
	if !ok || int64(idNum) != 1234567890 {
		t.Errorf("rootOrderIds[0]: got %v (type %T), want 1234567890 as JSON number", ids[0], ids[0])
	}
	if _, present := payload["orderId"]; present {
		t.Errorf("legacy `orderId` must not appear in /v1/cancelOrders body")
	}
}

func TestCancelOrder_RejectsNonNumericOrderID(t *testing.T) {
	b := newGmoForTest("http://unused", "http://unused")
	err := b.CancelOrder(context.Background(), "ord-99") // legacy string format from paper era
	if err == nil {
		t.Fatal("CancelOrder must reject non-numeric ids — GMO Forex rootOrderIds is a NUMBER array")
	}
}

func TestPrivateCall_UnauthorizedReturnsSignError(t *testing.T) {
	srv := gmoTestServer(t, map[string]http.HandlerFunc{
		"/v1/account/assets": func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "bad signature", http.StatusUnauthorized)
		},
	})
	defer srv.Close()

	b := newGmoForTest(srv.URL, srv.URL)
	_, err := b.GetAccountMargin(context.Background())
	if err == nil {
		t.Fatalf("expected SignError")
	}
	var se *SignError
	ok := false
	if e, isSign := err.(*SignError); isSign {
		ok = true
		se = e
	}
	if !ok || se.Status != http.StatusUnauthorized {
		t.Errorf("expected SignError 401, got %T %v", err, err)
	}
}

// Sanity helper used in some assertions — make sure url.Values used in callPublic
// doesn't include keys we didn't pass.
var _ = url.Values{}
