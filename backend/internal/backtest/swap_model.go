package backtest

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// SwapTable は symbol → side ("BUY"/"SELL") →
// 1,000 通貨あたり 円/晩 の符号付きスワップテーブル (負 = 支払い)。
// nil = スワップモデル無効 (back-compat: 既存 backtest の PnL は不変)。
//
// GMO のロールオーバーは NY クローズ = 21:00 UTC 跨ぎでカウントし、
// 水曜 (UTC) の跨ぎは週末ロール分で 3 倍になる (SwapNightsWeighted)。
type SwapTable map[string]map[string]float64

// SwapJPY は openedAt→closedAt の保有に対するスワップ合計 (円) を返す。
// テーブル値は 1,000 通貨基準なので qty/1,000 でスケールする。
// nil テーブル / 未登録 symbol / 未登録 side は 0 (= 影響なし)。
func (t SwapTable) SwapJPY(symbol, side string, openedAt, closedAt time.Time, qty int) float64 {
	if t == nil {
		return 0
	}
	perNight, ok := t[symbol][side]
	if !ok {
		return 0
	}
	nights := SwapNightsWeighted(openedAt, closedAt)
	return float64(nights) * perNight * float64(qty) / 1000.0
}

// SwapNightsWeighted は openedAt と closedAt の間 (両端 strict: openedAt <
// 跨ぎ瞬間 < closedAt) にある 21:00 UTC 跨ぎを数える。水曜 (UTC) の 21:00
// 跨ぎは週末ロール分として 3 泊で重み付けする。
func SwapNightsWeighted(openedAt, closedAt time.Time) int {
	if !closedAt.After(openedAt) {
		return 0
	}
	// openedAt より strict に後の最初の 21:00 UTC。
	o := openedAt.UTC()
	c := time.Date(o.Year(), o.Month(), o.Day(), 21, 0, 0, 0, time.UTC)
	if !c.After(openedAt) {
		c = c.Add(24 * time.Hour)
	}
	nights := 0
	for ; c.Before(closedAt); c = c.Add(24 * time.Hour) {
		switch c.Weekday() {
		case time.Wednesday:
			nights += 3 // 週末ロール (T+2: 土日分は水曜にまとめて付与)
		case time.Saturday, time.Sunday:
			// 市場閉場中 — ロールオーバーは発生しない (数えると週末分が
			// 水曜 3 倍と二重計上され ~29% 過大になる)。
		default:
			nights++
		}
	}
	return nights
}

// LoadSwapTable は -swap-table YAML を読む。フォーマット (1,000 通貨あたり 円/晩):
//
//	swap_table:
//	  USD_JPY: { BUY: 15.0, SELL: -18.0 }
func LoadSwapTable(path string) (SwapTable, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read swap table: %w", err)
	}
	var doc struct {
		SwapTable map[string]map[string]float64 `yaml:"swap_table"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("parse swap table: %w", err)
	}
	if len(doc.SwapTable) == 0 {
		return nil, fmt.Errorf("swap table %s: swap_table must be a non-empty map", path)
	}
	return SwapTable(doc.SwapTable), nil
}
