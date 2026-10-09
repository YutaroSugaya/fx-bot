package safety

// TripReason は emergency_stop 発火理由の型安全な表現。
//
// 動機: Trip / TripWithDetail は string を
// 受けていたためアドホック文字列が散在し、grep / typo / observability が辛かった。
// よく使う理由を定数化し、新規追加時はここを編集する慣行を作る。
// 既存のアドホック文字列も TripReason("...") キャストで通せる (後方互換)。
//
// 慣行:
//   - 全 reason は snake_case (":" や "/" を含めない)
//   - Source プレフィックス付き ("manual_...", "execute_...", "reconcile_...")
//   - 動的 ID は detail パラメータに渡す (Reason の中に concat しない)
type TripReason string

// String returns the underlying snake_case reason string.
func (r TripReason) String() string { return string(r) }

// よく使われる well-known reasons.
// 新規追加時はここに追記して、callsite 側はリテラル文字列ではなく定数を使う。
const (
	// app/bootstrap.go: startup の candle restore でエラー
	ReasonCandleRestoreFailed TripReason = "candle_restore_failed"

	// app/handler/emergency_handler.go: dashboard 経由の手動 trip
	ReasonManualViaAPI TripReason = "manual_via_api"
)

// TripFor は TripReason 版の Trip。
// 内部実装は Trip と同じ (path 空は no-op、onTrip callback あり)。
func TripFor(path string, reason TripReason) error {
	return Trip(path, string(reason))
}

// TripForWithDetail は TripReason 版の TripWithDetail。
// detail は order_id / broker_position_id などの動的コンテキスト。
func TripForWithDetail(path string, reason TripReason, detail string) error {
	return TripWithDetail(path, string(reason), detail)
}
