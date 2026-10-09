package broker

import (
	"testing"

	"fx-bot/backend/internal/port"
)

// TestGmoBroker_SatisfiesLiveBroker は GmoBroker が port.LiveBroker
// (Broker + ExecutionResolver) を実装し続けることをコンパイル時に保証する。
// シグネチャ drift が起きると compile error。
func TestGmoBroker_SatisfiesLiveBroker(t *testing.T) {
	var _ port.LiveBroker = (*GmoBroker)(nil)
}

// PaperBroker は LiveBroker を満たさないことを確認 (ExecutionResolver なし)。
// ここでは "should NOT compile" を直接 assert できないが、メソッドセットの
// 存在確認だけ行い、誤って実装されたら他テストで気付ける位置に置く。
func TestPaperBroker_SatisfiesBrokerNotLive(t *testing.T) {
	var _ port.Broker = (*PaperBroker)(nil)
	// 以下をコメントイン解除すると compile error になることを期待:
	// var _ port.LiveBroker = (*PaperBroker)(nil)
}
