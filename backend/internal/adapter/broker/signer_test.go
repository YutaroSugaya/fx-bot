package broker

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strconv"
	"testing"
	"time"
)

// TestSign_MatchesReferenceHMAC re-implements the GMO formula inline and
// confirms our Sign() produces a bit-identical result. This catches accidental
// rearrangements (e.g. body before path) in the future.
func TestSign_MatchesReferenceHMAC(t *testing.T) {
	cases := []struct {
		name      string
		secret    string
		timestamp string
		method    string
		path      string
		body      string
	}{
		{"GET margin", "secret-key", "1739000000000", "GET", "/v1/account/margin", ""},
		{"POST order", "secret-key", "1739000000001", "POST", "/v1/order",
			`{"symbol":"USD_JPY","side":"BUY","executionType":"MARKET","size":"100"}`},
		{"empty secret", "", "0", "GET", "/", ""},
		{"binary-ish secret", "abc\x00def", "1", "GET", "/x", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Sign(c.secret, c.timestamp, c.method, c.path, c.body)

			mac := hmac.New(sha256.New, []byte(c.secret))
			mac.Write([]byte(c.timestamp + c.method + c.path + c.body))
			want := hex.EncodeToString(mac.Sum(nil))

			if got != want {
				t.Errorf("got %q, want %q", got, want)
			}
		})
	}
}

// TestSign_DifferentMethodOrPathProducesDifferentSig is the obvious
// "shouldn't be the same" check.
func TestSign_DifferentInputsDiffer(t *testing.T) {
	secret := "k"
	ts := "1739000000000"
	a := Sign(secret, ts, "GET", "/v1/account/margin", "")
	b := Sign(secret, ts, "POST", "/v1/account/margin", "")
	c := Sign(secret, ts, "GET", "/v1/openPositions", "")
	if a == b || a == c || b == c {
		t.Errorf("signatures should differ: a=%s b=%s c=%s", a, b, c)
	}
}

func TestTimestampMillis(t *testing.T) {
	tm := time.Date(2026, 5, 15, 1, 0, 0, 0, time.UTC)
	want := strconv.FormatInt(tm.UnixMilli(), 10)
	if got := TimestampMillis(tm); got != want {
		t.Errorf("got %s, want %s", got, want)
	}
	// Sanity: positive and roughly right magnitude (13 digits in 2026).
	if len(want) != 13 {
		t.Errorf("expected 13-digit ms timestamp, got %q", want)
	}
}

func TestBuildPrivateHeaders_GET(t *testing.T) {
	now := time.Date(2026, 5, 15, 1, 0, 0, 0, time.UTC)
	h := BuildPrivateHeaders("api-key-x", "secret-y", http.MethodGet, "/v1/account/margin", nil, now)

	if got := h.Get("API-KEY"); got != "api-key-x" {
		t.Errorf("API-KEY: %q", got)
	}
	if h.Get("API-TIMESTAMP") == "" {
		t.Errorf("API-TIMESTAMP missing")
	}
	if h.Get("API-SIGN") == "" {
		t.Errorf("API-SIGN missing")
	}
	// GET should not set Content-Type
	if ct := h.Get("Content-Type"); ct != "" {
		t.Errorf("GET should not set Content-Type, got %q", ct)
	}
}

func TestBuildPrivateHeaders_POST_SetsContentType(t *testing.T) {
	now := time.Date(2026, 5, 15, 1, 0, 0, 0, time.UTC)
	body := []byte(`{"symbol":"USD_JPY","side":"BUY"}`)
	h := BuildPrivateHeaders("api-key-x", "secret-y", http.MethodPost, "/v1/order", body, now)

	if ct := h.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type: %q", ct)
	}
}

func TestBuildPrivateHeaders_SignatureIsReproducible(t *testing.T) {
	now := time.Date(2026, 5, 15, 1, 0, 0, 0, time.UTC)
	h1 := BuildPrivateHeaders("key", "secret", "GET", "/v1/x", nil, now)
	h2 := BuildPrivateHeaders("key", "secret", "GET", "/v1/x", nil, now)
	if h1.Get("API-SIGN") != h2.Get("API-SIGN") {
		t.Errorf("same inputs should produce same sig")
	}
}
