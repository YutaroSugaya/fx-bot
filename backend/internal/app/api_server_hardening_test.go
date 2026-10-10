package app

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// POST の body は上限で切る(認証の後ろとはいえ、巨大な body でメモリを食わせない)。
func TestBodyLimit_RejectsOversizedBody(t *testing.T) {
	var readErr error
	h := newBodyLimit(1024)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, readErr = io.ReadAll(r.Body)
	}))
	req := httptest.NewRequest(http.MethodPost, "/api/ask-claude", bytes.NewReader(make([]byte, 4096)))
	h.ServeHTTP(httptest.NewRecorder(), req)
	var tooLarge *http.MaxBytesError
	if !errors.As(readErr, &tooLarge) {
		t.Fatalf("expected *http.MaxBytesError, got %v", readErr)
	}

	readErr = nil
	small := httptest.NewRequest(http.MethodPost, "/api/ask-claude", strings.NewReader(`{"q":"hi"}`))
	h.ServeHTTP(httptest.NewRecorder(), small)
	if readErr != nil {
		t.Fatalf("a small body must pass: %v", readErr)
	}
}

// API の応答は iframe に埋め込ませず、MIME sniffing もさせない。
func TestSecurityHeaders(t *testing.T) {
	rec := httptest.NewRecorder()
	newSecurityHeaders(okHandler()).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/status", nil))
	for k, want := range map[string]string{
		"X-Frame-Options":         "DENY",
		"Content-Security-Policy": "frame-ancestors 'none'",
		"X-Content-Type-Options":  "nosniff",
		"Referrer-Policy":         "no-referrer",
	} {
		if got := rec.Header().Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
}

// 本体を読み切らない接続や放置された keep-alive を切る。advisor の起動は長いので WriteTimeout は付けない。
func TestNewHTTPServer_Timeouts(t *testing.T) {
	srv := newHTTPServer("127.0.0.1:0", okHandler())
	if srv.ReadHeaderTimeout <= 0 || srv.ReadTimeout <= 0 || srv.IdleTimeout <= 0 {
		t.Fatalf("timeouts must be set: header=%v read=%v idle=%v", srv.ReadHeaderTimeout, srv.ReadTimeout, srv.IdleTimeout)
	}
	if srv.WriteTimeout != 0 {
		t.Fatalf("WriteTimeout must stay 0 (long advisor runs), got %v", srv.WriteTimeout)
	}
}

// loopback の外に bind するときは、総当たりに耐える長さのパスワードを要求する。
func TestAPIServer_Run_RefusesShortPasswordOnNonLoopbackBind(t *testing.T) {
	srv := &APIServer{Addr: "192.0.2.10:0", Auth: APIAuth{User: "u", Pass: "short-pass"}}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := srv.Run(ctx); !errors.Is(err, ErrAPIAuthWeakPassword) {
		t.Fatalf("expected ErrAPIAuthWeakPassword, got %v", err)
	}

	loopback := &APIServer{Addr: "127.0.0.1:0", Auth: APIAuth{User: "u", Pass: "short-pass"}}
	ctx2, cancel2 := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel2()
	if err := loopback.Run(ctx2); err != nil {
		t.Fatalf("a short password is accepted on a loopback bind: %v", err)
	}
}
