package strategy

import (
	"fmt"
	"math"

	"fx-bot/backend/internal/config"
	"fx-bot/backend/internal/domain/market"
	"fx-bot/backend/internal/domain/order"
	"fx-bot/backend/internal/domain/ta"
)

// === exhaustion_fade — deterministic "fade the overshoot" detector ===
//
// The counter-trend half of the playbook ("オーバーシュートの逆張り" +
// the common professional mean-reversion practice). It fades a SHARP run that has STALLED at
// a meaningful level — NOT a range-bound mean reversion, and NOT a trade taken
// while price is still running (the cardinal "飛びつき逆張り" mistake).
//
// The 4 conditions that separate a fadeable exhausted spike from a runaway
// breakout (all must hold):
//   1. stretch     — price is MinDeviationATR..MaxDeviationATR from the reference
//                    MA (a real overshoot, but not a 3σ+ chase) AND the run over
//                    RunLookback bars is >= MinRunPips.
//   2. level       — the spike extreme sits within LevelTolerancePips of a round
//                    number or a PRIOR swing (where stop orders cluster = the
//                    cascade fuel that exhausts).
//   3. exhaustion  — price has STOPPED making new highs/lows (the push stalled)
//                    AND the latest bar shows a rejection wick (>= MinWickToBody).
//   4. not-a-trend — the Kaufman efficiency ratio over ERLookback is below
//                    MaxEfficiencyRatio (a strong clean trend would run the fade
//                    over). Skipped when ER is undefined (too little history).
//
// Pure & side-effect free. Constants are FIXED defaults to be CALIBRATED by
// backtest, never swept live (sweeping over live data is overfitting).
// The detector emits only geometry + an R:R; the risk Gate and
// broker OCO decide whether to act.

// htfEfficiencyLookback is the fixed 1h-bar window for the HTF trend-regime
// kill-switch (≈1 trading day). The threshold is config-driven
// (entry.htf_efficiency_max); the window is not swept.
const htfEfficiencyLookback = 24

// htfSlopeLookback is the fixed 1h-bar window for the MTF directional veto
// (≈1 trading day); the threshold (pips) is config-driven, the window is not.
const htfSlopeLookback = 24

// defaultRSIPeriod is used for the divergence check when the config enables it
// without specifying a period.
const defaultRSIPeriod = 14

// ExhaustionFadeParams holds the FIXED knobs. MAPeriod/ATRWindow are consumed by
// the ExhaustionFade Strategy wrapper (to compute the reference MA/ATR off the
// higher timeframe); the pure DetectExhaustionFade takes the resulting refMA /
// refATRPips as scalars.
type ExhaustionFadeParams struct {
	MAPeriod  int // reference-MA period on the higher TF (e.g. 1h) — stretch anchor
	ATRWindow int // reference-ATR window on the higher TF

	MinDeviationATR float64 // overshoot: |price-MA| in ATR units must reach this
	MaxDeviationATR float64 // ...but not exceed this (3σ+ = chase, gravity gone)
	MinRunPips      float64 // net move over RunLookback must reach this (the "走り")
	RunLookback     int     // bars (execution TF) over which the run / spike extreme is measured

	LevelTolerancePips float64 // spike extreme must be within this of a level
	RoundStep          float64 // round-number grid (0.50 for JPY = figures + half-figures)
	SwingN             int     // fractal half-width for PRIOR swing detection
	SwingMinPromPips   float64 // swing prominence noise gate

	NewHighLookback int     // exhaustion: last bar must NOT exceed prior-N extreme
	MinWickToBody   float64 // exhaustion: rejection wick >= this × body on the last bar

	ERLookback         int     // Kaufman efficiency ratio window (trend kill-switch)
	MaxEfficiencyRatio float64 // ER >= this = too trendy to fade (skip)

	SLBufferATR    float64 // stop = spike extreme ± this × refATR (beyond the wick)
	TPRevertFrac   float64 // take-profit = this fraction of the run given back
	MinTPPips      float64 // reject setups whose TP is below this (cost-floor guard)
	MaxHoldMinutes int     // time-stop: bail if the revert hasn't happened (trend-day)

	// --- refinements (all zero-value = OFF; calibrated by backtest, wired
	// from config so existing configs are unaffected until a value is proven).

	// MinRunEfficiencyRatio is the band-pass FLOOR: the run window itself must
	// be a clean directional push (Kaufman ER over the run >= this), i.e. fade
	// a sharp spike, not a choppy drift to the level. Pairs with the existing
	// MaxEfficiencyRatio ceiling over the longer ERLookback (not a sustained
	// trend) to form a band-pass: sharp recent spike inside a non-trending
	// context. 0 = off.
	MinRunEfficiencyRatio float64

	// RequireDivergence demands RSI regular divergence at the spike (the
	// oscillator refuses to confirm the new extreme = momentum exhausting) as a
	// second confirmation alongside the rejection wick. false = off.
	RequireDivergence bool
	RSIPeriod         int // RSI period for the divergence check (>0 required when RequireDivergence)

	// RoundTP caps the take-profit JUST BEFORE the next round level in the
	// profit direction (price stalls where limit orders cluster) — TP =
	// min(revert give-back, distance to that round − offset), still floored by
	// MinTPPips. false = off (pure revert give-back as before).
	RoundTP           bool
	RoundTPStep       float64 // round grid for the TP (e.g. 0.10 = 10-pip minor levels); falls back to RoundStep when 0
	RoundTPOffsetPips float64 // take profit this many pips before the round level

	// HTF directional veto: block a fade that OPPOSES a clean 1h trend (its net
	// move over HTFSlopeLookback 1h bars reaches HTFMinSlopePips) while ALLOWING
	// a trend-aligned fade — unlike the direction-agnostic HTFEfficiencyMax
	// gate. Applied by the Strategy wrapper (needs 1h closes). 0 pips = off.
	HTFSlopeLookback int
	HTFMinSlopePips  float64
}

// DefaultExhaustionFadeParams = the research-derived starting point for USD/JPY
// on a 1-minute execution TF with a 1h reference. NOT yet calibrated — these are
// the values to drive into backtest and then tune.
func DefaultExhaustionFadeParams() ExhaustionFadeParams {
	return ExhaustionFadeParams{
		MAPeriod: 20, ATRWindow: 14,
		MinDeviationATR: 1.8, MaxDeviationATR: 3.0,
		MinRunPips: 12, RunLookback: 15,
		LevelTolerancePips: 5, RoundStep: 0.50, SwingN: 3, SwingMinPromPips: 2,
		NewHighLookback: 3, MinWickToBody: 2.0,
		ERLookback: 60, MaxEfficiencyRatio: 0.65,
		SLBufferATR: 0.5, TPRevertFrac: 0.5, MinTPPips: 4,
		MaxHoldMinutes: 60,
	}
}

// FadeProposal is the deterministic candidate (geometry only — NOT an order).
type FadeProposal struct {
	Found        bool
	Side         order.Side
	Entry        float64 // reference entry = last close
	SpikeExtreme float64 // the high (SELL) / low (BUY) being faded
	Stop         float64 // structural stop price, beyond the spike extreme
	Target       float64 // take-profit price (partial give-back of the run)
	ATRPips      float64 // reference ATR used for sizing
	DeviationATR float64 // how far price sits from the MA, in ATR units (signed)
	StopPips     float64
	RewardPips   float64
	RR           float64
	Reason       string
}

func fadeNotFound(reason string) FadeProposal { return FadeProposal{Found: false, Reason: reason} }

// DetectExhaustionFade evaluates the 4-condition fade on the execution-TF candle
// stream c1m (oldest first; last element = current bar). refMA / refATRPips are
// the stretch anchor computed off the higher timeframe (see the Strategy
// wrapper). pip is the symbol pip size. Returns a SELL proposal on an exhausted
// up-spike, a BUY on an exhausted down-spike, or not-found.
func DetectExhaustionFade(c1m []market.Candle, refMA, refATRPips, pip float64, p ExhaustionFadeParams) FadeProposal {
	if pip <= 0 {
		return fadeNotFound("no_pip")
	}
	if refATRPips <= 0 {
		return fadeNotFound("no_ref_atr")
	}
	if p.RunLookback < 1 || len(c1m) < p.RunLookback+1 {
		return fadeNotFound("insufficient_history")
	}

	price := c1m[len(c1m)-1].Close

	// 1) stretch — overshoot from the reference mean, in ATR units.
	dev, ok := ta.DeviationATR(price, refMA, refATRPips, pip)
	if !ok {
		return fadeNotFound("no_deviation")
	}
	run, ok := ta.NetMovePips(c1m, p.RunLookback, pip)
	if !ok {
		return fadeNotFound("no_run")
	}
	// Direction: an up-overshoot (dev>0, run>0) is faded SHORT; a down-overshoot SELL→BUY.
	var side order.Side
	switch {
	case dev > 0 && run > 0:
		side = order.SideSell
	case dev < 0 && run < 0:
		side = order.SideBuy
	default:
		return fadeNotFound("no_overshoot_direction")
	}
	absDev := math.Abs(dev)
	if absDev < p.MinDeviationATR {
		return fadeNotFound(fmt.Sprintf("not_stretched(%.2f<%.2f)", absDev, p.MinDeviationATR))
	}
	if absDev > p.MaxDeviationATR {
		return fadeNotFound(fmt.Sprintf("too_stretched_chase(%.2f>%.2f)", absDev, p.MaxDeviationATR))
	}
	if math.Abs(run) < p.MinRunPips {
		return fadeNotFound(fmt.Sprintf("run_too_small(%.1f<%.1f)", math.Abs(run), p.MinRunPips))
	}

	// spike extreme over the run window.
	runWin := c1m[len(c1m)-p.RunLookback:]
	spikeHi, spikeLo := runWin[0].High, runWin[0].Low
	for _, c := range runWin {
		if c.High > spikeHi {
			spikeHi = c.High
		}
		if c.Low < spikeLo {
			spikeLo = c.Low
		}
	}
	spikeExtreme := spikeHi
	if side == order.SideBuy {
		spikeExtreme = spikeLo
	}

	// 1b) band-pass FLOOR — fade a sharp spike, not a choppy drift to the level.
	// The run window itself must be a clean directional push (Kaufman ER over
	// the run >= MinRunEfficiencyRatio). Together with the MaxEfficiencyRatio
	// ceiling over the longer ERLookback this is a band-pass: sharp recent spike
	// inside a non-trending context. Off when the floor is 0 or ER undefined.
	if p.MinRunEfficiencyRatio > 0 {
		if er, ok := ta.EfficiencyRatio(closesOf(runWin), p.RunLookback-1); ok && er < p.MinRunEfficiencyRatio {
			return fadeNotFound(fmt.Sprintf("run_too_choppy(%.2f<%.2f)", er, p.MinRunEfficiencyRatio))
		}
	}

	// 2) level — the spike extreme leans on a round number or a PRIOR swing.
	if !nearLevel(c1m, spikeExtreme, side, pip, p) {
		return fadeNotFound("no_level")
	}

	// 3) exhaustion — new-extreme push has stalled AND the last bar rejects.
	last := c1m[len(c1m)-1]
	shape, ok := ta.ShapeOf(last, pip)
	if !ok {
		return fadeNotFound("no_shape")
	}
	switch side {
	case order.SideSell:
		if ta.MakesNewHigh(c1m, p.NewHighLookback) {
			return fadeNotFound("still_making_new_highs")
		}
		if shape.UpperWickToBody < p.MinWickToBody {
			return fadeNotFound(fmt.Sprintf("no_rejection_wick(%.1f<%.1f)", shape.UpperWickToBody, p.MinWickToBody))
		}
	case order.SideBuy:
		if ta.MakesNewLow(c1m, p.NewHighLookback) {
			return fadeNotFound("still_making_new_lows")
		}
		if shape.LowerWickToBody < p.MinWickToBody {
			return fadeNotFound(fmt.Sprintf("no_rejection_wick(%.1f<%.1f)", shape.LowerWickToBody, p.MinWickToBody))
		}
	}

	// 3b) divergence (refinement) — demand the oscillator REFUSE to confirm the
	// new extreme (price higher high / lower low, RSI the opposite) as a second
	// exhaustion confirmation alongside the rejection wick. Needs two swings to
	// compare; undefined → blocked (conservative). Off when RequireDivergence
	// is false.
	if p.RequireDivergence {
		if !ta.RegularDivergence(c1m, side, p.RSIPeriod, p.SwingN, pip, p.SwingMinPromPips) {
			return fadeNotFound("no_divergence")
		}
	}

	// 4) not-a-trend — skip when the broader move is a clean efficient trend
	// (it would run the fade over). Only applied when ER is computable.
	if er, ok := ta.EfficiencyRatio(closesOf(c1m), p.ERLookback); ok && er >= p.MaxEfficiencyRatio {
		return fadeNotFound(fmt.Sprintf("efficiency_too_high_trend(%.2f>=%.2f)", er, p.MaxEfficiencyRatio))
	}

	// geometry — stop beyond the spike extreme, TP = partial give-back of the run.
	buffer := p.SLBufferATR * refATRPips * pip
	tpPips := p.TPRevertFrac * math.Abs(run)
	// round-number-aware TP (refinement) — when the give-back would carry past a
	// round level in the profit direction, take profit just BEFORE it (orders
	// cluster there, price stalls). Only TIGHTENS (min); never extends greed.
	if p.RoundTP {
		step := p.RoundTPStep
		if step <= 0 {
			step = p.RoundStep
		}
		if rtp, ok := roundTPPips(price, side, step, p.RoundTPOffsetPips, p.MinTPPips, pip); ok && rtp < tpPips {
			tpPips = rtp
		}
	}
	if tpPips < p.MinTPPips {
		return fadeNotFound(fmt.Sprintf("tp_below_floor(%.1f<%.1f)", tpPips, p.MinTPPips))
	}
	prop := FadeProposal{
		Found: true, Side: side, Entry: price, SpikeExtreme: spikeExtreme,
		ATRPips: refATRPips, DeviationATR: dev, RewardPips: tpPips,
	}
	switch side {
	case order.SideSell:
		prop.Stop = spikeExtreme + buffer
		prop.Target = price - tpPips*pip
		prop.StopPips = (prop.Stop - price) / pip
	case order.SideBuy:
		prop.Stop = spikeExtreme - buffer
		prop.Target = price + tpPips*pip
		prop.StopPips = (price - prop.Stop) / pip
	}
	if prop.StopPips <= 0 {
		return fadeNotFound("bad_geometry")
	}
	prop.RR = prop.RewardPips / prop.StopPips
	prop.Reason = fmt.Sprintf("exhaustion fade %s dev=%.2fATR run=%.0fpips level=%.3f SL=%.0f TP=%.0f RR=%.2f",
		side, dev, run, spikeExtreme, prop.StopPips, prop.RewardPips, prop.RR)
	return prop
}

// nearLevel reports whether the spike extreme sits within LevelTolerancePips of
// a round number OR a swing that formed BEFORE the run (a pre-existing level the
// spike ran into — not the spike's own pivot). pip > 0 is guaranteed by caller.
func nearLevel(c1m []market.Candle, extreme float64, side order.Side, pip float64, p ExhaustionFadeParams) bool {
	if d, _, ok := ta.RoundNumberDistancePips(extreme, p.RoundStep, pip); ok && d <= p.LevelTolerancePips {
		return true
	}
	// swings detected on the history BEFORE the run window.
	preRun := c1m[:len(c1m)-p.RunLookback]
	switch side {
	case order.SideSell:
		if sh, ok := ta.LastSwingHigh(preRun, p.SwingN, pip, p.SwingMinPromPips); ok {
			if math.Abs(extreme-sh)/pip <= p.LevelTolerancePips {
				return true
			}
		}
	case order.SideBuy:
		if sl, ok := ta.LastSwingLow(preRun, p.SwingN, pip, p.SwingMinPromPips); ok {
			if math.Abs(extreme-sl)/pip <= p.LevelTolerancePips {
				return true
			}
		}
	}
	return false
}

// roundTPPips returns the take-profit distance in pips that exits just before
// the nearest round level (on `step`) in the profit direction — for a SELL the
// round BELOW entry (exit offsetPips above it), for a BUY the round ABOVE (exit
// offsetPips below it). It walks to the next round out until the distance clears
// minTPPips (a round closer than the cost-floor is useless). ok=false on a bad
// grid; the 10k guard only trips on absurd inputs.
func roundTPPips(entry float64, side order.Side, step, offsetPips, minTPPips, pip float64) (float64, bool) {
	if step <= 0 || pip <= 0 {
		return 0, false
	}
	switch side {
	case order.SideSell: // profit is DOWN → round below, exit just above it
		r := math.Floor(entry/step) * step
		if r >= entry {
			r -= step
		}
		for i := 0; i < 10000 && r > 0; i++ {
			if tp := (entry - (r + offsetPips*pip)) / pip; tp >= minTPPips {
				return tp, true
			}
			r -= step
		}
	case order.SideBuy: // profit is UP → round above, exit just below it
		r := math.Ceil(entry/step) * step
		if r <= entry {
			r += step
		}
		for i := 0; i < 10000; i++ {
			if tp := (r - offsetPips*pip - entry) / pip; tp >= minTPPips {
				return tp, true
			}
			r += step
		}
	}
	return 0, false
}

// HTFTrendVetoesSide is the MTF DIRECTIONAL veto, shared by the deterministic
// exhaustion_fade and the autonomous LLM loop: it reports whether
// `side` FIGHTS a clean 1h trend (net move over `lookback` 1h bars reaches
// minSlopePips) while ALLOWING a trend-ALIGNED side — unlike the direction-agnostic
// HTFEfficiencyMax kill-switch. A SELL is vetoed by a strong uptrend; a BUY by a
// strong downtrend. Returns false (no veto) when the veto is off, the slope is
// below the threshold, or history is too short. Pure: no I/O, fully unit-testable.
func HTFTrendVetoesSide(closes1h []float64, lookback int, minSlopePips, pip float64, side order.Side) bool {
	if minSlopePips <= 0 || lookback < 1 || pip <= 0 || len(closes1h) < lookback+1 {
		return false
	}
	last := len(closes1h) - 1
	slopePips := (closes1h[last] - closes1h[last-lookback]) / pip
	switch side {
	case order.SideSell:
		return slopePips >= minSlopePips // fading SHORT into a strong uptrend
	case order.SideBuy:
		return slopePips <= -minSlopePips // fading LONG into a strong downtrend
	}
	return false
}

// htfTrendBlocksFade is the fade-specific call site kept for exhaustion_fade; it
// delegates to the shared HTFTrendVetoesSide so the two paths stay in lockstep.
func htfTrendBlocksFade(closes1h []float64, lookback int, minSlopePips, pip float64, side order.Side) bool {
	return HTFTrendVetoesSide(closes1h, lookback, minSlopePips, pip, side)
}

// ExhaustionFade is the Strategy wrapper: it computes the higher-timeframe
// reference MA/ATR (1h) from in.Candles1h, runs DetectExhaustionFade on the
// execution-TF stream (in.Candles1m), and converts a found proposal into an
// ENTER Signal with structural exits (broker-OCO SL/TP + a time-stop). NOTE:
// this detector is designed and calibrated for backtest; it has no live-specific
// wiring (faster cadence, LLM context gate). Exit is strategy-computed; no ratchet (a fade takes a
// fixed give-back, it does not let-run).
type ExhaustionFade struct {
	Params ExhaustionFadeParams
}

func (ExhaustionFade) Name() config.StrategyName { return config.StrategyExhaustionFade }

func (s ExhaustionFade) Evaluate(in EvalInput) Signal {
	name := config.StrategyExhaustionFade
	none := func(reason string) Signal {
		return Signal{Decision: DecisionNone, StrategyName: name, Reason: reason, CreatedAt: in.Now}
	}
	if in.Config == nil || in.Summary == nil {
		return Signal{Decision: DecisionNone, StrategyName: name, CreatedAt: in.Now}
	}
	dir := in.Config.Entry.Direction
	if dir == config.DirectionNone {
		return Signal{Decision: DecisionNoTrade, StrategyName: name, Reason: "direction=none", CreatedAt: in.Now}
	}
	if in.Summary.CurrentRate.SpreadPips > in.Config.Entry.MaxSpreadPips {
		return none("spread_too_wide")
	}
	pip := market.PipSize(in.Config.Symbol)
	if pip <= 0 {
		return none("no_pip")
	}
	p := s.Params
	if p.MAPeriod <= 0 { // zero-value Params (e.g. registry default) → use research defaults
		p = DefaultExhaustionFadeParams()
	}

	// Config-driven refinements (per-config so existing configs are unaffected
	// until a value is proven; all zero/false = OFF). Detector knobs are pushed
	// into p here; the MTF directional veto is applied below (it needs the 1h
	// closes). See EntrySection/ExitSection in config.
	if v := in.Config.Entry.MinRunEfficiency; v > 0 {
		p.MinRunEfficiencyRatio = v
	}
	if in.Config.Entry.RequireRSIDivergence {
		p.RequireDivergence = true
		if p.RSIPeriod <= 0 {
			p.RSIPeriod = defaultRSIPeriod
		}
	}
	if in.Config.Exit.RoundNumberTP {
		p.RoundTP = true
		if v := in.Config.Exit.RoundNumberTPStepPips; v > 0 {
			p.RoundTPStep = v * pip
		}
		p.RoundTPOffsetPips = in.Config.Exit.RoundNumberTPOffsetPips
	}
	if v := in.Config.Exit.MinTakeProfitPips; v > 0 {
		p.MinTPPips = v
	}
	if v := in.Config.Entry.HTFTrendVetoPips; v > 0 {
		p.HTFMinSlopePips = v
	}

	// Reference MA/ATR off the 1h series (the stretch anchor).
	if len(in.Candles1h) < p.MAPeriod {
		return none("insufficient_1h_history")
	}
	refMA, ok := ta.SMA(closesOf(in.Candles1h), p.MAPeriod)
	if !ok {
		return none("ma_unavailable")
	}
	refATRPips := market.ATRPips(lastN(in.Candles1h, p.ATRWindow), pip)
	if refATRPips <= 0 {
		return none("no_ref_atr")
	}

	// HTF trend-regime kill-switch (refinement): don't fade when the 1h is a
	// clean directional trend (it would run the fade over). Opt-in via config
	// (htf_efficiency_max); fixed 24-bar 1h window. Undefined ER (flat/short
	// history) → gate cannot apply, fade proceeds.
	if thr := in.Config.Entry.HTFEfficiencyMax; thr > 0 {
		if er, ok := ta.EfficiencyRatio(closesOf(in.Candles1h), htfEfficiencyLookback); ok && er >= thr {
			return none("htf_trending")
		}
	}

	prop := DetectExhaustionFade(in.Candles1m, refMA, refATRPips, pip, p)
	if !prop.Found {
		return none(prop.Reason)
	}
	if !directionAllows(dir, prop.Side) {
		return none("direction_blocked")
	}

	// MTF directional veto (refinement) — block a fade that fights a strong 1h
	// trend; a trend-aligned fade passes (config htf_trend_veto_pips). Applied
	// here at the wrapper since it needs the 1h closes.
	if p.HTFMinSlopePips > 0 {
		lb := p.HTFSlopeLookback
		if lb <= 0 {
			lb = htfSlopeLookback
		}
		if htfTrendBlocksFade(closesOf(in.Candles1h), lb, p.HTFMinSlopePips, pip, prop.Side) {
			return none("htf_trend_veto")
		}
	}

	entry := in.Summary.CurrentRate.Ask
	if prop.Side == order.SideSell {
		entry = in.Summary.CurrentRate.Bid
	}
	// SL cap (refinement): the structural stop sits beyond the spike extreme,
	// which on a big overshoot can be 10-20 pips — the wide stop behind the
	// reversed-RR losses. When config.exit.stop_loss_pips > 0 it acts as a MAX
	// cap ("cut fast"): the stop is the tighter of structural vs the cap.
	// Backtest configs set it high (=no cap) for the baseline and small (e.g. 6)
	// to test the tightened variant. 0 = no cap.
	slPips := prop.StopPips
	if c := in.Config.Exit.StopLossPips; c > 0 && c < slPips {
		slPips = c
	}
	return Signal{
		Decision:       DecisionEnter,
		Side:           prop.Side,
		EntryPrice:     entry,
		TakeProfitPips: prop.RewardPips, // partial give-back → broker OCO
		StopLossPips:   slPips,          // structural (beyond spike) or capped → broker OCO
		MaxHoldMinutes: p.MaxHoldMinutes,
		Quantity:       in.Config.Risk.Quantity,
		Reason:         prop.Reason,
		ConfigID:       in.Config.ConfigID,
		StrategyName:   name,
		CreatedAt:      in.Now,
	}
}
