// Package broker contains concrete implementations of port.Broker.
//
// gmo_fx.go: GMO Coin Forex API client. Both public (no auth) and private
// (HMAC-SHA256 signed) endpoints are implemented.
//
// References:
//
//	Public:  https://forex-api.coin.z.com/public
//	Private: https://forex-api.coin.z.com/private
//	Docs:    https://api.coin.z.com/docs/
//
// All paths used here are the path-portion only (e.g. "/v1/ticker") because
// that's the GMO signing convention.
package broker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"fx-bot/backend/internal/domain/market"
	"fx-bot/backend/internal/domain/order"
	"fx-bot/backend/internal/domain/position"
	"fx-bot/backend/internal/port"
)

// newIPv4OnlyHTTPClient builds an http.Client whose Transport refuses
// IPv6 connections. Background: macOS getaddrinfo prefers AAAA records,
// so the default Go HTTP client will reach GMO over IPv6 whenever the
// AAAA is returned. GMO's IP allowlist is IPv4-only in practice — the
// IPv6 source IP is treated as out-of-allowlist and every private call
// fails with ERR-5012 "Invalid API-KEY, IP, or permissions for action."
// (confirmed by smoke test against the live API). Forcing tcp4 here makes the same
// allowlisted IPv4 source IP get used regardless of DNS preference.
func newIPv4OnlyHTTPClient(timeout time.Duration) *http.Client {
	dialer := &net.Dialer{Timeout: timeout, KeepAlive: 30 * time.Second}
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
				return dialer.DialContext(ctx, "tcp4", addr)
			},
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          100,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
		},
	}
}

// GmoBrokerConfig holds the construction parameters for GmoBroker. The base
// URLs come from bot_config.yaml; API key/secret come from environment.
type GmoBrokerConfig struct {
	PublicBaseURL     string
	PrivateBaseURL    string
	APIKey            string
	APISecret         string
	PublicGetPerSec   float64 // typically 6
	PrivateGetPerSec  float64 // typically 6
	PrivatePostPerSec float64 // default 0.5 (see NewGmoBroker)
	HTTPClient        *http.Client
	Logger            *slog.Logger
	Now               func() time.Time // injected clock for signing
}

// GmoBroker speaks the GMO Coin Forex API. Implements port.Broker.
type GmoBroker struct {
	publicBase  string
	privateBase string
	apiKey      string
	apiSecret   string
	publicGet   *TokenBucket
	privateGet  *TokenBucket
	privatePost *TokenBucket
	http        *http.Client
	logger      *slog.Logger
	now         func() time.Time
}

// NewGmoBroker constructs a broker from cfg. Defaults are filled in where
// possible (HTTP client, logger, clock).
func NewGmoBroker(cfg GmoBrokerConfig) *GmoBroker {
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = newIPv4OnlyHTTPClient(15 * time.Second)
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.PublicGetPerSec == 0 {
		cfg.PublicGetPerSec = 6
	}
	if cfg.PrivateGetPerSec == 0 {
		cfg.PrivateGetPerSec = 6
	}
	if cfg.PrivatePostPerSec == 0 {
		// GMO Forex の公開仕様は private POST 1/s だが、実運用で連続 POST
		// (e.g. /v1/order MARKET → /v1/closeOrder OCO の 1 秒未満間隔) で
		// ERR-5003 "Requests are too many" を返してくる事象を確認している。
		// 0.5/s = 2 秒間隔に下げて余裕を持たせる。さらに callPrivate 側で
		// ERR-5003 自動 retry を持ち、突発的な揺らぎも吸収する。
		cfg.PrivatePostPerSec = 0.5
	}
	// TokenBucket consumes in 1-token units, so capacity must be ≥ 1 even when
	// the refill rate is < 1/s (= the GMO Forex private POST case at 0.5/s
	// above). Otherwise tokens are capped at sub-1 and Wait
	// loops forever waiting for a level it can never reach.
	bucketCap := func(rate float64) float64 {
		if rate < 1 {
			return 1
		}
		return rate
	}
	return &GmoBroker{
		publicBase:  cfg.PublicBaseURL,
		privateBase: cfg.PrivateBaseURL,
		apiKey:      cfg.APIKey,
		apiSecret:   cfg.APISecret,
		publicGet:   NewTokenBucket(bucketCap(cfg.PublicGetPerSec), cfg.PublicGetPerSec),
		privateGet:  NewTokenBucket(bucketCap(cfg.PrivateGetPerSec), cfg.PrivateGetPerSec),
		privatePost: NewTokenBucket(bucketCap(cfg.PrivatePostPerSec), cfg.PrivatePostPerSec),
		http:        cfg.HTTPClient,
		logger:      cfg.Logger,
		now:         cfg.Now,
	}
}

// flexString is a JSON-unmarshal helper that accepts EITHER a JSON string
// ("123") or a JSON number (123) and stores the textual representation.
// Background: GMO Forex API is inconsistent — positionId/orderId/
// executionId arrive as JSON numbers in /v1/openPositions and
// /v1/executions but as strings in others. Domain code wants strings
// (BrokerPositionID etc.), so we accept both at the boundary and convert
// uniformly. Smoke-confirmed against the live API.
type flexString string

func (f *flexString) UnmarshalJSON(data []byte) error {
	if len(data) == 0 {
		return nil
	}
	// JSON string: data starts with `"`. Defer to the default decoder.
	if data[0] == '"' {
		var s string
		if err := json.Unmarshal(data, &s); err != nil {
			return err
		}
		*f = flexString(s)
		return nil
	}
	if string(data) == "null" {
		*f = ""
		return nil
	}
	// Accept only JSON primitives (number/true/false). Object/array tokens
	// must be rejected so callers that try `Unmarshal(env.Data, &flex)` as
	// a probe can fall through to a struct-shaped retry (e.g. IFDOCO's
	// data={"rootOrderId":...} response shouldn't get captured as the raw
	// JSON string "{\"rootOrderId\":123}" here).
	if data[0] == '{' || data[0] == '[' {
		return fmt.Errorf("flexString: refusing to decode non-primitive JSON token: %s", string(data))
	}
	*f = flexString(string(data))
	return nil
}

// extractOrderID pulls the new order's id out of a GMO POST response's
// `data` field. GMO uses three different shapes depending on the endpoint:
//
//	scalar (string or number)            — legacy /v1/order
//	object {orderId|rootOrderId}         — /v1/ifoOrder, /v1/cancelOrders.success[]
//	array of order-detail objects        — /v1/order (live response carries the
//	                                       full executed order in [data][0])
//
// Returns "" when no id can be recovered — callers should treat that as
// a critical-failure signal (they cannot safely chain ResolveExecution).
func extractOrderID(data []byte) string {
	if len(data) == 0 {
		return ""
	}
	// 1) scalar (string or number)
	var scalar flexString
	if err := json.Unmarshal(data, &scalar); err == nil && string(scalar) != "" {
		return string(scalar)
	}
	// 2) single object with orderId / rootOrderId
	var obj struct {
		OrderID     flexString `json:"orderId"`
		RootOrderID flexString `json:"rootOrderId"`
	}
	if err := json.Unmarshal(data, &obj); err == nil {
		if string(obj.OrderID) != "" {
			return string(obj.OrderID)
		}
		if string(obj.RootOrderID) != "" {
			return string(obj.RootOrderID)
		}
	}
	// 3) array of order-detail objects — take the first element's orderId
	//    (or rootOrderId as fallback).
	var arr []struct {
		OrderID     flexString `json:"orderId"`
		RootOrderID flexString `json:"rootOrderId"`
	}
	if err := json.Unmarshal(data, &arr); err == nil && len(arr) > 0 {
		if string(arr[0].OrderID) != "" {
			return string(arr[0].OrderID)
		}
		if string(arr[0].RootOrderID) != "" {
			return string(arr[0].RootOrderID)
		}
	}
	return ""
}

// gmoEnvelope is the standard GMO response wrapper.
type gmoEnvelope struct {
	Status       int             `json:"status"`
	Data         json.RawMessage `json:"data"`
	ResponseTime string          `json:"responsetime"`
	Messages     []struct {
		MessageCode   string `json:"message_code"`
		MessageString string `json:"message_string"`
	} `json:"messages"`
}

// gmoPositionNotFoundErr is GMO's message_code for "Not found position" — the
// position no longer exists broker-side (already settled).
const gmoPositionNotFoundErr = "ERR-254"

// errorFromEnvelope returns nil when status == 0, else a descriptive error.
func errorFromEnvelope(env *gmoEnvelope) error {
	if env.Status == 0 {
		return nil
	}
	if len(env.Messages) > 0 {
		msg := env.Messages[0]
		// ERR-254 "Not found position": wrap the shared sentinel so the close
		// saga can treat a close-time ERR-254 as a benign already-closed race
		// (settle leg filled first) rather than a naked position → emergency_stop.
		if msg.MessageCode == gmoPositionNotFoundErr {
			return fmt.Errorf("gmo api status=%d: %s (%s): %w",
				env.Status, msg.MessageString, msg.MessageCode, port.ErrBrokerPositionNotFound)
		}
		return fmt.Errorf("gmo api status=%d: %s (%s)",
			env.Status, msg.MessageString, msg.MessageCode)
	}
	return fmt.Errorf("gmo api status=%d", env.Status)
}

// decodeEnvelope unmarshals a GMO response body into the standard envelope and
// runs the status check, returning the ready-to-decode envelope (use env.Data
// for the per-endpoint payload). label prefixes the unmarshal error so each
// endpoint keeps its own "<label> decode env" diagnostic. Centralizes the
// decode + status-check ritual repeated by every endpoint method so a future
// tweak (e.g. a new status code) cannot silently skip the check on one path.
func decodeEnvelope(label string, body []byte) (*gmoEnvelope, error) {
	var env gmoEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, fmt.Errorf("%s decode env: %w", label, err)
	}
	if err := errorFromEnvelope(&env); err != nil {
		return nil, err
	}
	return &env, nil
}

// publicRateLimitBackoffBase is the first backoff between callPublic retries on
// a GMO ERR-5003 ("Requests are too many") envelope. Public GETs (ticker/klines)
// are latency-sensitive — the price loop polls every second — so the backoff is
// deliberately much shorter than callPrivate's 1s→2s→4s schedule: just long
// enough to ride out GMO's momentary server-side throttle without blocking a
// tick for whole seconds. Exposed as a var so tests can shrink it.
var publicRateLimitBackoffBase = 250 * time.Millisecond

// callPublic executes a GET against the public base, rate-limited, with a
// bounded short-backoff retry on the GMO ERR-5003 rate-limit envelope (HTTP 200
// + status=4). The bot-side TokenBucket already paces requests; this retry
// covers the case where GMO's actual per-IP limit is briefly stricter than the
// bucket's rate, so a transient throttle no longer surfaces as a "ticker err"
// (or a dropped LLM decision cycle). Backoff: 250ms → 500ms (3 attempts total).
func (b *GmoBroker) callPublic(ctx context.Context, path string, query url.Values) ([]byte, error) {
	const maxAttempts = 3
	var lastBody []byte
	for attempt := 0; attempt < maxAttempts; attempt++ {
		body, err := b.callPublicOnce(ctx, path, query)
		if err != nil {
			return nil, err
		}
		lastBody = body
		if bytes.Contains(body, []byte(gmoRateLimitErr)) && attempt < maxAttempts-1 {
			backoff := publicRateLimitBackoffBase * time.Duration(1<<attempt) // 250ms, 500ms
			// Debug, not Warn: the retry RECOVERS the ticker transparently (no
			// dropped price/LLM cycle), and the price loop polls every second per
			// symbol, so at Warn this would spam the log for a handled, expected
			// event. Invisible at LOG_LEVEL=warn or above; available for debugging.
			b.logger.Debug("gmo_public_rate_limit_retry",
				"path", path, "attempt", attempt+1, "backoff", backoff)
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(backoff):
			}
			continue
		}
		return body, nil
	}
	return lastBody, nil
}

// callPublicOnce is the single-attempt public GET used by callPublic. Kept
// separate so the retry loop stays out of the request-building path (mirrors
// callPrivate / callPrivateOnce).
func (b *GmoBroker) callPublicOnce(ctx context.Context, path string, query url.Values) ([]byte, error) {
	if err := b.publicGet.Wait(ctx); err != nil {
		return nil, fmt.Errorf("rate limit (public get): %w", err)
	}
	u := b.publicBase + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	resp, err := b.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("gmo http %d: %s", resp.StatusCode, string(body))
	}
	return body, nil
}

// gmoRateLimitErr is the GMO error code returned (with HTTP 200 + JSON
// envelope status=4) when the server-side rate limiter rejects the request.
// Bot's TokenBucket is best-effort — GMO sometimes throttles tighter than
// the public spec suggests. Detecting the marker in the response body lets
// us retry transparently with exponential backoff.
const gmoRateLimitErr = "ERR-5003"

// callPrivate executes a private (signed) GET or POST, with built-in retry
// on the GMO rate-limit envelope (status=4 ERR-5003).
//
// Retry policy: up to 4 total attempts, with backoff 1s → 2s → 4s between
// attempts. The bot-side TokenBucket already paces requests; retry covers
// the case where GMO's actual limit is stricter than the bucket's rate.
func (b *GmoBroker) callPrivate(ctx context.Context, method, path string, query url.Values, body []byte) ([]byte, error) {
	const maxAttempts = 4
	var lastBody []byte
	for attempt := 0; attempt < maxAttempts; attempt++ {
		respBody, err := b.callPrivateOnce(ctx, method, path, query, body)
		if err != nil {
			return nil, err
		}
		lastBody = respBody
		// Detect ERR-5003 in the envelope. We only need a substring match;
		// the alternative (full JSON unmarshal here) duplicates work the
		// caller does anyway.
		if bytes.Contains(respBody, []byte(gmoRateLimitErr)) && attempt < maxAttempts-1 {
			backoff := time.Duration(1<<attempt) * time.Second // 1s, 2s, 4s
			b.logger.Warn("gmo_rate_limit_retry",
				"method", method, "path", path, "attempt", attempt+1, "backoff", backoff)
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(backoff):
			}
			continue
		}
		return respBody, nil
	}
	return lastBody, nil
}

// callPrivateOnce is the single-attempt HTTP call used by callPrivate.
// Kept separate so retry logic stays out of the request-building path.
func (b *GmoBroker) callPrivateOnce(ctx context.Context, method, path string, query url.Values, body []byte) ([]byte, error) {
	bucket := b.privateGet
	if method == http.MethodPost {
		bucket = b.privatePost
	}
	if err := bucket.Wait(ctx); err != nil {
		return nil, fmt.Errorf("rate limit (private %s): %w", method, err)
	}
	u := b.privateBase + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rdr)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	// IMPORTANT: signature path is the path-only portion (no host, no query).
	headers := BuildPrivateHeaders(b.apiKey, b.apiSecret, method, path, body, b.now())
	for k, v := range headers {
		for _, vv := range v {
			req.Header.Set(k, vv)
		}
	}
	resp, err := b.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http: %w", err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, &SignError{Status: resp.StatusCode, Message: string(respBody)}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("gmo http %d: %s", resp.StatusCode, string(respBody))
	}
	return respBody, nil
}

// ---------------------------------------------------------------------------
// Public market data
// ---------------------------------------------------------------------------

// GetTicker fetches the current bid/ask for symbol.
//
// Response shape:
//
//	{"status":0,"data":[{"symbol":"USD_JPY","ask":"...","bid":"...","timestamp":"..."}],...}
func (b *GmoBroker) GetTicker(ctx context.Context, symbol string) (*market.Ticker, error) {
	body, err := b.callPublic(ctx, "/v1/ticker", url.Values{"symbol": []string{symbol}})
	if err != nil {
		return nil, err
	}
	env, err := decodeEnvelope("ticker", body)
	if err != nil {
		return nil, err
	}
	var rows []struct {
		Symbol    string `json:"symbol"`
		Ask       string `json:"ask"`
		Bid       string `json:"bid"`
		Timestamp string `json:"timestamp"`
	}
	if err := json.Unmarshal(env.Data, &rows); err != nil {
		return nil, fmt.Errorf("ticker decode data: %w", err)
	}
	for _, r := range rows {
		if r.Symbol != symbol {
			continue
		}
		ask, err := parseGMOFloat("ticker.ask", r.Ask)
		if err != nil {
			return nil, err
		}
		bid, err := parseGMOFloat("ticker.bid", r.Bid)
		if err != nil {
			return nil, err
		}
		ts, err := parseGMOTime("ticker.timestamp", r.Timestamp)
		if err != nil {
			return nil, err
		}
		return &market.Ticker{
			Symbol:    r.Symbol,
			Ask:       ask,
			Bid:       bid,
			Timestamp: ts,
		}, nil
	}
	return nil, fmt.Errorf("ticker: symbol %q not in response", symbol)
}

// GetKlines fetches OHLC candles for the given interval/date.
//
// GMO klines: requires symbol, priceType (BID/ASK), interval, date (YYYYMMDD).
// We default priceType=ASK for entry-side simulation.
//
// Response: data is an array of {openTime, open, high, low, close, volume}
// with openTime in ms epoch as a string.
func (b *GmoBroker) GetKlines(ctx context.Context, symbol, interval, dateYYYYMMDD string) ([]market.Kline, error) {
	q := url.Values{
		"symbol":    []string{symbol},
		"priceType": []string{"ASK"},
		"interval":  []string{interval},
		"date":      []string{dateYYYYMMDD},
	}
	body, err := b.callPublic(ctx, "/v1/klines", q)
	if err != nil {
		return nil, err
	}
	env, err := decodeEnvelope("klines", body)
	if err != nil {
		return nil, err
	}
	var rows []struct {
		OpenTime string `json:"openTime"`
		Open     string `json:"open"`
		High     string `json:"high"`
		Low      string `json:"low"`
		Close    string `json:"close"`
		Volume   string `json:"volume"`
	}
	if err := json.Unmarshal(env.Data, &rows); err != nil {
		return nil, fmt.Errorf("klines decode data: %w", err)
	}
	out := make([]market.Kline, 0, len(rows))
	for i, r := range rows {
		ms, err := parseGMOInt64(fmt.Sprintf("klines[%d].openTime", i), r.OpenTime)
		if err != nil {
			return nil, err
		}
		o, err := parseGMOFloat(fmt.Sprintf("klines[%d].open", i), r.Open)
		if err != nil {
			return nil, err
		}
		h, err := parseGMOFloat(fmt.Sprintf("klines[%d].high", i), r.High)
		if err != nil {
			return nil, err
		}
		l, err := parseGMOFloat(fmt.Sprintf("klines[%d].low", i), r.Low)
		if err != nil {
			return nil, err
		}
		c, err := parseGMOFloat(fmt.Sprintf("klines[%d].close", i), r.Close)
		if err != nil {
			return nil, err
		}
		// volume は GMO Forex の interval=1day/1month レスポンスには含まれない
		// (intraday の 1min/5min 等のみ持つ)。空文字列は 0 とみなして続行し、
		// 空でない不正値だけ error として上げる。
		var v float64
		if r.Volume != "" {
			v, err = parseGMOFloat(fmt.Sprintf("klines[%d].volume", i), r.Volume)
			if err != nil {
				return nil, err
			}
		}
		out = append(out, market.Kline{
			Symbol:   symbol,
			Interval: interval,
			OpenTime: time.UnixMilli(ms).UTC(),
			Open:     o, High: h, Low: l, Close: c, Volume: v,
		})
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Private account / order reads
// ---------------------------------------------------------------------------

// GetAccountMargin queries the account's available margin / equity /
// margin ratio via /v1/account/assets (per the fxdocs). Note that
// /v1/account/margin does not exist on the GMO Forex API — that's the crypto
// API path and returns HTTP 404 on forex-api.coin.z.com. The response
// carries field names `availableAmount`,
// `equity`, `marginRatio` (alongside balance / margin / positionLossGain /
// totalSwap / transferableAmount / estimatedTradeFee which we don't need).
func (b *GmoBroker) GetAccountMargin(ctx context.Context) (*order.AccountMargin, error) {
	body, err := b.callPrivate(ctx, http.MethodGet, "/v1/account/assets", nil, nil)
	if err != nil {
		return nil, err
	}
	env, err := decodeEnvelope("assets", body)
	if err != nil {
		return nil, err
	}
	var d struct {
		AvailableAmount string `json:"availableAmount"`
		MarginRatio     string `json:"marginRatio"`
		Equity          string `json:"equity"`
	}
	if err := json.Unmarshal(env.Data, &d); err != nil {
		return nil, fmt.Errorf("assets decode data: %w", err)
	}
	avail, err := parseGMOFloat("assets.availableAmount", d.AvailableAmount)
	if err != nil {
		return nil, err
	}
	ratio, err := parseGMOFloat("assets.marginRatio", d.MarginRatio)
	if err != nil {
		return nil, err
	}
	eq, err := parseGMOFloat("assets.equity", d.Equity)
	if err != nil {
		return nil, err
	}
	return &order.AccountMargin{AvailableJPY: avail, MarginRatio: ratio, Equity: eq}, nil
}

func (b *GmoBroker) GetOpenPositions(ctx context.Context, symbol string) ([]position.Position, error) {
	q := url.Values{}
	if symbol != "" {
		q.Set("symbol", symbol)
	}
	body, err := b.callPrivate(ctx, http.MethodGet, "/v1/openPositions", q, nil)
	if err != nil {
		return nil, err
	}
	env, err := decodeEnvelope("openPositions", body)
	if err != nil {
		return nil, err
	}
	var d struct {
		List []struct {
			PositionID flexString `json:"positionId"`
			Symbol     string     `json:"symbol"`
			Side       string     `json:"side"`
			Size       string     `json:"size"`
			Price      string     `json:"price"`
			Timestamp  string     `json:"timestamp"`
		} `json:"list"`
	}
	if err := json.Unmarshal(env.Data, &d); err != nil {
		return nil, fmt.Errorf("openPositions decode data: %w", err)
	}
	out := make([]position.Position, 0, len(d.List))
	for i, r := range d.List {
		size, err := parseGMOAtoi(fmt.Sprintf("openPositions[%d].size", i), r.Size)
		if err != nil {
			return nil, err
		}
		px, err := parseGMOFloat(fmt.Sprintf("openPositions[%d].price", i), r.Price)
		if err != nil {
			return nil, err
		}
		ts, err := parseGMOTime(fmt.Sprintf("openPositions[%d].timestamp", i), r.Timestamp)
		if err != nil {
			return nil, err
		}
		out = append(out, position.Position{
			BrokerPositionID: string(r.PositionID),
			Symbol:           r.Symbol,
			Side:             order.Side(r.Side),
			Quantity:         size,
			EntryPrice:       px,
			Status:           position.StatusOpen,
			OpenedAt:         ts,
		})
	}
	return out, nil
}

func (b *GmoBroker) GetActiveOrders(ctx context.Context, symbol string) ([]order.Order, error) {
	q := url.Values{}
	if symbol != "" {
		q.Set("symbol", symbol)
	}
	body, err := b.callPrivate(ctx, http.MethodGet, "/v1/activeOrders", q, nil)
	if err != nil {
		return nil, err
	}
	env, err := decodeEnvelope("activeOrders", body)
	if err != nil {
		return nil, err
	}
	var d struct {
		List []struct {
			OrderID       flexString `json:"orderId"`
			Symbol        string     `json:"symbol"`
			Side          string     `json:"side"`
			ExecutionType string     `json:"executionType"`
			Size          string     `json:"size"`
			Price         string     `json:"price"`
			Status        string     `json:"status"`
			Timestamp     string     `json:"timestamp"`
		} `json:"list"`
	}
	if err := json.Unmarshal(env.Data, &d); err != nil {
		return nil, fmt.Errorf("activeOrders decode data: %w", err)
	}
	out := make([]order.Order, 0, len(d.List))
	for i, r := range d.List {
		size, err := parseGMOAtoi(fmt.Sprintf("activeOrders[%d].size", i), r.Size)
		if err != nil {
			return nil, err
		}
		px, err := parseGMOFloat(fmt.Sprintf("activeOrders[%d].price", i), r.Price)
		if err != nil {
			return nil, err
		}
		ts, err := parseGMOTime(fmt.Sprintf("activeOrders[%d].timestamp", i), r.Timestamp)
		if err != nil {
			return nil, err
		}
		out = append(out, order.Order{
			OrderID:   string(r.OrderID),
			Symbol:    r.Symbol,
			Side:      order.Side(r.Side),
			Type:      order.OrderType(r.ExecutionType),
			Quantity:  size,
			Price:     px,
			Status:    r.Status,
			CreatedAt: ts,
		})
	}
	return out, nil
}

func (b *GmoBroker) GetExecutions(ctx context.Context, orderID string) ([]order.Execution, error) {
	q := url.Values{"orderId": []string{orderID}}
	body, err := b.callPrivate(ctx, http.MethodGet, "/v1/executions", q, nil)
	if err != nil {
		return nil, err
	}
	env, err := decodeEnvelope("executions", body)
	if err != nil {
		return nil, err
	}
	return decodeExecutionList("executions", env.Data)
}

// GetLatestExecutionsBySymbol は GMO Forex /v1/latestExecutions を叩いて
// 直近の約定一覧を取得する。port.LatestExecutionsLookup の実装。
//
// 用途: reconcile の synthetic-close fallback の手前で、leg orderId を
// 持たない (記録漏れ / 外部建玉の adopt) live ポジションの実 exit price を
// positionId 一致で復元する。
//
// GMO API spec (Forex 公開仕様 + 実機 smoke-confirmed):
//   - query: symbol (必須), page (省略時 1), count (省略時 100, 最大 100)
//   - response.data.list[*]: executionId, orderId, positionId, symbol, side,
//     size, price, timestamp (settleType も返ってくるが現在は使わない —
//     positionId + side + timestamp で十分 close/open を判別できる)
func (b *GmoBroker) GetLatestExecutionsBySymbol(ctx context.Context, symbol string) ([]order.Execution, error) {
	q := url.Values{"symbol": []string{symbol}}
	body, err := b.callPrivate(ctx, http.MethodGet, "/v1/latestExecutions", q, nil)
	if err != nil {
		return nil, err
	}
	env, err := decodeEnvelope("latestExecutions", body)
	if err != nil {
		return nil, err
	}
	return decodeExecutionList("latestExecutions", env.Data)
}

// decodeExecutionList decodes the shared GMO execution-list payload — env.Data
// from /v1/executions or /v1/latestExecutions, which return the identical row
// shape — into domain Executions. label prefixes the per-row parse-error
// diagnostics ("<label> decode data", "<label>[i].size", ...) so each endpoint
// keeps its own message. Collapses the two formerly near-verbatim ~50-line
// build loops into one source of truth.
func decodeExecutionList(label string, data json.RawMessage) ([]order.Execution, error) {
	var d struct {
		List []struct {
			ExecutionID flexString `json:"executionId"`
			OrderID     flexString `json:"orderId"`
			PositionID  flexString `json:"positionId"`
			Symbol      string     `json:"symbol"`
			Side        string     `json:"side"`
			Size        string     `json:"size"`
			Price       string     `json:"price"`
			Timestamp   string     `json:"timestamp"`
			// Per-fill costs. Optional (empty/absent → 0).
			Fee         string `json:"fee"`
			SettledSwap string `json:"settledSwap"`
			LossGain    string `json:"lossGain"`
		} `json:"list"`
	}
	if err := json.Unmarshal(data, &d); err != nil {
		return nil, fmt.Errorf("%s decode data: %w", label, err)
	}
	out := make([]order.Execution, 0, len(d.List))
	for i, r := range d.List {
		size, err := parseGMOAtoi(fmt.Sprintf("%s[%d].size", label, i), r.Size)
		if err != nil {
			return nil, err
		}
		px, err := parseGMOFloat(fmt.Sprintf("%s[%d].price", label, i), r.Price)
		if err != nil {
			return nil, err
		}
		ts, err := parseGMOTime(fmt.Sprintf("%s[%d].timestamp", label, i), r.Timestamp)
		if err != nil {
			return nil, err
		}
		// Optional per-fill costs — empty/absent → 0, malformed → error.
		fee, err := parseGMOFloatOptional(fmt.Sprintf("%s[%d].fee", label, i), r.Fee)
		if err != nil {
			return nil, err
		}
		swap, err := parseGMOFloatOptional(fmt.Sprintf("%s[%d].settledSwap", label, i), r.SettledSwap)
		if err != nil {
			return nil, err
		}
		lossGain, err := parseGMOFloatOptional(fmt.Sprintf("%s[%d].lossGain", label, i), r.LossGain)
		if err != nil {
			return nil, err
		}
		out = append(out, order.Execution{
			ExecutionID: string(r.ExecutionID),
			OrderID:     string(r.OrderID),
			PositionID:  string(r.PositionID),
			Symbol:      r.Symbol,
			Side:        order.Side(r.Side),
			Quantity:    size,
			Price:       px,
			Timestamp:   ts,
			// 符号正規化: GMO の wire はキャッシュフロー符号
			// (amount = lossGain + fee + settledSwap、公式 docs 例 fee:"-30" = 徴収)。
			// 内部規約は「FeeJPY 正 = コスト」(trades.fee_jpy / net = gross−fee+swap /
			// 0.002% 推定 leg と同符号) なので、ここで反転する。Abs ではなく負号 —
			// 仮に rebate (wire 正) が来ても内部で負コスト = 受取として正しく残る。
			// settledSwap は「受取 = 正」のままで net 式と整合するため raw 維持。
			FeeJPY:         -fee,
			SettledSwapJPY: swap,
			LossGainJPY:    lossGain,
		})
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Private writes — IFDOCO / Close / Cancel
// ---------------------------------------------------------------------------

// priceDecimalsFor returns the number of fractional digits the GMO Forex
// API requires in price fields for a given symbol. GMO rejects requests
// whose decimal-string price exceeds the per-symbol tickSize with
// ERR-5114 "Decimal digits of size is invalid".
//
// GMO Forex quotes every pair to 1/10 pip, so the digit count follows the
// quote currency, not the symbol:
//   - JPY-quote (USD_JPY, EUR_JPY, ...): 1 pip = 0.01, tickSize 0.001 → 3
//   - USD-quote (EUR_USD, GBP_USD, ...): 1 pip = 0.0001, tickSize 0.00001 → 5
//
// Price strings carry exactly that many digits — no more (an artifact like
// "158.87899999999998" trips the check), no fewer (trailing zeros kept so
// the value is unambiguous). Unknown/other quote currencies fall back to 3.
func priceDecimalsFor(symbol string) int {
	switch market.QuoteCurrency(symbol) {
	case "USD":
		return 5
	case "JPY":
		return 3
	}
	return 3
}

// formatPrice formats price for a GMO Forex order payload, pinned to
// symbol's required decimal precision. See priceDecimalsFor — picking
// `'f', -1, 64` would emit float64 round-off artifacts that GMO
// rejects with ERR-5114.
func formatPrice(price float64, symbol string) string {
	return strconv.FormatFloat(price, 'f', priceDecimalsFor(symbol), 64)
}

// placeOrderPayloadIFO is the GMO Forex IFDOCO (IFO) body for entering
// with paired OCO settle orders attached, sent to POST /v1/ifoOrder.
//
// Field naming follows the official fxdocs sample:
//
//	firstSide / firstExecutionType / firstSize / firstPrice  → the entry
//	secondSize / secondLimitPrice / secondStopPrice          → the OCO pair
//
// Notes:
//   - firstExecutionType accepts ONLY "LIMIT" or "STOP". MARKET is not a
//     valid value for /v1/ifoOrder. The caller must supply firstPrice
//     close to the live ask (BUY) or bid (SELL) so the LIMIT entry fills
//     immediately.
//   - The OCO pair side is the opposite of firstSide per the fxdocs
//     ("The side not specified in firstSide becomes the side of the
//     second order"), so we don't send it.
//
// Reference: https://api.coin.z.com/fxdocs/  (anchor: ifoOrder)
type placeOrderPayloadIFO struct {
	Symbol             string `json:"symbol"`
	FirstSide          string `json:"firstSide"`
	FirstExecutionType string `json:"firstExecutionType"`
	FirstSize          string `json:"firstSize"`
	FirstPrice         string `json:"firstPrice"`
	SecondSize         string `json:"secondSize"`
	SecondLimitPrice   string `json:"secondLimitPrice"`
	SecondStopPrice    string `json:"secondStopPrice"`
}

type placeOrderPayloadMarket struct {
	Symbol        string `json:"symbol"`
	Side          string `json:"side"`
	ExecutionType string `json:"executionType"`
	Size          string `json:"size"`
}

// PlaceOrder issues a new order. For Type=IFDOCO we hit /v1/ifoOrder
// with the entry-LIMIT + OCO-pair body shape; for MARKET we issue a
// plain entry to /v1/order; in live the caller then attaches the
// broker-side TP/SL via PlaceSettleOCO (LiveExitProtector).
func (b *GmoBroker) PlaceOrder(ctx context.Context, req order.PlaceOrderRequest) (*order.Order, error) {
	if !req.Side.Valid() {
		return nil, fmt.Errorf("invalid side: %q", req.Side)
	}
	var (
		body []byte
		path = "/v1/order"
		err  error
	)
	switch req.Type {
	case order.OrderTypeIFDOCO:
		if req.TakeProfit <= 0 || req.StopLoss <= 0 {
			return nil, fmt.Errorf("IFDOCO requires take_profit and stop_loss")
		}
		if req.Price <= 0 {
			return nil, fmt.Errorf("IFDOCO requires entry Price for the LIMIT first leg (MARKET unsupported by /v1/ifoOrder)")
		}
		path = "/v1/ifoOrder"
		size := strconv.Itoa(req.Quantity)
		payload := placeOrderPayloadIFO{
			Symbol:             req.Symbol,
			FirstSide:          string(req.Side),
			FirstExecutionType: "LIMIT",
			FirstSize:          size,
			FirstPrice:         formatPrice(req.Price, req.Symbol),
			SecondSize:         size,
			SecondLimitPrice:   formatPrice(req.TakeProfit, req.Symbol),
			SecondStopPrice:    formatPrice(req.StopLoss, req.Symbol),
		}
		body, err = json.Marshal(payload)
	case order.OrderTypeMarket:
		// MARKET だけを明示 case に切り、それ以外 (LIMIT/STOP/OCO/IFD/"") は
		// default で reject する。黙って MARKET 扱いで発注すると silent
		// corruption になるため。
		payload := placeOrderPayloadMarket{
			Symbol:        req.Symbol,
			Side:          string(req.Side),
			ExecutionType: "MARKET",
			Size:          strconv.Itoa(req.Quantity),
		}
		body, err = json.Marshal(payload)
	default:
		return nil, fmt.Errorf("unsupported order type: %q (supported: MARKET, IFDOCO)", req.Type)
	}
	if err != nil {
		return nil, fmt.Errorf("payload: %w", err)
	}
	respBody, err := b.callPrivate(ctx, http.MethodPost, path, nil, body)
	if err != nil {
		return nil, err
	}
	env, err := decodeEnvelope("placeOrder", respBody)
	if err != nil {
		return nil, err
	}
	// GMO returns the new order id in four observed shapes depending on
	// the endpoint:
	//   - /v1/order            → ARRAY of order-detail objects (observed
	//                            via smoke test: data=[{orderId, rootOrderId,
	//                            …, executionType:"MARKET", status:"EXECUTED"}])
	//   - /v1/order (legacy)   → bare scalar in `data` (string or number)
	//   - /v1/order (legacy)   → object with `orderId`
	//   - /v1/ifoOrder (IFDOCO)→ object with `rootOrderId` (number)
	// flexString accepts both number and string forms.
	orderID := extractOrderID(env.Data)
	// GMO は 200 + empty data を返すことがある。OrderID="" の order.Order を
	// 返すと後段 (ResolveExecution / CancelOrder) が orderID なしで走り爆発する
	// ので、ここで明示的に弾く。
	if orderID == "" {
		return nil, fmt.Errorf("placeOrder: empty orderId in GMO response (path=%s body=%s)", path, string(env.Data))
	}
	return &order.Order{
		OrderID:   orderID,
		Symbol:    req.Symbol,
		Side:      req.Side,
		Type:      req.Type,
		Quantity:  req.Quantity,
		Price:     req.Price,
		Status:    "ACCEPTED",
		CreatedAt: b.now(),
	}, nil
}

// closePositionPayload is the body for /v1/closeOrder.
type closePositionPayload struct {
	Symbol         string               `json:"symbol"`
	Side           string               `json:"side"`
	ExecutionType  string               `json:"executionType"`
	SettlePosition []closePositionEntry `json:"settlePosition"`
}

// closePositionEntry mirrors the GMO Forex /v1/closeOrder settlePosition[] item.
// The fxdocs declares positionId as a NUMBER (not a JSON string), with size as
// a string. We accept the bot-internal positionId as a string (because that's
// what positions table stores) and convert on the wire.
type closePositionEntry struct {
	PositionID int64  `json:"positionId"`
	Size       string `json:"size"`
}

// ClosePosition closes (settles) one specific broker position at MARKET.
// To settle, the GMO Forex API expects the position's opposing side (e.g.
// settle a BUY position by issuing a SELL close), the symbol, and the size
// for the position. We derive those from pos directly.
func (b *GmoBroker) ClosePosition(ctx context.Context, pos position.Position) (*order.Order, error) {
	if pos.BrokerPositionID == "" {
		return nil, fmt.Errorf("ClosePosition: broker position id is empty")
	}
	posID, perr := strconv.ParseInt(pos.BrokerPositionID, 10, 64)
	if perr != nil {
		return nil, fmt.Errorf("ClosePosition: broker position id %q is not numeric: %w", pos.BrokerPositionID, perr)
	}
	closeSide := pos.Side.Opposite()
	payload := closePositionPayload{
		Symbol:        pos.Symbol,
		Side:          string(closeSide),
		ExecutionType: "MARKET",
		SettlePosition: []closePositionEntry{
			{PositionID: posID, Size: strconv.Itoa(pos.Quantity)},
		},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("payload: %w", err)
	}
	respBody, err := b.callPrivate(ctx, http.MethodPost, "/v1/closeOrder", nil, body)
	if err != nil {
		return nil, err
	}
	env, err := decodeEnvelope("closeOrder", respBody)
	if err != nil {
		return nil, err
	}
	// /v1/closeOrder POST returns data in the same array-of-order-detail
	// shape as /v1/order POST (confirmed by smoke test). Use the shared extractor
	// so all three known shapes (scalar / object / array) are handled.
	// Without this, ResolveExecution would be called with orderID="" and
	// GMO would return ERR-5106 — the same failure mode as the entry path.
	orderID := extractOrderID(env.Data)
	return &order.Order{
		OrderID:   orderID,
		Symbol:    pos.Symbol,
		Side:      pos.Side.Opposite(),
		Type:      order.OrderTypeMarket,
		Quantity:  pos.Quantity,
		Status:    "ACCEPTED",
		CreatedAt: b.now(),
	}, nil
}

// ResolveExecution polls /v1/executions until the order is filled, returning
// the broker positionId, actual fill price and broker-reported costs.
// Implements port.ExecutionResolver. Retries every 500ms until ctx expires;
// a 10-second deadline covers typical MARKET fill latency on GMO Forex.
//
// PositionID / Price は先頭 fill 由来、コスト (fee / settledSwap / lossGain) は
// 全 fill の合算 (部分約定で手数料を過少報告しないため)。
func (b *GmoBroker) ResolveExecution(ctx context.Context, orderID string) (port.ResolvedExecution, error) {
	res, err := pollUntilOrCtxDone(ctx, 500*time.Millisecond, func(ctx context.Context) (port.ResolvedExecution, bool, error) {
		execs, gerr := b.GetExecutions(ctx, orderID)
		if gerr != nil {
			return port.ResolvedExecution{}, false, fmt.Errorf("get executions: %w", gerr)
		}
		if len(execs) == 0 {
			return port.ResolvedExecution{}, false, nil
		}
		out := port.ResolvedExecution{PositionID: execs[0].PositionID, Price: execs[0].Price}
		for _, e := range execs {
			out.FeeJPY += e.FeeJPY
			out.SettledSwapJPY += e.SettledSwapJPY
			out.LossGainJPY += e.LossGainJPY
		}
		return out, true, nil
	})
	if err != nil {
		// ctx.Err() の場合は元のメッセージ形式を維持 (呼出側がエラー文を見ているため)
		if ctx.Err() != nil {
			return port.ResolvedExecution{}, fmt.Errorf("resolve execution timeout for order %s: %w", orderID, ctx.Err())
		}
		return port.ResolvedExecution{}, err
	}
	return res, nil
}

// ResolveSettleLegs finds the TP/SL child orders that protect the given broker
// position. Polls /v1/activeOrders until both legs are visible or ctx expires
// (typical GMO latency: <1s after OCO placement).
//
// GMO Forex /v1/activeOrders returns settle orders with a settlePosition
// block containing positionId. We filter for orders whose positionId
// matches brokerPositionID, then classify by executionType:
//   - LIMIT (or "OCO" + the limit side) → TP
//   - STOP                              → SL
//
// Returns an error if either leg is missing — a Live position without two
// recorded protection legs is critical and the caller must trip
// emergency_stop rather than continue.
func (b *GmoBroker) ResolveSettleLegs(ctx context.Context, brokerPositionID, symbol string) (string, string, error) {
	if brokerPositionID == "" {
		return "", "", fmt.Errorf("resolve settle legs: empty broker_position_id")
	}
	type legPair struct{ TP, SL string }
	// ctx 期限到来時の詳細エラー (tp/sl どちらが揃わなかったか) を出すために
	// 最後に観測した部分結果を保持する。
	var lastTP, lastSL string
	res, err := pollUntilOrCtxDone(ctx, 500*time.Millisecond, func(ctx context.Context) (legPair, bool, error) {
		legs, gerr := b.getSettleLegsRaw(ctx, symbol)
		if gerr != nil {
			return legPair{}, false, fmt.Errorf("get settle legs: %w", gerr)
		}
		var tpID, slID string
		for _, lg := range legs {
			if lg.PositionID != brokerPositionID {
				continue
			}
			switch lg.ExecutionType {
			case "LIMIT":
				tpID = lg.OrderID
			case "STOP":
				slID = lg.OrderID
			}
		}
		lastTP, lastSL = tpID, slID
		if tpID != "" && slID != "" {
			return legPair{TP: tpID, SL: slID}, true, nil
		}
		return legPair{}, false, nil
	})
	if err != nil {
		if ctx.Err() != nil {
			return "", "", fmt.Errorf("resolve settle legs timeout for position %s (tp=%q sl=%q): %w",
				brokerPositionID, lastTP, lastSL, ctx.Err())
		}
		return "", "", err
	}
	return res.TP, res.SL, nil
}

// settleLegRow is the minimal shape needed by ResolveSettleLegs.
type settleLegRow struct {
	OrderID       string
	ExecutionType string
	PositionID    string
}

// getSettleLegsRaw queries /v1/activeOrders and returns the settle orders
// (those whose settlePosition.positionId is non-empty), parsed into the
// shape ResolveSettleLegs needs. Kept separate from GetActiveOrders so the
// public Broker contract stays narrow.
func (b *GmoBroker) getSettleLegsRaw(ctx context.Context, symbol string) ([]settleLegRow, error) {
	q := url.Values{}
	if symbol != "" {
		q.Set("symbol", symbol)
	}
	body, err := b.callPrivate(ctx, http.MethodGet, "/v1/activeOrders", q, nil)
	if err != nil {
		return nil, err
	}
	env, err := decodeEnvelope("activeOrders", body)
	if err != nil {
		return nil, err
	}
	var d struct {
		List []struct {
			OrderID        flexString `json:"orderId"`
			ExecutionType  string     `json:"executionType"`
			SettlePosition struct {
				PositionID flexString `json:"positionId"`
			} `json:"settlePosition"`
		} `json:"list"`
	}
	if err := json.Unmarshal(env.Data, &d); err != nil {
		return nil, fmt.Errorf("activeOrders settle decode: %w", err)
	}
	out := make([]settleLegRow, 0, len(d.List))
	for _, r := range d.List {
		if string(r.SettlePosition.PositionID) == "" {
			continue
		}
		out = append(out, settleLegRow{
			OrderID:       string(r.OrderID),
			ExecutionType: r.ExecutionType,
			PositionID:    string(r.SettlePosition.PositionID),
		})
	}
	return out, nil
}

// PlaceSettleOCO posts a one-cancels-other settle order against an existing
// position. Used by the MARKET+OCO unified entry flow: after a
// MARKET fill resolves a positionId, the caller calls this to attach the TP
// (limit) + SL (stop) pair on the broker side. GMO Forex /v1/closeOrder
// with executionType=OCO supports this. Returns the rootOrderId of the OCO
// container. Individual leg orderIds must be resolved via /v1/activeOrders
// (see ResolveSettleLegs).
//
// Request body shape (per fxdocs):
//
//	{
//	  "symbol": "USD_JPY",
//	  "side": "SELL"         // CLOSE side, opposite of position
//	  "executionType": "OCO",
//	  "limitPrice": "<TP>",
//	  "stopPrice":  "<SL>",
//	  "settlePosition": [{"positionId": <number>, "size": "<n>"}]
//	}
func (b *GmoBroker) PlaceSettleOCO(ctx context.Context, in port.OCOCloseOrderInput) (string, error) {
	if in.Symbol == "" {
		return "", fmt.Errorf("OCO close: symbol is empty")
	}
	if in.BrokerPositionID <= 0 {
		return "", fmt.Errorf("OCO close: BrokerPositionID must be > 0")
	}
	if !in.Side.Valid() {
		return "", fmt.Errorf("OCO close: invalid Side %q", in.Side)
	}
	if in.Size <= 0 {
		return "", fmt.Errorf("OCO close: Size must be > 0")
	}
	if in.TPPrice <= 0 || in.SLPrice <= 0 {
		return "", fmt.Errorf("OCO close: TPPrice and SLPrice are required")
	}
	body, err := json.Marshal(struct {
		Symbol         string               `json:"symbol"`
		Side           string               `json:"side"`
		ExecutionType  string               `json:"executionType"`
		LimitPrice     string               `json:"limitPrice"`
		StopPrice      string               `json:"stopPrice"`
		SettlePosition []closePositionEntry `json:"settlePosition"`
	}{
		Symbol:        in.Symbol,
		Side:          string(in.Side),
		ExecutionType: "OCO",
		LimitPrice:    formatPrice(in.TPPrice, in.Symbol),
		StopPrice:     formatPrice(in.SLPrice, in.Symbol),
		SettlePosition: []closePositionEntry{
			{PositionID: in.BrokerPositionID, Size: strconv.Itoa(in.Size)},
		},
	})
	if err != nil {
		return "", fmt.Errorf("OCO payload: %w", err)
	}
	respBody, err := b.callPrivate(ctx, http.MethodPost, "/v1/closeOrder", nil, body)
	if err != nil {
		return "", err
	}
	env, err := decodeEnvelope("OCO closeOrder", respBody)
	if err != nil {
		return "", err
	}
	// Response shape mirrors /v1/order (scalar / object / array of order-
	// detail). Reuse the shared extractor.
	id := extractOrderID(env.Data)
	if id == "" {
		return "", fmt.Errorf("OCO closeOrder: could not extract orderId from response %s", string(env.Data))
	}
	return id, nil
}

// CancelOrder cancels an active order via POST /v1/cancelOrders.
//
// GMO Forex API spec:
//
//	body = { "rootOrderIds": [<number>, ...] }
//
// rootOrderIds is an array of NUMBER ids (not strings), up to 10 per call.
// The path is /v1/cancelOrders (plural) — the singular /v1/cancelOrder does
// not exist on the Forex API and returns 404, which would make every close-saga
// cancel fail silently.
//
// The bot's port surface still accepts one orderID string per call (the
// close saga cancels TP/SL legs one at a time); we wrap it in a single-
// element array. Non-numeric ids are rejected up front — they cannot
// possibly be valid GMO ids.
func (b *GmoBroker) CancelOrder(ctx context.Context, orderID string) error {
	id, perr := strconv.ParseInt(orderID, 10, 64)
	if perr != nil {
		return fmt.Errorf("CancelOrder: orderID %q is not a numeric GMO id: %w", orderID, perr)
	}
	body, err := json.Marshal(map[string]any{"rootOrderIds": []int64{id}})
	if err != nil {
		return fmt.Errorf("payload: %w", err)
	}
	respBody, err := b.callPrivate(ctx, http.MethodPost, "/v1/cancelOrders", nil, body)
	if err != nil {
		return err
	}
	var env gmoEnvelope
	if err := json.Unmarshal(respBody, &env); err != nil {
		return fmt.Errorf("cancelOrders decode: %w", err)
	}
	return errorFromEnvelope(&env)
}
