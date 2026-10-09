package broker

import (
	"context"
	"net/http"
	"testing"
)

// REGRESSION: GMO Forex の /v1/klines は interval=1day/1month で
// volume フィールドを返さない (intraday の 1min/5min 等は含む)。
// 全フィールド strict parsing (gmo_fx_parse.go) のままだと、
// daily/monthly レスポンスに対して r.Volume="" → parseGMOFloat fail → 関数全体
// fail → market_state の 1D/1M/3M が silently omit される。
//
// Fix: 空文字列は 0 とみなす (旧挙動互換)。空でない不正値だけ error に保つ。
func TestGetKlines_DailyWithoutVolume_DoesNotFail(t *testing.T) {
	srv := gmoTestServer(t, map[string]http.HandlerFunc{
		"/v1/klines": func(w http.ResponseWriter, _ *http.Request) {
			// GMO Forex daily/monthly: openTime/open/high/low/close のみ、volume なし
			_, _ = w.Write([]byte(`{
				"status":0,
				"data":[
					{"openTime":"1767301200000","open":"156.799","high":"157.005","low":"156.523","close":"156.983"},
					{"openTime":"1767560400000","open":"156.847","high":"157.301","low":"156.119","close":"156.451"}
				],
				"responsetime":"2026-05-27T01:00:00.000Z"
			}`))
		},
	})
	defer srv.Close()

	b := newGmoForTest(srv.URL, srv.URL)
	klines, err := b.GetKlines(context.Background(), "USD_JPY", "1day", "2026")
	if err != nil {
		t.Fatalf("daily klines without volume must NOT fail; got %v", err)
	}
	if len(klines) != 2 {
		t.Fatalf("expected 2 bars; got %d", len(klines))
	}
	if klines[0].Volume != 0 {
		t.Errorf("missing volume should default to 0; got %v", klines[0].Volume)
	}
	if klines[0].Close != 156.983 {
		t.Errorf("close: got %v want 156.983", klines[0].Close)
	}
}

// 不正値 (空ではないが ParseFloat 不能) は引き続き reject される regression guard
func TestGetKlines_NonEmptyMalformedVolumeStillRejected(t *testing.T) {
	srv := gmoTestServer(t, map[string]http.HandlerFunc{
		"/v1/klines": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{
				"status":0,
				"data":[
					{"openTime":"1767301200000","open":"156.799","high":"157.005","low":"156.523","close":"156.983","volume":"BROKEN"}
				]
			}`))
		},
	})
	defer srv.Close()

	b := newGmoForTest(srv.URL, srv.URL)
	_, err := b.GetKlines(context.Background(), "USD_JPY", "1min", "20260527")
	if err == nil {
		t.Fatal("non-empty malformed volume should still fail-loud")
	}
}
