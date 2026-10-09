// 価格の表示桁数は通貨ペアの quote 通貨で変わる。
//
//   _JPY quote (USD_JPY=160.346, GBP_JPY=205.123)  pip=0.01   → 小数3桁
//   USD quote (GBP_USD=1.33824, EUR_USD=1.08000)   pip=0.0001 → 小数5桁
//
// どちらも「0.1 pip」の分解能になる桁数。全ペアを toFixed(3) で表示すると
// GBP_USD の 1.33824 と 1.33795 がどちらも "1.338" に潰れ、エントリー=現在値
// に見えるのに -2.9 pips の損益が出ている、という矛盾した表示になる。
//
// TradingChart.tsx の priceFormatForSymbol と同じ規則をここに集約し、
// ポジションカード・市況パネル・約定結果でも共有する。
export function priceDecimals(symbol: string): number {
  return symbol.endsWith('_JPY') ? 3 : 5
}

// formatPrice は symbol の精度で価格を文字列化する。undefined / null / NaN は
// クラッシュさせず "N/A" を返す (format_pnl.ts の堅牢化と同方針)。
export function formatPrice(symbol: string, price: number): string {
  if (price == null || Number.isNaN(price)) return 'N/A'
  return price.toFixed(priceDecimals(symbol))
}
