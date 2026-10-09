package broker

import (
	"fmt"
	"strconv"
	"time"
)

// gmo_fx.go 内で strconv / time.Parse の error を `_ :=` で握り潰すと、GMO が
// 壊れた値を返したときに price=0 / timestamp=zero の ticker/position/order が
// 下流に流れる。これらヘルパで field 名 + raw 値を付けたエラーを必ず返すように
// 統一する。
//
// 命名: parseGMO* (parseFloat / parseInt(=Atoi) / parseInt64 / parseTime)。
// すべて wrap 形式: "gmo parse <field>=<raw>: <cause>"。

func parseGMOFloat(field, raw string) (float64, error) {
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0, fmt.Errorf("gmo parse %s=%q: %w", field, raw, err)
	}
	return v, nil
}

// parseGMOFloatOptional は省略可能な数値フィールド用 (約定の fee /
// settledSwap / lossGain)。空文字 / 欠損は 0 を返す (error にしない)。空でない
// 値が壊れているときだけ error。GMO がこれらを返さない約定でも fail させない。
func parseGMOFloatOptional(field, raw string) (float64, error) {
	if raw == "" {
		return 0, nil
	}
	return parseGMOFloat(field, raw)
}

func parseGMOAtoi(field, raw string) (int, error) {
	v, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("gmo parse %s=%q: %w", field, raw, err)
	}
	return v, nil
}

func parseGMOInt64(field, raw string) (int64, error) {
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("gmo parse %s=%q: %w", field, raw, err)
	}
	return v, nil
}

// parseGMOTime parses GMO のタイムスタンプ。GMO は ".000Z" 付き RFC3339
// (e.g. "2026-05-15T01:00:00.000Z") を返すが time.RFC3339Nano は許容する。
func parseGMOTime(field, raw string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		// fallback: ナノ秒精度 ".000Z" 形式
		t, err2 := time.Parse(time.RFC3339Nano, raw)
		if err2 == nil {
			return t, nil
		}
		return time.Time{}, fmt.Errorf("gmo parse %s=%q: %w", field, raw, err)
	}
	return t, nil
}
