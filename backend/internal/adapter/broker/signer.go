package broker

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"strconv"
	"time"
)

// Sign computes the GMO Coin FX API request signature.
//
// GMO docs (Forex API): the signature input is the concatenation of
//
//	timestamp + method + path + body
//
// where timestamp is the millisecond unix epoch, method is uppercase
// ("GET"/"POST"), path is the API path part *without* the host or base URL
// (e.g. "/v1/account/assets", "/v1/order"), and body is the request body
// for POST (empty string for GET).
//
// The signature is HMAC-SHA256(secret, input) encoded as lowercase hex.
//
// Reference: https://api.coin.z.com/docs/#authorization (Forex docs share
// the same auth scheme as Spot/Margin).
func Sign(apiSecret, timestamp, method, path, body string) string {
	mac := hmac.New(sha256.New, []byte(apiSecret))
	mac.Write([]byte(timestamp + method + path + body))
	return hex.EncodeToString(mac.Sum(nil))
}

// TimestampMillis returns t formatted as the unix epoch in milliseconds.
func TimestampMillis(t time.Time) string {
	return strconv.FormatInt(t.UnixMilli(), 10)
}

// BuildPrivateHeaders returns the three headers GMO requires for any private
// API call: API-KEY, API-TIMESTAMP, API-SIGN. Pass an empty `body` for GETs.
func BuildPrivateHeaders(apiKey, apiSecret, method, path string, body []byte, now time.Time) http.Header {
	ts := TimestampMillis(now)
	sig := Sign(apiSecret, ts, method, path, string(body))
	h := http.Header{}
	h.Set("API-KEY", apiKey)
	h.Set("API-TIMESTAMP", ts)
	h.Set("API-SIGN", sig)
	if method == http.MethodPost && len(body) > 0 {
		h.Set("Content-Type", "application/json")
	}
	return h
}

// SignError is returned by the broker when API responses indicate an
// authentication problem; useful for callers wanting to surface auth issues
// distinctly from transport errors.
type SignError struct {
	Status  int
	Message string
}

func (e *SignError) Error() string {
	return fmt.Sprintf("gmo auth error: status=%d msg=%s", e.Status, e.Message)
}
