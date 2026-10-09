package command

import (
	"context"
	"fmt"
	"log/slog"

	"fx-bot/backend/internal/domain/order"
	"fx-bot/backend/internal/domain/position"
	"fx-bot/backend/internal/port"
	"fx-bot/backend/internal/safety"
)

// LiveExitProtector は Live エントリの MARKET 約定後に走る共通フェーズ:
//
//  1. ResolveExecution → 実 positionId + 実 fill price
//  2. ComputeTPSLPrices(side, fillPx, tpPips, slPips) → TP/SL 絶対価格
//  3. PlaceSettleOCO   → broker 側 OCO 親注文 (TP limit + SL stop)
//  4. ResolveSettleLegs → tp/sl leg orderIds (soft fail: empty 返却で続行)
//
// を 1 箇所に集約する。manual_trade.go と execute_order.go が共有する。
//
// エラー分類:
//   - critical: resolver/oco_placer 未実装 / ResolveExecution 失敗 /
//     OCO 失敗 + compensating close も失敗 / 等。emergency_stop を発火。
//   - compensated: PlaceSettleOCO 失敗 → 即 ClosePosition で巻き戻し成功。
//     emergency_stop は焚かず、呼出側に rolled-back エラーを返す。
//   - soft: ResolveSettleLegs 失敗。OCO は GMO 側に存在するので leg id を
//     喪失するだけ。WARN ログのみで空 id を返却し続行。
//
// 呼び出し側 (manual_trade / execute_order) の reason ラベル
// ("manual_no_resolver" / "execute_no_resolver" など) は Source フィールド
// (= "manual" / "execute") を prefix にすることで互換維持。
type LiveExitProtector struct {
	Broker            port.Broker
	Symbol            string
	PipSize           float64
	Source            string // "manual" / "execute" — reason prefix
	EmergencyFlagPath string
	Logger            *slog.Logger
	// PendingTracker は entry saga 進行中 (broker fill 済みだが DB INSERT 未完) の
	// broker_position_id を一時保存し、reconcile race-window で誤って外部 adopt
	// されないようにする。
	//
	// Attach は ResolveExecution で positionId を取った直後に MarkPending、
	// 補償 close 経路 (rolled-back) は positionId が broker 側で既に close 済みなので
	// Attach 内で即 MarkResolved。Happy path / Critical path で broker 側にポジが残る
	// 場合は **呼出側 (manual_trade / execute_order)** が DB INSERT 完了 / 失敗時に
	// 必ず MarkResolved する責任を負う (`defer tracker.MarkResolved(result.BrokerPositionID)`)。
	//
	// nil 許容 (paper mode / 単体テスト)。
	PendingTracker port.PendingPositionTracker
}

// LiveExitProtectionInput は Attach の引数。
type LiveExitProtectionInput struct {
	Order          *order.Order // PlaceOrder の戻り値
	Side           order.Side
	Quantity       int
	TakeProfitPips float64
	StopLossPips   float64
}

// LiveExitProtectionResult は Attach の戻り値。
type LiveExitProtectionResult struct {
	EntryPrice       float64
	BrokerPositionID string
	TPOrderID        string // soft 失敗時は ""
	SLOrderID        string // soft 失敗時は ""
	// EntryFeeJPY は entry fill の broker 実報告手数料。
	// positions.entry_fee_jpy として凍結保存し、close 時に trades.fee_jpy
	// (往復) へ合成する。0 は「broker が 0 と報告」(手数料無料期間) の実値。
	EntryFeeJPY float64
}

// Attach は MARKET 約定済みエントリの protection を完成させる。
// 呼出側は PlaceOrder の成功 (ord != nil, ord.OrderID != "") を前提に呼ぶ。
func (p *LiveExitProtector) Attach(ctx context.Context, in LiveExitProtectionInput) (LiveExitProtectionResult, error) {
	ord := in.Order
	orderID := ord.OrderID

	// 1. ResolveExecution
	resolver, ok := p.Broker.(port.ExecutionResolver)
	if !ok {
		return LiveExitProtectionResult{}, p.critical("no_resolver", orderID,
			fmt.Errorf("live_config requires ExecutionResolver broker; got %T", p.Broker))
	}
	rctx, cancel := context.WithTimeout(ctx, safety.ResolveExecutionTimeout)
	res, rerr := resolver.ResolveExecution(rctx, orderID)
	cancel()
	if rerr != nil {
		return LiveExitProtectionResult{}, p.critical("resolve_execution_failed", orderID, rerr)
	}
	posID, fillPx := res.PositionID, res.Price
	// 以降の OCO 配置 / leg resolve / 呼出側の DB INSERT
	// 完了まで、reconcile が当該 positionId を「裸 broker position」と誤検出
	// しないよう pending tracker に登録する。caller が DB INSERT 終端で
	// MarkResolved する責任を負う。Attach 内で broker 側にポジが残らない
	// (= 補償 close 成功) 経路は Attach 自身が即 MarkResolved する。
	p.markPending(posID)

	// 2. ComputeTPSLPrices from actual fill (not estimate)
	tpPrice, slPrice := position.ComputeTPSLPrices(in.Side, fillPx, in.TakeProfitPips, in.StopLossPips, p.PipSize)

	// 3. PlaceSettleOCO
	ocoPlacer, ok := p.Broker.(port.OCOCloseOrderPlacer)
	if !ok {
		p.markResolved(posID) // critical 後、tracker leak 防止
		return LiveExitProtectionResult{}, p.critical("no_oco_placer", orderID,
			fmt.Errorf("live_config requires OCOCloseOrderPlacer broker; got %T", p.Broker))
	}
	brokerPosIDNum, perr := parseBrokerPositionID(posID)
	if perr != nil {
		p.markResolved(posID)
		return LiveExitProtectionResult{}, p.critical("broker_position_id_not_numeric", orderID, perr)
	}
	octx, ocancel := context.WithTimeout(ctx, safety.ResolveExecutionTimeout)
	_, oerr := ocoPlacer.PlaceSettleOCO(octx, port.OCOCloseOrderInput{
		Symbol:           p.Symbol,
		BrokerPositionID: brokerPosIDNum,
		Side:             in.Side.Opposite(),
		Size:             in.Quantity,
		TPPrice:          tpPrice,
		SLPrice:          slPrice,
	})
	ocancel()
	if oerr != nil {
		// Compensating close — invariant "Live position always has GMO-side OCO"
		// を維持するため、TP/SL なしの裸ポジを market-close で巻き戻す。
		return LiveExitProtectionResult{}, p.compensate(ctx, posID, in.Side, in.Quantity,
			"place_settle_oco_failed", oerr, orderID)
	}

	// 4. ResolveSettleLegs (soft failure OK)
	tpOrderID, slOrderID := "", ""
	legResolver, ok := p.Broker.(port.SettleLegResolver)
	if !ok {
		// Hard failure: leg resolver interface 必須。OCO は GMO 側にあるが
		// close saga が leg id で cancel できないので emergency_stop。
		p.markResolved(posID)
		return LiveExitProtectionResult{}, p.critical("no_settle_leg_resolver", orderID,
			fmt.Errorf("live_config requires SettleLegResolver broker; got %T", p.Broker))
	}
	lctx, lcancel := context.WithTimeout(ctx, safety.ResolveExecutionTimeout)
	tpID, slID, lerr := legResolver.ResolveSettleLegs(lctx, posID, p.Symbol)
	lcancel()
	if lerr != nil {
		// Soft failure: OCO は GMO 側で生きているので、ここで止めると
		// 裸ポジが残る方が悪い。leg id だけ捨てて続行 (close saga は
		// /v1/activeOrders による leg discovery で cancel する — live_close_helpers.go)。
		p.Logger.Warn(p.Source+"_resolve_settle_legs_soft_failure",
			"order_id", orderID, "broker_position_id", posID,
			"err", lerr,
			"note", "OCO is placed at GMO; leg id lookup failed — position recorded without leg ids")
	} else {
		tpOrderID = tpID
		slOrderID = slID
	}

	return LiveExitProtectionResult{
		EntryPrice:       fillPx,
		BrokerPositionID: posID,
		TPOrderID:        tpOrderID,
		SLOrderID:        slOrderID,
		EntryFeeJPY:      res.FeeJPY,
	}, nil
}

// critical は emergency_stop を発火 + 上位に Critical error を返す。
// reason は Source プレフィックス付き (例: "manual_resolve_execution_failed")。
func (p *LiveExitProtector) critical(short, orderID string, cause error) error {
	reason := p.Source + "_" + short
	p.Logger.Error("live_exit_protector_critical",
		"source", p.Source, "reason", reason, "order_id", orderID, "cause", cause)
	if werr := safety.TripWithDetail(p.EmergencyFlagPath, reason,
		fmt.Sprintf("order=%s cause=%v", orderID, cause)); werr != nil {
		p.Logger.Error("emergency_stop_write_failed", "err", werr)
	}
	return fmt.Errorf("CRITICAL live failure (%s) for order %s: %w", reason, orderID, cause)
}

// markPending / markResolved は PendingTracker 連携。tracker が nil でも
// 安全に no-op で動くよう薄くラップする。
func (p *LiveExitProtector) markPending(brokerPositionID string) {
	if p.PendingTracker != nil {
		p.PendingTracker.MarkPending(brokerPositionID)
	}
}
func (p *LiveExitProtector) markResolved(brokerPositionID string) {
	if p.PendingTracker != nil {
		p.PendingTracker.MarkResolved(brokerPositionID)
	}
}

// compensate は OCO 失敗時の rollback。market-close 成功 → rolled-back error
// (emergency_stop なし)、失敗 → critical でエスカレ。
func (p *LiveExitProtector) compensate(
	ctx context.Context,
	brokerPosID string, openSide order.Side, qty int,
	short string, cause error, orderID string,
) error {
	closeReq := position.Position{
		BrokerPositionID: brokerPosID,
		Symbol:           p.Symbol,
		Side:             openSide, // Broker.ClosePosition は内部で flip する
		Quantity:         qty,
	}
	cctx, ccancel := context.WithTimeout(ctx, safety.ResolveExecutionTimeout)
	_, closeErr := p.Broker.ClosePosition(cctx, closeReq)
	ccancel()
	if closeErr != nil {
		// 補償 close 失敗時は broker 側にポジが残る可能性。critical で emergency_stop
		// を焚いて bot を止めるが、tracker は手放してはいけない (= 残し続けると次回
		// reconcile が永久に skip する)。critical 経路でも明示 MarkResolved する。
		p.markResolved(brokerPosID)
		return p.critical(short+"_and_compensate_close_failed", orderID,
			fmt.Errorf("setup: %w; compensating close also failed: %v", cause, closeErr))
	}
	// 補償 close 成功 → broker 側にポジは無い。tracker からも除外しないと、
	// 次回 reconcile が同 id を見たときに誤って skip する (= 永久 leak)。
	p.markResolved(brokerPosID)
	p.Logger.Warn("live_exit_protector_compensated_close_ok",
		"source", p.Source, "reason", short, "order_id", orderID,
		"broker_position_id", brokerPosID, "side", openSide, "qty", qty, "cause", cause)
	return fmt.Errorf("%s entry rolled back via compensating close (%s) for order %s: %w",
		p.Source, short, orderID, cause)
}
