package broker

import (
	"context"
	"testing"
)

// ResolveExecution が fee / settledSwap を
// 捨てずに port.ResolvedExecution として返すことを検証する。複数 fill (部分約定)
// の場合、positionId / price は先頭 fill 由来 (既存セマンティクス維持) だが、
// コストは全 fill の合算 — でなければ部分約定で手数料を過少報告する。
func TestGmoBroker_ResolveExecution_AggregatesCostsAcrossFills(t *testing.T) {
	executionsData := map[string]any{
		"list": []map[string]any{
			{
				"executionId": 1, "orderId": 12345, "positionId": 9876,
				"symbol": "USD_JPY", "side": "BUY", "size": "500",
				"price": "150.10", "timestamp": "2026-06-10T12:00:01Z",
				"fee": "-4", "settledSwap": "-1", "lossGain": "60",
			},
			{
				"executionId": 2, "orderId": 12345, "positionId": 9876,
				"symbol": "USD_JPY", "side": "BUY", "size": "500",
				"price": "150.12", "timestamp": "2026-06-10T12:00:02Z",
				"fee": "-3", "settledSwap": "-1", "lossGain": "40",
			},
		},
	}
	srv := fakeGMOServer(t, map[string]string{
		"/v1/executions": fixtureResponse(executionsData),
	})
	br := NewGmoBroker(GmoBrokerConfig{
		PublicBaseURL:  srv.URL,
		PrivateBaseURL: srv.URL,
		APIKey:         "k", APISecret: "s",
	})

	res, err := br.ResolveExecution(context.Background(), "12345")
	if err != nil {
		t.Fatalf("ResolveExecution: %v", err)
	}
	if res.PositionID != "9876" {
		t.Errorf("PositionID: got %q want 9876", res.PositionID)
	}
	if res.Price != 150.10 {
		t.Errorf("Price: got %v want 150.10 (first fill)", res.Price)
	}
	if res.FeeJPY != 7 {
		t.Errorf("FeeJPY: got %v want 7 (wire は負=徴収、内部は正=コストで合算)", res.FeeJPY)
	}
	if res.SettledSwapJPY != -2 {
		t.Errorf("SettledSwapJPY: got %v want -2 (sum over fills)", res.SettledSwapJPY)
	}
	if res.LossGainJPY != 100 {
		t.Errorf("LossGainJPY: got %v want 100 (sum over fills)", res.LossGainJPY)
	}
}
