package app

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// protect/withCORS が os.Getenv("DASHBOARD_USER"/"DASHBOARD_PASS") を毎リクエスト
// 読むと片方欠落で auth bypass になり、CORS を Access-Control-Allow-Origin: * 固定に
// すると任意オリジンから叩ける。そのため fail-closed + allowlist にする。
//
// 仕様:
//   - newAuthMiddleware は creds 欠落で error。AllowUnauthenticated=true で bypass。
//   - 認証要求が通ったハンドラは元の HandlerFunc が呼ばれる。
//   - 不正 creds は 401 + WWW-Authenticate。
//   - newCORSMiddleware は AllowedOrigins に含まれる Origin のみ ACAO に echo。
//     空 list の場合は ACAO ヘッダを一切出さない (同一 origin only)。
//   - Run() は起動時に Auth を validate、creds 欠落 && !AllowUnauthenticated で
//     ErrAPIAuthMisconfigured を返す。

// --- newAuthMiddleware ---

func TestNewAuthMiddleware_FailsWhenCredsMissing(t *testing.T) {
	tests := []struct {
		name string
		auth APIAuth
	}{
		{"both empty", APIAuth{}},
		{"user only", APIAuth{User: "admin"}},
		{"pass only", APIAuth{Pass: "secret"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := newAuthMiddleware(tc.auth)
			if !errors.Is(err, ErrAPIAuthMisconfigured) {
				t.Errorf("got %v, want ErrAPIAuthMisconfigured", err)
			}
		})
	}
}

func TestNewAuthMiddleware_AllowUnauthenticatedSkipsAuth(t *testing.T) {
	mw, err := newAuthMiddleware(APIAuth{AllowUnauthenticated: true})
	if err != nil {
		t.Fatalf("middleware: %v", err)
	}
	rec := httptest.NewRecorder()
	mw(okHandler())(rec, httptest.NewRequest(http.MethodGet, "/foo", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("got %d want 200", rec.Code)
	}
}

func TestNewAuthMiddleware_RequiresCredentials(t *testing.T) {
	mw, err := newAuthMiddleware(APIAuth{User: "admin", Pass: "secret"})
	if err != nil {
		t.Fatalf("middleware: %v", err)
	}

	t.Run("no auth → 401 + WWW-Authenticate", func(t *testing.T) {
		rec := httptest.NewRecorder()
		mw(okHandler())(rec, httptest.NewRequest(http.MethodGet, "/foo", nil))
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("got %d want 401", rec.Code)
		}
		if got := rec.Header().Get("WWW-Authenticate"); !strings.HasPrefix(got, "Basic ") {
			t.Errorf("WWW-Authenticate: got %q want Basic realm=", got)
		}
	})

	t.Run("correct creds → handler runs", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/foo", nil)
		req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte("admin:secret")))
		mw(okHandler())(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("got %d want 200", rec.Code)
		}
	})

	t.Run("wrong user → 401", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/foo", nil)
		req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte("evil:secret")))
		mw(okHandler())(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("got %d want 401", rec.Code)
		}
	})

	t.Run("wrong password → 401", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/foo", nil)
		req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte("admin:wrong")))
		mw(okHandler())(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("got %d want 401", rec.Code)
		}
	})
}

// --- newCORSMiddleware ---

func TestNewCORSMiddleware_EmptyAllowlist_NoHeaders(t *testing.T) {
	mw := newCORSMiddleware(APICORS{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/foo", nil)
	req.Header.Set("Origin", "http://evil.example")
	mw(okHandler()).ServeHTTP(rec, req)
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("expected no ACAO header; got %q", got)
	}
}

func TestNewCORSMiddleware_AllowlistOnly(t *testing.T) {
	mw := newCORSMiddleware(APICORS{AllowedOrigins: []string{"http://localhost:3000", "https://app.example"}})

	t.Run("allowed origin is echoed", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/foo", nil)
		req.Header.Set("Origin", "http://localhost:3000")
		mw(okHandler()).ServeHTTP(rec, req)
		if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "http://localhost:3000" {
			t.Errorf("ACAO: got %q want http://localhost:3000", got)
		}
		if got := rec.Header().Get("Vary"); !strings.Contains(got, "Origin") {
			t.Errorf("Vary should include Origin; got %q", got)
		}
	})

	t.Run("disallowed origin is dropped", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/foo", nil)
		req.Header.Set("Origin", "http://evil.example")
		mw(okHandler()).ServeHTTP(rec, req)
		if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
			t.Errorf("expected no ACAO for disallowed; got %q", got)
		}
	})

	t.Run("preflight returns 204 with ACAO when allowed", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodOptions, "/foo", nil)
		req.Header.Set("Origin", "http://localhost:3000")
		req.Header.Set("Access-Control-Request-Method", "POST")
		mw(okHandler()).ServeHTTP(rec, req)
		if rec.Code != http.StatusNoContent {
			t.Errorf("preflight: got %d want 204", rec.Code)
		}
		if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "http://localhost:3000" {
			t.Errorf("preflight ACAO: got %q", got)
		}
	})

	t.Run("preflight from disallowed origin → 204 but no ACAO", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodOptions, "/foo", nil)
		req.Header.Set("Origin", "http://evil.example")
		req.Header.Set("Access-Control-Request-Method", "POST")
		mw(okHandler()).ServeHTTP(rec, req)
		if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
			t.Errorf("preflight from evil: expected no ACAO; got %q", got)
		}
	})
}

// --- newCSRFMiddleware ---
//
// 仕様:
//   - ブラウザからの cross-origin な POST (Sec-Fetch-Site: cross-site 等) は 403。
//     別サイトを開いただけで発注/決済/緊急停止解除を叩かれる CSRF を塞ぐ。
//   - 同一オリジン (dashboard の Next.js 経由) と非ブラウザ (curl / healthcheck) は通す。
//   - GET などの safe method は常に通す。
//   - CORS allowlist の Origin と既定 dashboard (localhost:3000) は trusted origin として通す。

func csrfPOST(t *testing.T, mw func(http.Handler) http.Handler, headers map[string]string) int {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8080/api/emergency-resume", nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	mw(okHandler()).ServeHTTP(rec, req)
	return rec.Code
}

func TestNewCSRFMiddleware(t *testing.T) {
	mw, err := newCSRFMiddleware(APICORS{AllowedOrigins: []string{"https://app.example"}})
	if err != nil {
		t.Fatalf("newCSRFMiddleware: %v", err)
	}

	cases := []struct {
		name    string
		headers map[string]string
		want    int
	}{
		{"cross-site browser POST is rejected",
			map[string]string{"Sec-Fetch-Site": "cross-site", "Origin": "https://evil.example"}, http.StatusForbidden},
		{"cross-origin Origin without fetch metadata is rejected",
			map[string]string{"Origin": "https://evil.example"}, http.StatusForbidden},
		{"same-origin browser POST passes",
			map[string]string{"Sec-Fetch-Site": "same-origin", "Origin": "http://127.0.0.1:8080"}, http.StatusOK},
		{"non-browser POST (curl) passes",
			map[string]string{}, http.StatusOK},
		{"CORS allowlisted origin passes",
			map[string]string{"Sec-Fetch-Site": "cross-site", "Origin": "https://app.example"}, http.StatusOK},
		{"default dashboard origin via Next proxy passes",
			map[string]string{"Origin": "http://localhost:3000"}, http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := csrfPOST(t, mw, tc.headers); got != tc.want {
				t.Errorf("status: got %d want %d", got, tc.want)
			}
		})
	}

	t.Run("cross-site GET (safe method) passes", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8080/api/status", nil)
		req.Header.Set("Sec-Fetch-Site", "cross-site")
		req.Header.Set("Origin", "https://evil.example")
		mw(okHandler()).ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("status: got %d want 200", rec.Code)
		}
	})
}

func TestNewCSRFMiddleware_InvalidAllowlistOriginErrors(t *testing.T) {
	if _, err := newCSRFMiddleware(APICORS{AllowedOrigins: []string{"not a url"}}); err == nil {
		t.Fatal("expected error for malformed trusted origin")
	}
}

// --- Run() startup validation ---

// Run() は CSRF ミドルウェアを組み込む: trusted origin が壊れていれば起動時に失敗する。
func TestAPIServer_Run_FailsOnInvalidCSRFTrustedOrigin(t *testing.T) {
	srv := &APIServer{
		Addr: "127.0.0.1:0",
		Auth: APIAuth{AllowUnauthenticated: true},
		CORS: APICORS{AllowedOrigins: []string{"not a url"}},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := srv.Run(ctx); err == nil || !strings.Contains(err.Error(), "trusted origin") {
		t.Fatalf("expected trusted origin error, got %v", err)
	}
}

func TestAPIServer_Run_FailsClosedAtStartup(t *testing.T) {
	srv := &APIServer{Addr: "127.0.0.1:0", Auth: APIAuth{}}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := srv.Run(ctx)
	if !errors.Is(err, ErrAPIAuthMisconfigured) {
		t.Fatalf("expected ErrAPIAuthMisconfigured, got %v", err)
	}
}

// 認証バイパス(AllowUnauthenticated)は loopback bind のときだけ許す。
// 0.0.0.0 / 空ホスト(全インタフェース)/ LAN の IP に無認証で bind すると、同じネットワークの
// 誰でも建玉の参照と決済・緊急停止の解除ができてしまうので、起動を拒否する(fail-close)。
func TestAPIServer_Run_RefusesUnauthenticatedNonLoopbackBind(t *testing.T) {
	for _, addr := range []string{"0.0.0.0:0", ":0", "192.0.2.10:0", "[::]:0", "not-an-addr"} {
		t.Run(addr, func(t *testing.T) {
			srv := &APIServer{Addr: addr, Auth: APIAuth{AllowUnauthenticated: true}}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			if err := srv.Run(ctx); !errors.Is(err, ErrAPIAuthUnsafeBind) {
				t.Fatalf("addr %q: expected ErrAPIAuthUnsafeBind, got %v", addr, err)
			}
		})
	}
}

func TestAPIServer_Run_AllowsUnauthenticatedLoopbackBind(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:0", "localhost:0", "[::1]:0"} {
		t.Run(addr, func(t *testing.T) {
			srv := &APIServer{Addr: addr, Auth: APIAuth{AllowUnauthenticated: true}}
			ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
			defer cancel()
			if err := srv.Run(ctx); err != nil {
				t.Fatalf("addr %q: expected clean shutdown, got %v", addr, err)
			}
		})
	}
}

// 認証ありなら bind 先は問わない(BasicAuth が GET / POST の全 /api を守る)。
// 実際に全インタフェースへ bind しないよう、割り当てられない文書用 IP で listen まで進むことだけ見る。
func TestAPIServer_Run_AllowsAuthenticatedNonLoopbackBind(t *testing.T) {
	srv := &APIServer{Addr: "192.0.2.10:0", Auth: APIAuth{User: "u", Pass: "a-long-enough-password"}}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := srv.Run(ctx)
	if errors.Is(err, ErrAPIAuthUnsafeBind) {
		t.Fatalf("authenticated server must not be refused by the bind guard: %v", err)
	}
	if err == nil || !strings.Contains(err.Error(), "listen") {
		t.Fatalf("expected the listen attempt to fail on an unassigned address, got %v", err)
	}
}

// --- helpers ---

func okHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }
}
