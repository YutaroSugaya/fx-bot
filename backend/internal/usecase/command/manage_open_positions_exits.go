package command

import (
	"time"

	"fx-bot/backend/internal/domain/market"
	"fx-bot/backend/internal/domain/order"
	"fx-bot/backend/internal/port"
)

// 終了条件 evaluator を純粋関数に分離している。
//
// ratchet / early-exit / max_hold / TP/SL を 1 関数に混在させると
// 各分岐の単体テストが難しくなるため。各 evaluator は
//   - 引数: rec / ticker / now / pipSize の純粋入力
//   - 戻り値: 発火時の close_reason、未発火なら ""
// として副作用なし。優先順位は ManageOpenPositions.evaluateExit が決定する
// (ratchet → max_hold(early/extension含む) → paper TP/SL)。

// sessionFlattenWindowMinutes: flatten 窓の幅。開始 tick を万一取り逃しても
// (一時的な ticker 断など) 窓の間は掃除を再試行し、窓を過ぎたら諦めて OCO に任せる。
const sessionFlattenWindowMinutes = 30

// evaluateSessionFlattenExit は毎朝の強制手仕舞いを判定する。
// 目的: 毎朝 05:45 JST に GMO がスプレッドを 10〜14pips へ開く「壁」で、SL(ask/bid
// ジャンプで機械トリガー) や ratchet(giveback 超過と誤認) が広スプレッドへ market
// close する構造事故を、壁の 15 分前に通常スプレッドで手仕舞って断つ。
// 土曜のこの回は週末ギャップ回避 (金曜 NY クローズ時点で玉を持ち越さない) を兼ねる。
//
//   - now の JST minutes-of-day が [startMinuteJST, +30min) にある間だけ発火
//   - 窓の中で建った玉は対象外 (建てた直後の自食い防止; 02-06 時新規禁止で通常起きない)
//   - JST は jstHourIn と同じ固定 UTC+9 (DST なし)
func evaluateSessionFlattenExit(rec port.PositionRecord, now time.Time, startMinuteJST int) string {
	jstNow := now.UTC().Add(9 * time.Hour)
	minutes := jstNow.Hour()*60 + jstNow.Minute()
	if minutes < startMinuteJST || minutes >= startMinuteJST+sessionFlattenWindowMinutes {
		return ""
	}
	windowStart := time.Date(jstNow.Year(), jstNow.Month(), jstNow.Day(),
		startMinuteJST/60, startMinuteJST%60, 0, 0, time.UTC)
	openedJST := rec.OpenedAt.UTC().Add(9 * time.Hour)
	if !openedJST.Before(windowStart) {
		return "" // opened inside this window occurrence
	}
	return "session_flatten"
}

// evaluateRatchetExit は trailing TP の発火を判定する。
// armed && peak から RatchetGivebackPips 戻ったら "ratchet_takeprofit"。
// Live でも発火する (GMO OCO は trailing をサポートしないので bot 側で判定)。
func evaluateRatchetExit(rec port.PositionRecord, t market.Ticker, pipSize float64) string {
	if rec.RatchetArmPips <= 0 || !rec.RatchetArmed || pipSize <= 0 {
		return ""
	}
	unrealized, ok := unrealizedPipsExit(rec, t, pipSize)
	if !ok {
		return ""
	}
	// 1e-9 epsilon は early_exit と同様、IEEE 754 の丸め誤差で
	// "ちょうど閾値" の retrace を取り逃すのを防ぐ。
	if rec.PeakUnrealizedPips-unrealized >= rec.RatchetGivebackPips-1e-9 {
		return "ratchet_takeprofit"
	}
	return ""
}

// evaluateLossRatchetExit は trailing STOP (損切り側 ratchet) の発火を判定する。
// 利確 ratchet (evaluateRatchetExit) の鏡像:
//   - 利確: peak (最良益) から RatchetGivebackPips 戻ったら "ratchet_takeprofit"
//   - 損切: trough (最悪損) から RatchetGivebackPips 戻ったら "ratchet_stoploss"
//
// LossRatchetArmed は trough が -RatchetArmPips に達したとき OnTick で立つ。
// arm/giveback は利確側と同じ RatchetArmPips / RatchetGivebackPips を共用する。
// 「深い含み損から少し戻したら浅い傷で撤退」= broker OCO の満額 SL を待たずに
// 損失を圧縮する狙い。Live でも発火する (broker OCO は trailing 非対応なので
// bot 側で判定。満額 SL は OCO が backstop として残る)。
func evaluateLossRatchetExit(rec port.PositionRecord, t market.Ticker, pipSize float64) string {
	if rec.RatchetArmPips <= 0 || !rec.LossRatchetArmed || pipSize <= 0 {
		return ""
	}
	unrealized, ok := unrealizedPipsExit(rec, t, pipSize)
	if !ok {
		return ""
	}
	// trough から giveback だけ回復したら損切り。1e-9 epsilon は利確側と同様、
	// IEEE 754 の丸め誤差で "ちょうど閾値" の回復を取り逃すのを防ぐ。
	if unrealized-rec.TroughUnrealizedPips >= rec.RatchetGivebackPips-1e-9 {
		return "ratchet_stoploss"
	}
	return ""
}

// evaluateMaxHoldExit は MaxHold 関連 (early_exit + soft/hard deadline) を判定する。
// 発火時の close_reason:
//   - Early-exit window 発火 → "early_exit" (soft 到達前の救済 close)
//   - soft/hard deadline / extension 強制 close → "max_hold"
//
// 両者を "max_hold" で統一すると early-exit 撤退と本来の deadline
// close を集計で分離できず分析を歪めるため、別ラベルにしている。
//
// 評価順:
//  1. Early-exit window (EarlyExitWindowMinutes > 0): soft 直前の窓で
//     unrealized_pips >= EarlyExitTargetPips なら close ("early_exit")
//  2. Hard deadline (soft + ExtensionMaxMinutes) 到達なら必ず close ("max_hold")
//  3. Soft deadline 到達 + extension 未設定なら close ("max_hold")
//  4. Soft deadline 到達 + extension 設定 + |unrealized| > threshold なら close ("max_hold")
//  5. それ以外は extension grace で待機 ("")
func evaluateMaxHoldExit(rec port.PositionRecord, t market.Ticker, now time.Time, pipSize float64) string {
	if rec.MaxHoldMinutes <= 0 || rec.OpenedAt.IsZero() {
		return ""
	}
	elapsed := now.Sub(rec.OpenedAt)
	soft := time.Duration(rec.MaxHoldMinutes) * time.Minute
	hard := soft + time.Duration(rec.ExtensionMaxMinutes)*time.Minute

	// 1. Early-exit window (soft 到達前のみ)
	if rec.EarlyExitWindowMinutes > 0 && pipSize > 0 && elapsed < soft {
		windowStart := soft - time.Duration(rec.EarlyExitWindowMinutes)*time.Minute
		if elapsed >= windowStart {
			pnl, ok := unrealizedPipsMid(rec, t, pipSize)
			if !ok {
				// Defensive: CHECK 制約で BUY/SELL のみだが、constraint が緩んだ
				// 場合 target が負だと毎 tick で誤発火するのを早期 return で回避。
				return ""
			}
			// Epsilon: 1e-9 で IEEE 754 丸め誤差を吸収。
			if pnl >= rec.EarlyExitTargetPips-1e-9 {
				return "early_exit"
			}
		}
	}

	// 2. Hard deadline
	if elapsed >= hard {
		return "max_hold"
	}

	// 3-5. Soft deadline と extension grace
	if elapsed >= soft {
		if rec.ExtensionMaxMinutes <= 0 || rec.ExtensionUnrealizedPipsThreshold <= 0 {
			return "max_hold"
		}
		if pipSize == 0 {
			return "max_hold"
		}
		unrealizedPips, _ := unrealizedPipsMid(rec, t, pipSize)
		// 非対称 extension。勝ち / フラット (unrealized ≥
		// -threshold) は「動いてる/伸びてる間は延ばす」で wait し、トレンドの
		// 続伸を待つ (利確側は ratchet が担当)。負けが -threshold を超えたら
		// soft deadline で損切り close する。hard deadline (上の elapsed>=hard)
		// では勝ち負けに関わらず必ず close 済み。
		//
		// |unrealized| > threshold で勝ちも close すると (= 勝ちを
		// deadline で取り上げると) トレンドを伸ばせない。
		if unrealizedPips < -rec.ExtensionUnrealizedPipsThreshold {
			return "max_hold"
		}
		// flat or winning — wait (extension grant; winner は ratchet/TP に委ねる)。
	}
	return ""
}

// evaluatePaperTPSLExit は paper モードの TP/SL を判定する。
// Live モードでは呼んではいけない (GMO OCO がサーバ側で処理するため、
// bot 側で TP/SL を判定すると OCO と race する)。Live 判定は呼出側の責務。
func evaluatePaperTPSLExit(rec port.PositionRecord, t market.Ticker, pipSize float64) string {
	if pipSize == 0 {
		return ""
	}
	tpDelta := rec.TakeProfitPips * pipSize
	slDelta := rec.StopLossPips * pipSize

	switch rec.Side {
	case string(order.SideBuy):
		if rec.TakeProfitPips > 0 && t.Bid >= rec.EntryPrice+tpDelta {
			return "take_profit"
		}
		if rec.StopLossPips > 0 && t.Bid <= rec.EntryPrice-slDelta {
			return "stop_loss"
		}
	case string(order.SideSell):
		if rec.TakeProfitPips > 0 && t.Ask <= rec.EntryPrice-tpDelta {
			return "take_profit"
		}
		if rec.StopLossPips > 0 && t.Ask >= rec.EntryPrice+slDelta {
			return "stop_loss"
		}
	}
	return ""
}
