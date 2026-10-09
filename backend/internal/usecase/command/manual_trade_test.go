package command

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"

	"fx-bot/backend/internal/backtest"
	"fx-bot/backend/internal/config"
	"fx-bot/backend/internal/domain/market"
	"fx-bot/backend/internal/domain/order"
	"fx-bot/backend/internal/safety"
)

func makeManualCmd(t *testing.T, br any, mode config.Mode, flag string) *ManualTradeCommand {
	t.Helper()
	pos := backtest.NewInMemoryPositionRepo()
	// br は fakeBroker / fakeLiveBroker のどちらも受け取れるよう any
	var broker interface {
		GetTicker(ctx context.Context, sym string) (*market.Ticker, error)
		PlaceOrder(ctx context.Context, req order.PlaceOrderRequest) (*order.Order, error)
	}
	switch b := br.(type) {
	case *fakeBroker:
		broker = b
	case *fakeLiveBroker:
		broker = b
	default:
		t.Fatalf("unsupported broker type %T", br)
	}
	_ = broker // unused: we cast via switch below
	cmd := &ManualTradeCommand{
		Mode: mode, Symbol: "USD_JPY", PipSize: 0.01,
		Positions: pos, EmergencyFlagPath: flag, Logger: silentLogger(),
		// Step B: positions.strategy_config_id is a NOT NULL FK. Provide
		// an active config so manual entry has a FK target.
		ActiveConfig: func() *config.StrategyConfig {
			return &config.StrategyConfig{ConfigID: "cfg-active-test"}
		},
	}
	switch b := br.(type) {
	case *fakeBroker:
		cmd.Broker = b
	case *fakeLiveBroker:
		cmd.Broker = b
	}
	return cmd
}

func TestManualTradeCommand_PaperMode_Buy_InsertsPosition(t *testing.T) {
	br := &fakeBroker{
		ticker:           &market.Ticker{Bid: 100.00, Ask: 100.01},
		placeOrderResult: &order.Order{OrderID: "ord-1", Price: 100.01, Status: "FILLED"},
	}
	cmd := makeManualCmd(t, br, config.ModePaperConfig, tempFlag(t))

	out, err := cmd.Execute(context.Background(), ManualTradeInput{
		Side: order.SideBuy, TakeProfitPips: 20, StopLossPips: 15, MaxHoldMinutes: 240, Quantity: 100,
	})
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if out.EntryPrice != 100.01 {
		t.Errorf("EntryPrice: got %v want 100.01", out.EntryPrice)
	}
	if !nearly(out.TPPrice, 100.21) {
		t.Errorf("TPPrice: got %v want 100.21", out.TPPrice)
	}
	if !nearly(out.SLPrice, 99.86) {
		t.Errorf("SLPrice: got %v want 99.86", out.SLPrice)
	}
	if out.Quantity != 100 {
		t.Errorf("Quantity: got %d want 100", out.Quantity)
	}
	if br.placeOrderReq.Type != order.OrderTypeMarket {
		t.Errorf("paper mode should use MARKET; got %v", br.placeOrderReq.Type)
	}
}

func TestManualTradeCommand_PaperMode_Sell_ComputesTPSLMirror(t *testing.T) {
	br := &fakeBroker{
		ticker:           &market.Ticker{Bid: 100.00, Ask: 100.01},
		placeOrderResult: &order.Order{OrderID: "ord-1", Price: 100.00, Status: "FILLED"},
	}
	cmd := makeManualCmd(t, br, config.ModePaperConfig, tempFlag(t))

	out, err := cmd.Execute(context.Background(), ManualTradeInput{
		Side: order.SideSell, TakeProfitPips: 20, StopLossPips: 15,
	})
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if !nearly(out.TPPrice, 99.80) {
		t.Errorf("SELL TP: got %v want 99.80", out.TPPrice)
	}
	if !nearly(out.SLPrice, 100.15) {
		t.Errorf("SELL SL: got %v want 100.15", out.SLPrice)
	}
}

// Live entry uses MARKET+OCO instead of IFDOCO single-call:
// two-step (PlaceOrder MARKET → ResolveExecution → PlaceSettleOCO →
// ResolveSettleLegs). Reason: GMO Forex /v1/ifoOrder rejects sizes <
// 10,000; MARKET via /v1/order accepts the symbols-API minimum (100 for
// USD_JPY) so the manual trade UI can place 1-lot tests without margin
// constraints. The OCO TP/SL pair lives on the GMO side.
func TestManualTradeCommand_LiveMode_Buy_UsesMarketPlusOCOClose(t *testing.T) {
	br := &fakeLiveBroker{
		fakeBroker: fakeBroker{
			ticker:           &market.Ticker{Bid: 100.00, Ask: 100.01},
			placeOrderResult: &order.Order{OrderID: "market-1", Price: 0, Status: "ACCEPTED"},
		},
		resolvePosID:  "1000042", // must be numeric — GMO Forex positionId
		resolveFillPx: 100.015,   // slightly different from request estimate to verify TP/SL re-derive from fill
	}
	cmd := makeManualCmd(t, br, config.ModeLiveConfig, tempFlag(t))

	out, err := cmd.Execute(context.Background(), ManualTradeInput{
		Side: order.SideBuy, TakeProfitPips: 20, StopLossPips: 15,
	})
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	// PlaceOrder is MARKET, no Price / TP / SL on the request itself —
	// those belong on the subsequent PlaceSettleOCO call.
	if br.placeOrderReq.Type != order.OrderTypeMarket {
		t.Errorf("live MARKET+OCO path should send Type=MARKET; got %v", br.placeOrderReq.Type)
	}
	if br.placeOrderReq.Price != 0 || br.placeOrderReq.TakeProfit != 0 || br.placeOrderReq.StopLoss != 0 {
		t.Errorf("MARKET entry must not carry Price/TP/SL on the entry call; got %+v", br.placeOrderReq)
	}
	// PlaceSettleOCO must fire exactly once, with TP/SL derived from the
	// ACTUAL fill price (100.015), close side opposite of entry, numeric
	// positionId, matching size.
	if br.settleOCOCalls != 1 {
		t.Fatalf("PlaceSettleOCO must be called once; got %d", br.settleOCOCalls)
	}
	args := br.settleOCOLastArgs
	if args.Side != order.SideSell {
		t.Errorf("OCO close side must be opposite (SELL for BUY entry); got %v", args.Side)
	}
	if args.BrokerPositionID != 1000042 {
		t.Errorf("OCO BrokerPositionID: got %v, want 1000042", args.BrokerPositionID)
	}
	// TP/SL re-derived from actual fill 100.015 (not request estimate 100.01):
	//   TP = 100.015 + 0.20 = 100.215
	//   SL = 100.015 - 0.15 =  99.865
	if !nearly(args.TPPrice, 100.215) {
		t.Errorf("OCO TPPrice (re-derived from fill): got %v want 100.215", args.TPPrice)
	}
	if !nearly(args.SLPrice, 99.865) {
		t.Errorf("OCO SLPrice (re-derived from fill): got %v want 99.865", args.SLPrice)
	}
	if out.EntryPrice != 100.015 {
		t.Errorf("EntryPrice should be ResolveExecution fillPx; got %v", out.EntryPrice)
	}
}

// TestManualTradeCommand_LiveMode_EmergencyPaths: Live モードで emergency_stop
// を発火する 2 経路 (ResolveTimeout / NoResolver) を 1 table に統合。
func TestManualTradeCommand_LiveMode_EmergencyPaths(t *testing.T) {
	cases := []struct {
		name        string
		brokerSetup func() any
	}{
		{
			name: "ResolveExecution timeout",
			brokerSetup: func() any {
				return &fakeLiveBroker{
					fakeBroker: fakeBroker{
						ticker:           &market.Ticker{Bid: 100.00, Ask: 100.01},
						placeOrderResult: &order.Order{OrderID: "ifdoco-1", Price: 0},
					},
					resolveErr: errResolverFailure,
				}
			},
		},
		{
			name: "broker doesn't implement ExecutionResolver",
			brokerSetup: func() any {
				return &fakeBroker{
					ticker:           &market.Ticker{Bid: 100.00, Ask: 100.01},
					placeOrderResult: &order.Order{OrderID: "ifdoco-1", Price: 0},
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			flag := tempFlag(t)
			cmd := makeManualCmd(t, tc.brokerSetup(), config.ModeLiveConfig, flag)
			_, err := cmd.Execute(context.Background(), ManualTradeInput{
				Side: order.SideBuy, TakeProfitPips: 20, StopLossPips: 15,
			})
			if err == nil {
				t.Fatal("expected critical error")
			}
			if _, ferr := os.Stat(flag); ferr != nil {
				t.Errorf("emergency_stop flag must be written; stat err=%v", ferr)
			}
			// DB に position が入っていないこと (ゴーストレコード防止)
			pos := cmd.Positions.(*backtest.InMemoryPositionRepo)
			open, _ := pos.ListOpenOrClosing(context.Background(), "USD_JPY")
			if len(open) != 0 {
				t.Errorf("position must NOT be inserted on critical failure; got %d rows", len(open))
			}
		})
	}
}

// TestManualTradeCommand_Validation: side / pips 入力検証を 1 table に。
func TestManualTradeCommand_Validation(t *testing.T) {
	cases := []struct {
		name    string
		input   ManualTradeInput
		wantErr error
	}{
		{"invalid side", ManualTradeInput{Side: order.Side("INVALID"), TakeProfitPips: 20, StopLossPips: 15}, ErrInvalidSide},
		{"tp_pips <= 0", ManualTradeInput{Side: order.SideBuy, TakeProfitPips: 0, StopLossPips: 15}, ErrInvalidPips},
		{"sl_pips <= 0", ManualTradeInput{Side: order.SideBuy, TakeProfitPips: 20, StopLossPips: 0}, ErrInvalidPips},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd := makeManualCmd(t, &fakeBroker{}, config.ModePaperConfig, tempFlag(t))
			_, err := cmd.Execute(context.Background(), tc.input)
			if !errors.Is(err, tc.wantErr) {
				t.Errorf("expected %v; got %v", tc.wantErr, err)
			}
		})
	}
}

func TestManualTradeCommand_DefaultQuantityWhenZero(t *testing.T) {
	br := &fakeBroker{
		ticker:           &market.Ticker{Bid: 100.00, Ask: 100.01},
		placeOrderResult: &order.Order{OrderID: "ord-1", Price: 100.01},
	}
	cmd := makeManualCmd(t, br, config.ModePaperConfig, tempFlag(t))
	out, err := cmd.Execute(context.Background(), ManualTradeInput{
		Side: order.SideBuy, TakeProfitPips: 20, StopLossPips: 15, Quantity: 0,
	})
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if out.Quantity != safety.DefaultQuantity {
		t.Errorf("Quantity 0 → default %d (safety.DefaultQuantity); got %d", safety.DefaultQuantity, out.Quantity)
	}
}

func TestManualTradeCommand_TickerFails_NoOrderPlaced(t *testing.T) {
	br := &fakeBroker{tickerErr: errors.New("ticker down")}
	cmd := makeManualCmd(t, br, config.ModePaperConfig, tempFlag(t))
	_, err := cmd.Execute(context.Background(), ManualTradeInput{
		Side: order.SideBuy, TakeProfitPips: 20, StopLossPips: 15,
	})
	if err == nil {
		t.Fatal("expected error on ticker failure")
	}
	if br.placeOrderCalls != 0 {
		t.Errorf("PlaceOrder must NOT be called when ticker fails; got %d calls", br.placeOrderCalls)
	}
}

// Mutex は ClosePositionCommand と shared なので Execute() を順次呼べる
// ことを基本動作として確認 (race 検出は -race で全テスト通すことで担保)。
func TestManualTradeCommand_SerialExecutions(t *testing.T) {
	br := &fakeBroker{
		ticker:           &market.Ticker{Bid: 100.00, Ask: 100.01},
		placeOrderResult: &order.Order{OrderID: "ord-x", Price: 100.01},
	}
	cmd := makeManualCmd(t, br, config.ModePaperConfig, tempFlag(t))
	for i := 0; i < 3; i++ {
		if _, err := cmd.Execute(context.Background(), ManualTradeInput{
			Side: order.SideBuy, TakeProfitPips: 20, StopLossPips: 15,
		}); err != nil {
			t.Errorf("iter %d: %v", i, err)
		}
	}
	if br.placeOrderCalls != 3 {
		t.Errorf("PlaceOrder calls: got %d want 3", br.placeOrderCalls)
	}
}

// keep the linter from flagging sync import as unused in some refactors
var _ = sync.Mutex{}
