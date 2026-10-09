package app

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// DNS rebinding 対策: loopback に bind しているとき、Host ヘッダが loopback 名か
// DASHBOARD_ALLOWED_HOSTS(AllowedHosts)に無いリクエストは 421 で拒否する。攻撃者のドメインを
// 127.0.0.1 に向け直したページは、ブラウザから見て同一 origin なので CSRF 検査を通ってしまう。
func TestHostGuard_LoopbackBindRejectsForeignHosts(t *testing.T) {
	h := newHostGuard("127.0.0.1:8080", []string{"dash.example.com"})(okHandler())
	for host, want := range map[string]int{
		"127.0.0.1:8080":        http.StatusOK,
		"localhost:3000":        http.StatusOK,
		"[::1]:8080":            http.StatusOK,
		"dash.example.com:8080": http.StatusOK,
		"evil.example:8080":     http.StatusMisdirectedRequest,
		"":                      http.StatusMisdirectedRequest,
	} {
		req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
		req.Host = host
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != want {
			t.Errorf("Host %q: got %d want %d", host, rec.Code, want)
		}
	}
}

// loopback 以外に bind するときは BasicAuth が必須なので Host では絞らない(IP やホスト名で来る)。
func TestHostGuard_NonLoopbackBindDoesNotFilter(t *testing.T) {
	h := newHostGuard("0.0.0.0:8080", nil)(okHandler())
	req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	req.Host = "192.0.2.10:8080"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d want 200", rec.Code)
	}
}

// .env.example の既定パスワードのままでは起動しない。
func TestAPIServer_Run_RefusesPlaceholderPassword(t *testing.T) {
	for _, pass := range []string{"change_me_locally", "change_me", "password", "admin"} {
		srv := &APIServer{Addr: "127.0.0.1:0", Auth: APIAuth{User: "admin", Pass: pass}}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		err := srv.Run(ctx)
		cancel()
		if !errors.Is(err, ErrAPIAuthPlaceholderPassword) {
			t.Errorf("pass %q: expected ErrAPIAuthPlaceholderPassword, got %v", pass, err)
		}
	}
}
