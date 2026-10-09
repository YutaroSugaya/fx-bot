package broker

import (
	"encoding/json"
	"testing"
)

// GMO /v1/executions returns fee / settledSwap / lossGain per fill; the parser
// must capture them (not discard them). Capturing them is the prerequisite for net-of-cost edge judgment (the round-trip cost floor
// includes the 0.002%-per-side API fee). Missing fields must default to 0 (tolerant).
//
// 符号規約: GMO の wire 形式はキャッシュフロー符号
// (amount = lossGain + fee + settledSwap、公式 docs の例は fee:"-30" = 徴収)。
// 内部規約は「FeeJPY 正 = コスト」なので decode 境界で fee を符号反転する。
// settledSwap は受取が正のままで net = gross − fee + swap と整合するため raw 維持。
func TestDecodeExecutionList_CapturesFeeSwapLossGain(t *testing.T) {
	data := json.RawMessage(`{"list":[{"executionId":"1","orderId":"2","positionId":"3","symbol":"USD_JPY","side":"BUY","size":"1000","price":"156.30","timestamp":"2026-06-10T12:00:00.000Z","fee":"-7","settledSwap":"-2","lossGain":"123"}]}`)
	ex, err := decodeExecutionList("test", data)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(ex) != 1 {
		t.Fatalf("len=%d, want 1", len(ex))
	}
	if ex[0].FeeJPY != 7 {
		t.Errorf("FeeJPY=%v, want 7 (wire -7 = 徴収 → 内部 正=コスト)", ex[0].FeeJPY)
	}
	if ex[0].SettledSwapJPY != -2 {
		t.Errorf("SettledSwapJPY=%v, want -2", ex[0].SettledSwapJPY)
	}
	if ex[0].LossGainJPY != 123 {
		t.Errorf("LossGainJPY=%v, want 123", ex[0].LossGainJPY)
	}
}

func TestDecodeExecutionList_MissingCostFieldsDefaultZero(t *testing.T) {
	data := json.RawMessage(`{"list":[{"executionId":"1","orderId":"2","positionId":"3","symbol":"USD_JPY","side":"BUY","size":"1000","price":"156.30","timestamp":"2026-06-10T12:00:00.000Z"}]}`)
	ex, err := decodeExecutionList("test", data)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if ex[0].FeeJPY != 0 || ex[0].SettledSwapJPY != 0 || ex[0].LossGainJPY != 0 {
		t.Errorf("missing cost fields must default to 0; got fee=%v swap=%v lossGain=%v",
			ex[0].FeeJPY, ex[0].SettledSwapJPY, ex[0].LossGainJPY)
	}
}
