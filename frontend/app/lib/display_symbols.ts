import { DISPLAY_SYMBOLS } from './constants'

// ダッシュボードの per-symbol 表示(タブ・LLM 判断・v2 パネル)を「今動かしているペア」だけに
// 絞る表示専用フィルタ。スコープは constants.ts の DISPLAY_SYMBOLS(env)で決まり、DB・backend は無変更。
//
// 可視性原則(全判断・全約定を画面に出す): スコープ外のペアでも OPEN ポジションが
// あるものは絶対に隠さない(外部 / 手動の玉が現れたら見えないと止められない)。
export function isDisplayedSymbol(
  symbol: string,
  openSymbols: string[] = [],
  scope: string[] = DISPLAY_SYMBOLS,
): boolean {
  if (scope.length === 0) return true // 空 = 全ペア表示(旧挙動)
  return scope.includes(symbol) || openSymbols.includes(symbol)
}
