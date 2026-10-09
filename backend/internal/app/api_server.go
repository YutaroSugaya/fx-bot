package app

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"fx-bot/backend/internal/app/handler"
)

// APIServer is a thin orchestrator for the dashboard's REST API.
//
// It owns:
//   - the listen address (Addr)
//   - BasicAuth (Auth) — fail-closed: Run() refuses to start without credentials
//     unless AllowUnauthenticated is set explicitly (dev escape hatch)
//   - CORS allowlist (CORS) — empty list means no CORS headers (same-origin only)
//   - route registration that delegates to handler.* structs
//
// All endpoint logic lives in app/handler/ — see /docs/architecture/layers/handler.md.
type APIServer struct {
	Addr   string
	Logger *slog.Logger

	// Auth controls BasicAuth. Must be configured at construction time
	// (Run() validates and fails closed if credentials are missing without
	// AllowUnauthenticated).
	Auth APIAuth

	// CORS controls cross-origin policy. Empty AllowedOrigins → no CORS
	// headers (same-origin only). Use a non-empty allowlist for the
	// dashboard origin; we never echo a wildcard `*`.
	CORS APICORS

	// AllowedHosts are extra Host names accepted on a loopback bind besides
	// localhost / 127.0.0.1 / ::1 (DASHBOARD_ALLOWED_HOSTS). See newHostGuard.
	AllowedHosts []string

	// Handlers — wired from main.go
	StatusHandler         *handler.StatusHandler
	PositionsHandler      *handler.PositionsHandler
	TradesHandler         *handler.TradesHandler
	AdvisorHandler        *handler.AdvisorHandler
	EmergencyHandler      *handler.EmergencyHandler
	AskClaudeHandler      *handler.AskClaudeHandler
	MarketHandler         *handler.MarketHandler
	StrategySignalHandler *handler.StrategySignalHandler
	AdvisorV2Handler      *handler.AdvisorV2Handler
	LLMDecisionHandler    *handler.LLMDecisionHandler

	// Legacy ticker error counter (kept here so worker.OnTickerError can hook
	// in via APIServer.IncrementTickerErrors). Eventually fold into Counters.
	tickerErrors atomic.Int64

	// Counters は Live 重要イベントの累積カウンタ (StatusHandler 経由で /api/status に露出)。
	Counters *Counters
}

// APIAuth carries BasicAuth credentials and the explicit dev bypass flag.
// Construction site (main.go)決定で fail-closed/open を選ぶ。
type APIAuth struct {
	User string
	Pass string
	// AllowUnauthenticated must be set explicitly to skip BasicAuth. Use only
	// for dev/disabled mode; production must keep this false and provide both
	// User/Pass.
	AllowUnauthenticated bool
}

// APICORS represents the CORS allowlist policy.
type APICORS struct {
	AllowedOrigins []string
}

// ErrAPIAuthMisconfigured is returned by Run() (and newAuthMiddleware) when
// Auth is missing credentials and AllowUnauthenticated is not set
// (fail-closed; see docs/integrations/API_CONTRACT.md).
var ErrAPIAuthMisconfigured = errors.New("api: BasicAuth credentials required (set DASHBOARD_USER and DASHBOARD_PASS, or APIAuth.AllowUnauthenticated for dev)")

// ErrAPIAuthPlaceholderPassword is returned by Run() when BasicAuth is on but the
// password is still an example value.
var ErrAPIAuthPlaceholderPassword = errors.New("api: DASHBOARD_PASS is still a placeholder value — set a real password")

// placeholderPasswords are example values (.env.example and obvious defaults) that must not
// protect a dashboard that can close positions and place orders.
var placeholderPasswords = map[string]bool{
	"change_me_locally": true, "change_me": true, "changeme": true, "password": true, "admin": true,
}

// ErrAPIAuthUnsafeBind is returned by Run() when auth is bypassed
// (AllowUnauthenticated) but Addr is not a loopback address.
var ErrAPIAuthUnsafeBind = errors.New("api: auth bypass is allowed only on a loopback bind (set DASHBOARD_USER and DASHBOARD_PASS, or bind API_ADDR to 127.0.0.1)")

// TickerErrors は worker → APIServer の hook が使う legacy カウンタへのアクセサ。
func (s *APIServer) TickerErrors() *atomic.Int64 { return &s.tickerErrors }

// IncrementTickerErrors lets the worker bump the error counter visible via
// /api/status (worker calls this on transient ticker fetch errors).
func (s *APIServer) IncrementTickerErrors() {
	s.tickerErrors.Add(1)
	if s.Counters != nil {
		s.Counters.TickerErrors.Add(1)
	}
}

// Run starts the HTTP server and blocks until ctx cancels. Returns
// ErrAPIAuthMisconfigured immediately when Auth is missing credentials
// without AllowUnauthenticated — production must not start with an
// unprotected dashboard.
func (s *APIServer) Run(ctx context.Context) error {
	authMW, err := newAuthMiddleware(s.Auth)
	if err != nil {
		return err
	}
	if s.Auth.AllowUnauthenticated && !isLoopbackAddr(s.Addr) {
		return fmt.Errorf("%w (addr=%q)", ErrAPIAuthUnsafeBind, s.Addr)
	}
	if !s.Auth.AllowUnauthenticated && placeholderPasswords[strings.ToLower(s.Auth.Pass)] {
		return ErrAPIAuthPlaceholderPassword
	}
	if s.Auth.AllowUnauthenticated && s.Logger != nil {
		s.Logger.Warn("api_server_auth_disabled",
			"reason", "AllowUnauthenticated=true (dev escape hatch)")
	}
	corsMW := newCORSMiddleware(s.CORS)
	csrfMW, err := newCSRFMiddleware(s.CORS)
	if err != nil {
		return err
	}

	mux := http.NewServeMux()

	// /healthz is always registered (k8s/probe friendliness) and never goes
	// through auth. StatusHandler.Healthz is the canonical liveness probe
	// (returns only {"status":"ok"});
	// fall back to a minimal stub only when StatusHandler is absent (tests).
	// ServeMux panics on duplicate pattern registration, so this must be
	// either/or — not register-then-override.
	if s.StatusHandler != nil {
		mux.HandleFunc("/healthz", s.StatusHandler.Healthz)
		mux.HandleFunc("/api/status", authMW(s.StatusHandler.Status))
		mux.HandleFunc("/api/active-config", authMW(s.StatusHandler.ActiveConfig))
	} else {
		mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ok":true}`))
		})
	}
	if s.TradesHandler != nil {
		mux.HandleFunc("/api/trades", authMW(s.TradesHandler.List))
		mux.HandleFunc("/api/trade/manual", authMW(s.TradesHandler.Manual))
	}
	if s.PositionsHandler != nil {
		mux.HandleFunc("/api/positions", authMW(s.PositionsHandler.List))
		mux.HandleFunc("/api/positions/close", authMW(s.PositionsHandler.Close))
		mux.HandleFunc("/api/positions/extend", authMW(s.PositionsHandler.Extend))
	}
	if s.AdvisorHandler != nil {
		mux.HandleFunc("/api/advisor/recent", authMW(s.AdvisorHandler.Recent))
		mux.HandleFunc("/api/advisor/trigger", authMW(s.AdvisorHandler.TriggerNow))
	}
	if s.EmergencyHandler != nil {
		mux.HandleFunc("/api/emergency-stop", authMW(s.EmergencyHandler.Stop))
		mux.HandleFunc("/api/emergency-resume", authMW(s.EmergencyHandler.Resume))
	}
	if s.AskClaudeHandler != nil {
		mux.HandleFunc("/api/ask-claude", authMW(s.AskClaudeHandler.Ask))
	}
	if s.MarketHandler != nil {
		mux.HandleFunc("/api/market/state", authMW(s.MarketHandler.State))
	}
	if s.StrategySignalHandler != nil {
		mux.HandleFunc("/api/strategy/signal", authMW(s.StrategySignalHandler.Signal))
	}
	if s.AdvisorV2Handler != nil {
		mux.HandleFunc("/api/advisor-v2", authMW(s.AdvisorV2Handler.Status))
	}
	if s.LLMDecisionHandler != nil {
		mux.HandleFunc("/api/llm-decision", authMW(s.LLMDecisionHandler.Status))
		mux.HandleFunc("/api/llm-decision/trigger", authMW(s.LLMDecisionHandler.TriggerNow))
	}

	srv := &http.Server{
		Addr:              s.Addr,
		Handler:           newHostGuard(s.Addr, s.AllowedHosts)(corsMW(csrfMW(mux))),
		ReadHeaderTimeout: 5 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		if s.Logger != nil {
			s.Logger.Info("api_server_started", "addr", s.Addr,
				"cors_allowed_origins", len(s.CORS.AllowedOrigins))
		}
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
		if s.Logger != nil {
			s.Logger.Info("api_server_stopped")
		}
		return nil
	case err := <-errCh:
		return err
	}
}

// newHostGuard rejects requests whose Host header is not a loopback name (or one of extra) while
// the server is bound to loopback. This blocks DNS rebinding: a page on an attacker's domain that
// resolves to 127.0.0.1 is same-origin from the browser's point of view, so CSRF checks pass, but
// its Host header still names the attacker's domain. On a non-loopback bind BasicAuth is mandatory
// and clients arrive by IP or host name, so the Host header is not filtered.
func newHostGuard(addr string, extra []string) func(http.Handler) http.Handler {
	if !isLoopbackAddr(addr) {
		return func(next http.Handler) http.Handler { return next }
	}
	allowed := map[string]bool{"localhost": true, "127.0.0.1": true, "::1": true}
	for _, h := range extra {
		if h = strings.ToLower(strings.TrimSpace(h)); h != "" {
			allowed[h] = true
		}
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			host := r.Host
			if h, _, err := net.SplitHostPort(host); err == nil {
				host = h
			}
			if !allowed[strings.ToLower(strings.Trim(host, "[]"))] {
				http.Error(w, "misdirected request", http.StatusMisdirectedRequest)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// isLoopbackAddr reports whether a listen address binds only to loopback.
// An empty host (":8080") listens on every interface, and an unparsable
// address is treated as non-loopback (fail-close).
func isLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// newAuthMiddleware returns a HandlerFunc wrapper that enforces BasicAuth.
//
// Fail-closed: missing User or Pass with AllowUnauthenticated=false yields
// ErrAPIAuthMisconfigured at construction time, so the caller (Run()) can
// refuse to start the server. Empty env vars never silently disable auth —
// AllowUnauthenticated must be set explicitly (dev only, loopback bind only).
func newAuthMiddleware(a APIAuth) (func(http.HandlerFunc) http.HandlerFunc, error) {
	if a.AllowUnauthenticated {
		return func(h http.HandlerFunc) http.HandlerFunc { return h }, nil
	}
	if a.User == "" || a.Pass == "" {
		return nil, fmt.Errorf("%w (user_empty=%v pass_empty=%v)",
			ErrAPIAuthMisconfigured, a.User == "", a.Pass == "")
	}
	user, pass := a.User, a.Pass
	return func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			u, p, ok := r.BasicAuth()
			if !ok ||
				subtle.ConstantTimeCompare([]byte(u), []byte(user)) != 1 ||
				subtle.ConstantTimeCompare([]byte(p), []byte(pass)) != 1 {
				w.Header().Set("WWW-Authenticate", `Basic realm="fx-bot"`)
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			h(w, r)
		}
	}, nil
}

// defaultDashboardOrigins are the origins the bundled Next.js dashboard is
// served from (`make start` → next dev on 127.0.0.1:3000). Its /api rewrite
// proxies to this server, so a browser POST arrives with this Origin.
var defaultDashboardOrigins = []string{"http://localhost:3000", "http://127.0.0.1:3000"}

// newCSRFMiddleware rejects non-safe cross-origin browser requests (CSRF) via
// net/http.CrossOriginProtection (Sec-Fetch-Site / Origin vs Host). Without
// it, any web page the operator opens could POST to /api/trade/manual,
// /api/emergency-resume, etc. Same-origin and non-browser (curl,
// healthcheck) requests pass; the CORS allowlist and the default dashboard
// origins are trusted.
func newCSRFMiddleware(c APICORS) (func(http.Handler) http.Handler, error) {
	cop := http.NewCrossOriginProtection()
	for _, o := range append(append([]string{}, defaultDashboardOrigins...), c.AllowedOrigins...) {
		if err := cop.AddTrustedOrigin(o); err != nil {
			return nil, fmt.Errorf("api: invalid trusted origin %q: %w", o, err)
		}
	}
	return cop.Handler, nil
}

// newCORSMiddleware applies CORS headers based on an allowlist.
//
// Empty AllowedOrigins → no Access-Control-Allow-Origin header emitted at
// all (same-origin only). Non-empty list → echo the request's Origin if it
// matches exactly. Wildcard ("*") is intentionally unsupported (fail-closed).
//
// OPTIONS preflight returns 204; the response only carries CORS headers
// when the Origin is allowed.
func newCORSMiddleware(c APICORS) func(http.Handler) http.Handler {
	allowed := make(map[string]struct{}, len(c.AllowedOrigins))
	for _, o := range c.AllowedOrigins {
		allowed[o] = struct{}{}
	}
	return func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")
			if origin != "" {
				if _, ok := allowed[origin]; ok {
					w.Header().Set("Access-Control-Allow-Origin", origin)
					w.Header().Set("Vary", "Origin")
					w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
					w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
				}
			}
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			h.ServeHTTP(w, r)
		})
	}
}
