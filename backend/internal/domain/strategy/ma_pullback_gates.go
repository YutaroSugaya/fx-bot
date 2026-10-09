package strategy

import (
	"fmt"
	"math"

	"fx-bot/backend/internal/domain/market"
	"fx-bot/backend/internal/domain/ta"
)

// Gate is one step of the ma_pullback entry funnel, for the dashboard's
// "現在どこまで通過していて、次に何が必要か" view. OK=true means that condition is
// currently met; Detail carries the live numbers behind it. The gates are
// returned in evaluation order, so the FIRST OK=false is the active blocker.
type Gate struct {
	Key    string `json:"key"`    // "spread" | "data" | "trend" | "zone" | "side" | "confluence" | "rebound"
	OK     bool   `json:"ok"`     // condition currently satisfied
	Detail string `json:"detail"` // live numbers (e.g. "1.5 / 上限 1.0 pip")
}

// Gates re-runs the entry checks WITHOUT short-circuiting and reports each one's
// live status, so the UI can show the full funnel (✓ passed / ✗ blocked / what's
// needed). It mirrors Evaluate's gate order and thresholds exactly; a test pins
// the first failing gate's Key to Evaluate's Reason so the two can't drift.
func (m MAPullback) Gates(in EvalInput) []Gate {
	if in.Config == nil || in.Summary == nil {
		return nil
	}
	// Evaluate と同じ実効パラメータを使う (zero-value = 固定デフォルト)。
	p := m.P.effective()
	// 不正注入ガード: Evaluate と同じ panic-safe 退避。
	if p.TrendSlopeLookback1h < 1 || p.SwingLookback < 1 {
		return nil
	}
	pip := market.PipSize(in.Config.Symbol)
	gates := make([]Gate, 0, 7)
	add := func(key string, ok bool, detail string) {
		gates = append(gates, Gate{Key: key, OK: ok, Detail: detail})
	}

	// 1) spread guard.
	spr := in.Summary.CurrentRate.SpreadPips
	maxSpr := in.Config.Entry.MaxSpreadPips
	add("spread", spr <= maxSpr, fmt.Sprintf("%.1f / 上限 %.1f pip", spr, maxSpr))

	// 2) trend = 1h 200SMA slope (MTF: the big flow). Needs maPB1hMinBars 1h bars.
	n1 := len(in.Candles1h)
	if n1 < p.min1hBars() || pip <= 0 {
		add("trend", false, fmt.Sprintf("1H足の蓄積待ち (%d / %d 本)", n1, p.min1hBars()))
		add("data", false, "—")
		add("zone", false, "—")
		add("side", false, "—")
		add("confluence", false, "—")
		add("rebound", false, "—")
		return gates
	}
	closes1h := closesOf(in.Candles1h)
	sma1h, _ := ta.SMA(closes1h, maPB1hPeriod)
	sma1hPrev, _ := ta.SMA(closes1h[:len(closes1h)-p.TrendSlopeLookback1h], maPB1hPeriod)
	slope1hPips := (sma1h - sma1hPrev) / pip
	H := maTrendDir(sma1h, sma1hPrev, p.TrendMinSlopePips, pip)
	trendJP := map[trendDir]string{trendUp: "上昇", trendDown: "下降", trendFlat: "横ばい"}[H]
	add("trend", H != trendFlat, fmt.Sprintf("1H 200SMA傾き %+.1fp (要±%.0f) → %s", slope1hPips, p.TrendMinSlopePips, trendJP))

	// 3) enough 5m bars for the 200MA (execution chart).
	n5 := len(in.Candles5m)
	add("data", n5 >= maPBMin5mBars, fmt.Sprintf("%d / %d 本", n5, maPBMin5mBars))
	if n5 < maPBMin5mBars {
		add("zone", false, "—")
		add("side", false, "—")
		add("confluence", false, "—")
		add("rebound", false, "—")
		return gates
	}

	closes := closesOf(in.Candles5m)
	sma, _ := ta.SMA(closes, maPB5mPeriod)
	ema, _ := ta.EMA(closes, maPB5mPeriod)

	// 4) pullback to the 5m MA zone.
	price := (in.Summary.CurrentRate.Bid + in.Summary.CurrentRate.Ask) / 2
	atrPips := atrPipsOf(in.Candles5m, maPBATRWindow, pip)
	tolPips := p.ZoneATR * atrPips
	minDistPips := math.Min(math.Abs(price-sma), math.Abs(price-ema)) / pip
	add("zone", inMAZone(price, sma, ema, tolPips*pip), fmt.Sprintf("MAまで %.1fp (ゾーン ±%.1fp)", minDistPips, tolPips))

	// 5) price on the trend side of the MA.
	sideOK := priceOnTrendSide(price, sma, H)
	wantSide := "MA上"
	if H == trendDown {
		wantSide = "MA下"
	}
	sidePos := "MA上"
	if price < sma {
		sidePos = "MA下"
	}
	add("side", sideOK, fmt.Sprintf("価格 %s (要 %s)", sidePos, wantSide))

	// 6) confluence with a recent swing S/R.
	cWin := lastN(in.Candles5m, p.SwingLookback)
	highs := ta.SwingHighs(cWin, maPBSwingN, pip, p.SwingPromPips)
	lows := ta.SwingLows(cWin, maPBSwingN, pip, p.SwingPromPips)
	conf := hasConfluence(price, confluenceLevels(in.Candles5m, highs, lows), p.ConfluenceATR*atrPips*pip)
	confDetail := "節目との重なりなし"
	if conf {
		confDetail = "近くに節目あり"
	}
	add("confluence", conf, confDetail)

	// 7) rebound confirmation on the last 5m bar (needs a known trend direction).
	last := in.Candles5m[len(in.Candles5m)-1]
	if H == trendFlat {
		add("rebound", false, "トレンド確定後に判定")
	} else {
		wantBar := "陽線"
		if H == trendDown {
			wantBar = "陰線"
		}
		gotBar := "陽線"
		switch {
		case last.Close < last.Open:
			gotBar = "陰線"
		case last.Close == last.Open:
			gotBar = "同値"
		}
		add("rebound", reboundConfirmed(last, H), fmt.Sprintf("最新5m足 %s (要 %s)", gotBar, wantBar))
	}
	return gates
}
