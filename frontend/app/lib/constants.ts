import { parseDisplayEpoch, parseDisplaySymbols } from './display_scope'

// lot ↔ 通貨単位の換算定数。
//
// USD_JPY の 1 lot = 10,000 通貨。バックエンドは「通貨単位」で持ち、
// UI 側のみで lot 表記に変換する。複数 component から参照したいので
// page.tsx から切り出した。
export const LOT_SIZE_USDJPY = 10000

// DISPLAY_SYMBOLS: per-symbol 表示(通貨ペアタブ・LLM 判断パネル・v2 パネル)をこのリストに絞る
// 表示専用フィルタ。frontend/.env.local の NEXT_PUBLIC_DISPLAY_SYMBOLS(例: USD_JPY)で決める。
// 未設定 = 全ペア表示。backend は bot_config の全シンボルを返し続ける(建玉管理・reconcile は継続)。
// ⚠️ OPEN ポジションを持つペアはスコープ外でも必ず表示される(lib/display_symbols.ts の
// 可視性原則 — 外部 / 手動の玉が現れたとき見えないと止められない)。
export const DISPLAY_SYMBOLS: string[] = parseDisplaySymbols(process.env.NEXT_PUBLIC_DISPLAY_SYMBOLS)

// TRADES_DISPLAY_EPOCH: ダッシュボードの戦績(累計損益・勝率・通貨別 / 日別テーブル・取引履歴)を
// 「今動かしている戦略」の期間だけに絞る表示下限。NEXT_PUBLIC_TRADES_DISPLAY_EPOCH に ISO 時刻
// (例: 2020-01-06T07:00:00+09:00)を入れる。未設定 = 全期間。
// 判定は opened_at(エントリー時刻)基準: トレードは建てた時点の戦略に帰属する(config は建玉時に
// 凍結保存)ので、epoch 前に建てた持ち越し玉が epoch 後に決済されても戦績に混ぜない。
// ⚠️ 表示専用。DB の行は消さない・backend の反省ループの起点(reflection_start_at)とは別物。
// 変えたら next dev を再起動する(NEXT_PUBLIC_* はビルド時に埋め込まれる)。
export const TRADES_DISPLAY_EPOCH: string = parseDisplayEpoch(process.env.NEXT_PUBLIC_TRADES_DISPLAY_EPOCH)
