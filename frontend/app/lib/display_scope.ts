// ダッシュボードの表示スコープを env から読む。どちらも表示専用で、DB と backend は変えない。

// parseDisplayEpoch は戦績(累計損益・勝率・通貨別/日別・取引履歴)の表示下限を返す。
// 空文字 = 絞らない。解釈できない値も空扱いにする(全件が消えて「取引ゼロ」に見えるのを防ぐ)。
export function parseDisplayEpoch(raw: string | undefined): string {
  const v = (raw ?? '').trim()
  if (v === '' || Number.isNaN(Date.parse(v))) return ''
  return v
}

// parseDisplaySymbols は per-symbol 表示(タブ・判断パネル)に出すペアを返す。空配列 = 全ペア。
export function parseDisplaySymbols(raw: string | undefined): string[] {
  return (raw ?? '')
    .split(',')
    .map((s) => s.trim().toUpperCase())
    .filter((s) => s !== '')
}
