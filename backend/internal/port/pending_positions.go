package port

import "time"

// PendingPositionTracker は entry saga 進行中 (= broker fill 済みだが DB INSERT
// 未完) の broker_position_id を一時的に保持するための tracker 契約。
//
// 動機:
//
//	bot の Live entry は (1) MARKET 発注 → GMO 側で約定 (positionId 発行) →
//	(2) ResolveExecution → (3) PlaceSettleOCO → (4) ResolveSettleLegs →
//	(5) DB INSERT の順で進む。(1)〜(5) の間に reconcile が並行で走ると、
//	GetOpenPositions は当該 positionId を返してくる一方 DB にはまだ存在しないため
//	「裸 broker position」と誤検出 → Live で adoptExternalLivePosition 経路 →
//	何らかの理由で active config resolve に失敗すると emergency_stop trip。
//	数秒の race-window でも trip し得て、trip 後は advisor が config を
//	更新できなくなる二次障害につながる。
//
// 契約:
//   - MarkPending: ResolveExecution で positionId を取った直後に呼ぶ
//   - MarkResolved: DB INSERT 成功 / 失敗 / rollback いずれの終端でも必ず呼ぶ
//   - IsPending: reconcile の naked-broker handler 入口で確認
//   - StaleIDs: saga crash で終端 defer が呼ばれず永久 pending になった id の
//     検出。reconcile は TTL 超過 pending を skip 対象から外す
//
// 注: in-memory 実装で十分。bot restart 中の race は対象外 (restart 後の reconcile
// は startup adoption 経路で正しく取り扱う)。
type PendingPositionTracker interface {
	MarkPending(brokerPositionID string)
	MarkResolved(brokerPositionID string)
	IsPending(brokerPositionID string) bool
	StaleIDs(ttl time.Duration) []string
}
