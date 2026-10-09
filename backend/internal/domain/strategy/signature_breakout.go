package strategy

import (
	"fmt"
	"time"

	"fx-bot/backend/internal/config"
	"fx-bot/backend/internal/domain/market"
	"fx-bot/backend/internal/domain/order"
	"fx-bot/backend/internal/domain/ta"
)

// === advisor v2 — deterministic signature-setup detector (classic chart-breakout model) ===
//
// This is the DETERMINISTIC half of advisor v2. It supplies
// only the GEOMETRY of one signature setup — a with-trend daily breakout of a clean level with
// a measured-move target — and the R:R math. It does NOT decide to trade: the periodic advisor
// (Claude) grades pattern quality + multi-axis alignment on top and may say no_trade; the risk
// Gate can still veto. The LLM never sees tunable parameters, only "is this clean / do axes agree".
//
// Pure & side-effect free (like every Strategy). Constants are FIXED, not swept (sweeping geometry
// over the data is overfitting). countertrend is a
// future label; the detector currently emits trend_continuation or no_trade only (trend-only first).

// BreakoutLabel classifies the setup; it deterministically selects the exit logic downstream
// (trend_continuation -> ratchet/let-run, countertrend_reversion -> fixed TP).
type BreakoutLabel string

const (
	BreakoutNoTrade      BreakoutLabel = "no_trade"
	BreakoutTrendCont    BreakoutLabel = "trend_continuation"
	BreakoutCountertrend BreakoutLabel = "countertrend_reversion"
)

// SignatureParams holds the FIXED geometry constants. Injected so tests can use small periods;
// production uses DefaultSignatureParams (daily 200-SMA trend, Donchian level, ATR-scaled stop).
type SignatureParams struct {
	SMAPeriod    int     // trend filter SMA period (daily) — default 200
	SMASlopeLB   int     // bars back to measure the SMA slope — default 20
	MinSlopePips float64 // SMA must drift >= this over the lookback to count as a trend
	DonchianBars int     // consolidation/level window before the breakout bar — default 20
	ATRWindow    int     // ATR window for volatility-scaled stop — default 14
	DispMultATR  float64 // breakout bar body must be >= this * ATR (conviction close, not a wick)
	SLBufferATR  float64 // invalidation = level -/+ this * ATR (failed-breakout stop, cut-fast)
	MinRR        float64 // hard minimum reward:risk at entry — default 2.5
}

// DefaultSignatureParams = classic breakout/Turtle geometry, daily timeframe. Not tuned to our data.
func DefaultSignatureParams() SignatureParams {
	return SignatureParams{
		SMAPeriod:    200,
		SMASlopeLB:   20,
		MinSlopePips: 50.0, // daily moves are large; 50 pips drift over 20 days = a real trend
		DonchianBars: 20,
		ATRWindow:    14,
		DispMultATR:  0.5,
		SLBufferATR:  1.0,
		MinRR:        2.5,
	}
}

// BreakoutProposal is the deterministic candidate the advisor then judges. It is NOT an order.
type BreakoutProposal struct {
	Found        bool
	Label        BreakoutLabel
	Side         order.Side
	Level        float64 // the broken structural level (resistance for up, support for down)
	Entry        float64 // reference entry (breakout bar close)
	Invalidation float64 // structural stop level (failed-breakout, ATR-buffered)
	TargetRef    float64 // measured-move reference target (level +/- range height)
	ATRPips      float64
	StopPips     float64 // |entry - invalidation| in pips
	RewardPips   float64 // |targetRef - entry| in pips
	RR           float64 // reward / risk
	Reason       string
}

func notFound(reason string) BreakoutProposal {
	return BreakoutProposal{Found: false, Label: BreakoutNoTrade, Reason: reason}
}

// CompletedDailyBars drops a trailing STILL-FORMING daily bar so the detector only ever judges a
// CONFIRMED daily close (matching the backtest, which uses completed bars). Brokers' daily klines
// include the current day's forming bar (its "close" = the live price); judging that bar would fire
// on intra-day pokes that can reverse before the close. A bar is treated as forming if its open
// time is within the last 24h. Pure.
func CompletedDailyBars(daily []market.Candle, now time.Time) []market.Candle {
	n := len(daily)
	if n == 0 {
		return daily
	}
	if now.Sub(daily[n-1].OpenTime) < 24*time.Hour {
		return daily[:n-1]
	}
	return daily
}

// AdvisorVerdict is the domain-level result of the periodic advisor v2 (Claude) judgment on a
// proposal: the go/no-go gate + the chosen label. Geometry fields are echoes for audit — the
// deterministic BreakoutProposal remains authoritative for the actual stop/levels. The LLM can
// only gate (and the risk Gate can still veto); it never sets the stop.
type AdvisorVerdict struct {
	Go           bool
	Label        BreakoutLabel
	Side         order.Side
	Level        float64
	ATRPips      float64
	Invalidation float64
	Reason       string
}

// DetectSignatureBreakout finds a with-trend daily breakout (classic breakout geometry) on completed bars.
// daily must be completed daily candles, oldest first; the LAST element is the breakout bar.
func DetectSignatureBreakout(daily []market.Candle, pip float64, p SignatureParams) BreakoutProposal {
	if pip <= 0 {
		return notFound("no_pip")
	}
	n := len(daily)
	if n < p.SMAPeriod+p.SMASlopeLB || n < p.DonchianBars+1 {
		return notFound("insufficient_history")
	}

	closes := closesOf(daily)
	sma, ok1 := ta.SMA(closes, p.SMAPeriod)
	smaPrev, ok2 := ta.SMA(closes[:len(closes)-p.SMASlopeLB], p.SMAPeriod)
	if !ok1 || !ok2 {
		return notFound("sma_unavailable")
	}
	trend := maTrendDir(sma, smaPrev, p.MinSlopePips, pip)
	if trend == trendFlat {
		return notFound("no_trend")
	}

	atrPips := atrPipsOf(daily, p.ATRWindow, pip)
	if atrPips <= 0 {
		return notFound("no_atr")
	}

	// Level + consolidation height from the DonchianBars completed bars BEFORE the breakout bar.
	breakout := daily[n-1]
	win := daily[n-1-p.DonchianBars : n-1]
	donHi, donLo := win[0].High, win[0].Low
	for _, c := range win {
		if c.High > donHi {
			donHi = c.High
		}
		if c.Low < donLo {
			donLo = c.Low
		}
	}
	rangeHeight := donHi - donLo
	bodyPips := (breakout.Close - breakout.Open) / pip // signed: + for bullish close

	var prop BreakoutProposal
	switch trend {
	case trendUp:
		level := donHi
		if breakout.Close <= level { // must close beyond the level
			return notFound("no_breakout")
		}
		if bodyPips < p.DispMultATR*atrPips { // conviction close, not a wick
			return notFound("weak_breakout")
		}
		entry := breakout.Close
		inval := level - p.SLBufferATR*atrPips*pip
		target := level + rangeHeight
		prop = BreakoutProposal{
			Found: true, Label: BreakoutTrendCont, Side: order.SideBuy,
			Level: level, Entry: entry, Invalidation: inval, TargetRef: target, ATRPips: atrPips,
			StopPips: (entry - inval) / pip, RewardPips: (target - entry) / pip,
		}
	case trendDown:
		level := donLo
		if breakout.Close >= level {
			return notFound("no_breakout")
		}
		if -bodyPips < p.DispMultATR*atrPips {
			return notFound("weak_breakout")
		}
		entry := breakout.Close
		inval := level + p.SLBufferATR*atrPips*pip
		target := level - rangeHeight
		prop = BreakoutProposal{
			Found: true, Label: BreakoutTrendCont, Side: order.SideSell,
			Level: level, Entry: entry, Invalidation: inval, TargetRef: target, ATRPips: atrPips,
			StopPips: (inval - entry) / pip, RewardPips: (entry - target) / pip,
		}
	}

	if prop.StopPips <= 0 {
		return notFound("bad_geometry")
	}
	prop.RR = prop.RewardPips / prop.StopPips
	if prop.RR < p.MinRR {
		return notFound(fmt.Sprintf("rr_too_low(%.2f<%.2f)", prop.RR, p.MinRR))
	}
	prop.Reason = fmt.Sprintf("signature breakout %s level=%.5f RR=%.2f atr=%.0fpips", prop.Side, prop.Level, prop.RR, atrPips)
	return prop
}

// SignatureState is a human-facing snapshot of the signature setup per symbol — what the strategy
// is "watching" and how close it is to firing. Surfaced in runtime/advisor_v2_status.json + the API
// so the entry conditions are visible from the UI (does it fire? at what level? how far away?).
type SignatureState struct {
	Trend          string  `json:"trend"`             // "up" | "down" | "flat(no_trade)"
	SlopePips      float64 `json:"slope_pips"`        // 200SMA slope over the lookback
	CurrentPrice   float64 `json:"current_price"`     // live mid (or last close fallback)
	BuyTrigger     float64 `json:"buy_trigger"`       // daily close ABOVE this (uptrend) arms a buy
	SellTrigger    float64 `json:"sell_trigger"`      // daily close BELOW this (downtrend) arms a sell
	ATRPips        float64 `json:"atr_pips"`          //
	DistToTrigPips float64 `json:"dist_to_trig_pips"` // pips from current price to the with-trend trigger (>=0 = not yet broken)
	Reason         string  `json:"reason"`            // ok | insufficient_history | no_atr
	// NextStep is a human sentence (JP) describing what must happen NEXT for an entry to fire, so the
	// dashboard can show the firing conditions plainly. Derived from the fields above; display-only.
	NextStep string `json:"next_step"`

	// --- display thresholds (so the dashboard shows targets without hardcoding strategy constants) ---
	MinSlopePips float64 `json:"min_slope_pips"` // |slope| must reach this to light the trend
	BodyNeedPips float64 `json:"body_need_pips"` // conviction body needed at the confirming daily close (DispMultATR*ATR)
	MinRR        float64 `json:"min_rr"`         // hard minimum reward:risk at entry

	// --- intraday firing state (so the UI can show "armed, waiting within the day") ---
	BrokePips float64 `json:"broke_pips"` // pips ALREADY beyond the with-trend trigger (>0 = broken); the live breakout size
	Armed     bool    `json:"armed"`      // trend on AND price beyond the trigger = waiting for the daily close to confirm

	// Steps is the entry-condition checklist (TODO-list style) so the user sees which conditions are
	// met (done), which one is being waited on now (active), and which are still ahead (todo).
	Steps []SignatureStep `json:"steps"`
}

// SignatureStep is one row of the entry-condition checklist surfaced on the dashboard. Display-only.
type SignatureStep struct {
	Key    string `json:"key"`    // trend | trigger | confirm
	Label  string `json:"label"`  // short JP label
	Status string `json:"status"` // done | active | todo  (exactly one "active" = the current wait)
	Detail string `json:"detail"` // current value vs threshold (JP)
}

// DescribeSignatureState computes the current signature snapshot from COMPLETED daily bars (call
// strategy.CompletedDailyBars first). currentPrice is the live mid (or 0 to fall back to last close).
// Pure — for display only; the actual entry decision is DetectSignatureBreakout + BuildSignatureSignal.
func DescribeSignatureState(daily []market.Candle, currentPrice, pip float64, p SignatureParams) SignatureState {
	n := len(daily)
	if pip <= 0 || n < p.SMAPeriod+p.SMASlopeLB || n < p.DonchianBars+1 {
		return SignatureState{Reason: "insufficient_history", NextStep: "履歴待ち: 日足が不足(200SMA+傾き測定に約220本必要)"}
	}
	closes := closesOf(daily)
	sma, ok1 := ta.SMA(closes, p.SMAPeriod)
	smaPrev, ok2 := ta.SMA(closes[:len(closes)-p.SMASlopeLB], p.SMAPeriod)
	if !ok1 || !ok2 {
		return SignatureState{Reason: "insufficient_history", NextStep: "履歴待ち: 日足が不足(200SMA+傾き測定に約220本必要)"}
	}
	slopePips := (sma - smaPrev) / pip
	trend := "flat(no_trade)"
	switch maTrendDir(sma, smaPrev, p.MinSlopePips, pip) {
	case trendUp:
		trend = "up"
	case trendDown:
		trend = "down"
	}
	win := daily[n-1-p.DonchianBars : n-1]
	donHi, donLo := win[0].High, win[0].Low
	for _, c := range win {
		if c.High > donHi {
			donHi = c.High
		}
		if c.Low < donLo {
			donLo = c.Low
		}
	}
	atrPips := atrPipsOf(daily, p.ATRWindow, pip)
	price := currentPrice
	if price <= 0 {
		price = daily[n-1].Close
	}
	st := SignatureState{
		Trend: trend, SlopePips: slopePips, CurrentPrice: price,
		BuyTrigger: donHi, SellTrigger: donLo, ATRPips: atrPips, Reason: "ok",
	}
	switch trend {
	case "up":
		st.DistToTrigPips = (donHi - price) / pip // how far below the buy trigger
	case "down":
		st.DistToTrigPips = (price - donLo) / pip // how far above the sell trigger
	}
	if atrPips <= 0 {
		st.Reason = "no_atr"
		st.NextStep = "データ不足(ATR算出不可)"
		return st
	}
	bodyNeed := p.DispMultATR * atrPips // conviction-body threshold in pips
	switch trend {
	case "flat(no_trade)":
		st.NextStep = fmt.Sprintf("トレンド待ち: 200日線が横ばい(傾き%+.0fpips/20日)。±%.0fpips超でトレンド点灯", slopePips, p.MinSlopePips)
	case "up":
		if st.DistToTrigPips > 0 {
			st.NextStep = fmt.Sprintf("買い点灯まで: 日足終値が %.3f を上抜け(現在%.3f / あと+%.0fpips)。抜けたら実体≥%.0fpips・RR≥%.1fで成行買い",
				donHi, price, st.DistToTrigPips, bodyNeed, p.MinRR)
		} else {
			st.NextStep = fmt.Sprintf("買いトリガー上抜け済み(%.3f): 次の確定日足が実体≥%.0fpips・RR≥%.1fを満たせば成行買い", donHi, bodyNeed, p.MinRR)
		}
	case "down":
		if st.DistToTrigPips > 0 {
			st.NextStep = fmt.Sprintf("売り点灯まで: 日足終値が %.3f を下抜け(現在%.3f / あと-%.0fpips)。抜けたら実体≥%.0fpips・RR≥%.1fで成行売り",
				donLo, price, st.DistToTrigPips, bodyNeed, p.MinRR)
		} else {
			st.NextStep = fmt.Sprintf("売りトリガー下抜け済み(%.3f): 次の確定日足が実体≥%.0fpips・RR≥%.1fを満たせば成行売り", donLo, bodyNeed, p.MinRR)
		}
	}
	// Display thresholds + intraday firing state + the TODO-style checklist (all display-only).
	st.MinSlopePips = p.MinSlopePips
	st.MinRR = p.MinRR
	st.BodyNeedPips = bodyNeed
	if trend != "flat(no_trade)" && st.DistToTrigPips <= 0 {
		st.BrokePips = -st.DistToTrigPips // pips already beyond the trigger = the live breakout size
		st.Armed = true                   // trend on + broken = waiting within the day for the close to confirm
	}
	st.Steps = buildSignatureSteps(trend, slopePips, p.MinSlopePips, st.DistToTrigPips, st.BrokePips, st.Armed, bodyNeed, p.MinRR)
	return st
}

// buildSignatureSteps renders the entry-condition checklist (① trend → ② trigger → ③ daily-close
// confirm). Exactly one step is "active" (the current wait); satisfied steps are "done"; later ones
// "todo". Pure / display-only — mirrors the deterministic detector chain so the UI never re-derives it.
func buildSignatureSteps(trend string, slopePips, minSlope, distToTrig, brokePips float64, armed bool, bodyNeed, minRR float64) []SignatureStep {
	trendOn := trend == "up" || trend == "down"
	dir, arrow := "上昇", "上抜け"
	if trend == "down" {
		dir, arrow = "下降", "下抜け"
	}

	trendStep := SignatureStep{Key: "trend", Label: "① トレンド点灯 (200日線の傾き)"}
	if trendOn {
		trendStep.Status = "done"
		trendStep.Detail = fmt.Sprintf("%s %+.0fpips(≥%.0fで点灯)✓", dir, slopePips, minSlope)
	} else {
		trendStep.Status = "active"
		trendStep.Detail = fmt.Sprintf("横ばい %+.0fpips。±%.0fpips超で点灯", slopePips, minSlope)
	}

	trigStep := SignatureStep{Key: "trigger", Label: "② トリガー突破 (直近20日の極値)"}
	switch {
	case !trendOn:
		trigStep.Status = "todo"
		trigStep.Detail = "トレンド点灯後に判定"
	case armed:
		trigStep.Status = "done"
		trigStep.Detail = fmt.Sprintf("%s済み(+%.0fpips)✓", arrow, brokePips)
	default:
		trigStep.Status = "active"
		trigStep.Detail = fmt.Sprintf("終値が %s 待ち(あと%.0fpips)", arrow, distToTrig)
	}

	confirmStep := SignatureStep{Key: "confirm", Label: "③ 確定日足で本確認 → 成行"}
	if trendOn && armed {
		confirmStep.Status = "active" // armed: now waiting within the day for the daily close to confirm
		confirmStep.Detail = fmt.Sprintf("発火待ち: 確定日足で 実体≥%.0fpips・RR≥%.1f を確認したら成行", bodyNeed, minRR)
	} else {
		confirmStep.Status = "todo"
		confirmStep.Detail = fmt.Sprintf("実体≥%.0fpips かつ RR≥%.1f", bodyNeed, minRR)
	}

	return []SignatureStep{trendStep, trigStep, confirmStep}
}

// BuildSignatureSignal converts an ARMED breakout (the advisor v2 pass said go=true with the
// structural levels) into an ENTER Signal — the exit ROUTER.
//
// SL is ALWAYS placed at pattern invalidation (structural, not a money amount) -> broker OCO,
// so bot death never removes protection. TP is placed at the measured-move target -> broker OCO too,
// so BOTH protective legs live at the broker (the live invariant). A runner with NO broker TP (the
// old 1e6 "TP off" sentinel) is unsupported by the OCO path — GMO requires both legs, so it would
// place an absurd far TP, get rejected, and churn open→compensate-close every cycle. A wide ATR
// ratchet (arm/give) still trails ON TOP, so a strong move banks early while the broker TP/SL guard
// if the bot dies. advisor v2 is TREND-ONLY; countertrend_reversion is deferred and returns NONE for now. Pure.
func BuildSignatureSignal(side order.Side, entry, invalidation, target, atrPips, pip float64, label BreakoutLabel, qty int, name config.StrategyName, now time.Time) Signal {
	none := func(reason string) Signal {
		return Signal{Decision: DecisionNone, StrategyName: name, Reason: reason, CreatedAt: now}
	}
	if label != BreakoutTrendCont {
		return none("v2_trend_only") // countertrend deferred
	}
	if !side.Valid() || qty <= 0 || pip <= 0 || atrPips <= 0 {
		return none("not_armed")
	}
	var slPips, tpPips float64
	switch side {
	case order.SideBuy:
		slPips = (entry - invalidation) / pip // invalidation must be below entry
		tpPips = (target - entry) / pip       // measured-move target must be above entry
	case order.SideSell:
		slPips = (invalidation - entry) / pip // invalidation must be above entry
		tpPips = (entry - target) / pip       // measured-move target must be below entry
	}
	if slPips <= 0 {
		return none("bad_invalidation")
	}
	if tpPips <= 0 {
		// Live entry has chased to/beyond the measured-move target — no reward room left.
		// The cycle's chase guard normally catches this first; this is the structural backstop.
		return none("past_target")
	}
	return Signal{
		Decision:            DecisionEnter,
		Side:                side,
		EntryPrice:          entry,
		TakeProfitPips:      tpPips, // measured-move target, broker-side OCO
		StopLossPips:        slPips, // structural, at pattern invalidation
		RatchetArmPips:      tfRatchetArmMult * atrPips,
		RatchetGivebackPips: tfRatchetGiveMult * atrPips,
		MaxHoldMinutes:      tfMaxHoldMin,
		Quantity:            qty,
		Reason:              fmt.Sprintf("signature %s SL=%.0fpips(struct) TP=%.0fpips(target) ratchet arm=%.0f give=%.0f", side, slPips, tpPips, tfRatchetArmMult*atrPips, tfRatchetGiveMult*atrPips),
		StrategyName:        name,
		CreatedAt:           now,
	}
}
