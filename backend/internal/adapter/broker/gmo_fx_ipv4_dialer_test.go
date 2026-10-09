package broker

import (
	"context"
	"net"
	"net/http"
	"testing"
)

// macOS の getaddrinfo は AAAA を優先するため、
// `forex-api.coin.z.com` の AAAA レコードが返ると Go の HTTP client は
// IPv6 経由で接続する。GMO 側の IP 制限リストには通常 IPv4 のみ登録
// するため、IPv6 で出ていくと「想定外 IP」として ERR-5012 が返り、
// 認証が完全に通らない。
//
// 修正方針: NewGmoBroker のデフォルト http.Client.Transport.DialContext
// で `tcp4` のみ許可する。これでホスト側 DNS が AAAA を返しても A
// レコードだけが使われる。
//
// 以下 2 ペアの test:
//   - default HTTP client が IPv6 アドレスへの dial を拒否する (= tcp4 強制が効いている)
//   - default HTTP client が IPv4 アドレスへの dial は通す (= 副作用で v4 まで壊さない)

func newDefaultBrokerForDialTest(t *testing.T) *GmoBroker {
	t.Helper()
	// HTTPClient を明示しないことで NewGmoBroker のデフォルト client が
	// 構築される。これが IPv4 強制になっていることを検証する。
	return NewGmoBroker(GmoBrokerConfig{
		APIKey:    "k",
		APISecret: "s",
	})
}

func TestNewGmoBroker_DefaultHTTPClient_RefusesIPv6Dial(t *testing.T) {
	// Listen on IPv6 loopback only. If the dialer is dual-stack (default Go
	// behaviour) it will happily connect; with tcp4 forced it must fail.
	v6ln, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Skipf("IPv6 not available on this host: %v", err)
	}
	defer v6ln.Close()

	b := newDefaultBrokerForDialTest(t)
	tr, ok := b.http.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("broker http.Client.Transport must be *http.Transport (we set a custom one to force tcp4); got %T", b.http.Transport)
	}
	if tr.DialContext == nil {
		t.Fatal("broker http.Client.Transport.DialContext must be non-nil (tcp4-forcing dialer). Leaving it nil = dual-stack default = macOS prefers IPv6 = GMO ERR-5012")
	}

	_, derr := tr.DialContext(context.Background(), "tcp", v6ln.Addr().String())
	if derr == nil {
		t.Fatalf("DialContext must refuse IPv6 addresses; succeeded against %s", v6ln.Addr().String())
	}
}

func TestNewGmoBroker_DefaultHTTPClient_AllowsIPv4Dial(t *testing.T) {
	v4ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Skipf("IPv4 not available on this host: %v", err)
	}
	defer v4ln.Close()

	b := newDefaultBrokerForDialTest(t)
	tr, ok := b.http.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("expected *http.Transport; got %T", b.http.Transport)
	}

	conn, derr := tr.DialContext(context.Background(), "tcp", v4ln.Addr().String())
	if derr != nil {
		t.Fatalf("DialContext must succeed for IPv4 addresses; got %v", derr)
	}
	conn.Close()
}
