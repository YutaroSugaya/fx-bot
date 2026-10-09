// Package safety はランタイム安全機構 (emergency_stop など) のレイヤ依存ゼロ
// 実装を提供する。app / usecase / cmd の全レイヤから import 可能。
//
// 過去は app/emergency.go に同等関数があったが、usecase 側から循環依存を避けて
// import できないため、reconcile / execute_order / manage_open_positions /
// main.go の 4 箇所に flag 書き込みが個別実装されていた。本パッケージはその
// drift を解消する。
package safety

import (
	"fmt"
	"os"
	"sync/atomic"
	"time"
)

// onTrip は Trip / TripWithDetail 成功時に呼ばれる callback。app/counters 等から
// SetOnTrip で注入する。テスト時に nil でも安全 (atomic load → no-op)。
//
// グローバルだが「単一プロセスの bot で emergency trip 回数を集計するだけ」
// なので妥協。観測専用 (Trip 自体の挙動は変えない)。
var onTrip atomic.Pointer[func(reason string)]

// SetOnTrip は Trip 系関数が呼ばれた直後に発火する callback を登録する。
// 引数 nil で登録解除。テストで取り回す時に使う。
func SetOnTrip(fn func(reason string)) {
	if fn == nil {
		onTrip.Store(nil)
		return
	}
	onTrip.Store(&fn)
}

func notify(reason string) {
	if fn := onTrip.Load(); fn != nil {
		(*fn)(reason)
	}
}

// Active は runtime/emergency_stop.flag の存在で「稼働停止状態」を判定する。
// 空 path は常に false (テスト / 設定漏れの安全側デフォルト)。Cheap stat()
// なので priceTick / entry gate / API status から毎回呼んで良い。
func Active(path string) bool {
	if path == "" {
		return false
	}
	_, err := os.Stat(path)
	return err == nil
}

// Trip は flag に "<RFC3339> <reason>" を書き込む。空 path は no-op (=nil)。
// 既存 flag への上書きは許容 (最新理由が勝つ)。書き込み成功時に onTrip
// callback (登録されていれば) を発火する。
func Trip(path, reason string) error {
	if path == "" {
		notify(reason) // observability: path 空でも回数は数える (テスト容易性)
		return nil
	}
	body := fmt.Sprintf("%s %s\n", time.Now().UTC().Format(time.RFC3339), reason)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		return fmt.Errorf("trip emergency_stop: %w", err)
	}
	notify(reason)
	return nil
}

// TripWithDetail は Trip と同じだが、追加コンテキスト ("order=xxx cause=...")
// を 1 行に書く。usecase の criticalLiveFailure 系で order_id 等を残したい
// 用途。
func TripWithDetail(path, reason, detail string) error {
	if path == "" {
		notify(reason)
		return nil
	}
	body := fmt.Sprintf("%s %s %s\n", time.Now().UTC().Format(time.RFC3339), reason, detail)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		return fmt.Errorf("trip emergency_stop: %w", err)
	}
	notify(reason)
	return nil
}
