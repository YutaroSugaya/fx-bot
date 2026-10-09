package broker

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"fx-bot/backend/internal/domain/order"
	"fx-bot/backend/internal/domain/position"
	"fx-bot/backend/internal/port"
)

func newPaper(t *testing.T, bid, ask float64) *PaperBroker {
	t.Helper()
	clk := time.Date(2026, 5, 15, 10, 0, 0, 0, time.UTC)
	return NewPaperBroker(PaperBrokerConfig{
		Clock: func() time.Time { return clk },
		Pricer: func(_ context.Context, _ string) (float64, float64, error) {
			return bid, ask, nil
		},
	})
}

// compile-time port.Broker satisfaction
var _ port.Broker = (*PaperBroker)(nil)
var _ port.Broker = (*MockBroker)(nil)
var _ port.Broker = (*GmoBroker)(nil)

func TestPaper_PlaceBuy_FillsAtAsk(t *testing.T) {
	b := newPaper(t, 150.10, 150.13)
	ord, err := b.PlaceOrder(context.Background(), order.PlaceOrderRequest{
		Symbol:   "USD_JPY",
		Side:     order.SideBuy,
		Type:     order.OrderTypeMarket,
		Quantity: 100,
	})
	if err != nil {
		t.Fatalf("PlaceOrder: %v", err)
	}
	if ord.Price != 150.13 {
		t.Errorf("BUY fill should be ask 150.13, got %v", ord.Price)
	}
	if ord.Status != "FILLED" {
		t.Errorf("status: %s", ord.Status)
	}
	ps, _ := b.GetOpenPositions(context.Background(), "USD_JPY")
	if len(ps) != 1 || ps[0].EntryPrice != 150.13 || ps[0].Side != order.SideBuy {
		t.Errorf("position: %+v", ps)
	}
}

func TestPaper_PlaceSell_FillsAtBid(t *testing.T) {
	b := newPaper(t, 150.10, 150.13)
	ord, _ := b.PlaceOrder(context.Background(), order.PlaceOrderRequest{
		Symbol:   "USD_JPY",
		Side:     order.SideSell,
		Type:     order.OrderTypeMarket,
		Quantity: 100,
	})
	if ord.Price != 150.10 {
		t.Errorf("SELL fill should be bid 150.10, got %v", ord.Price)
	}
}

func TestPaper_ClosePositionBuy_AtBid_PnLPositive(t *testing.T) {
	// Open at ask 150.10. Move price up so the close (bid) is 150.20.
	open := newPaper(t, 150.07, 150.10)
	openOrd, _ := open.PlaceOrder(context.Background(), order.PlaceOrderRequest{
		Symbol: "USD_JPY", Side: order.SideBuy, Type: order.OrderTypeMarket, Quantity: 100,
	})
	if openOrd.Price != 150.10 {
		t.Fatalf("open price expected 150.10, got %v", openOrd.Price)
	}

	// Swap pricer mid-test to simulate price move (bid 150.20, ask 150.23).
	open.pricer = func(_ context.Context, _ string) (float64, float64, error) {
		return 150.20, 150.23, nil
	}

	ps, _ := open.GetOpenPositions(context.Background(), "USD_JPY")
	closeOrd, err := open.ClosePosition(context.Background(), ps[0])
	if err != nil {
		t.Fatalf("ClosePosition: %v", err)
	}
	if closeOrd.Price != 150.20 {
		t.Errorf("close price should be bid 150.20, got %v", closeOrd.Price)
	}
	if closeOrd.Side != order.SideSell {
		t.Errorf("close side should be opposite (SELL), got %s", closeOrd.Side)
	}

	pnl, err := open.PnLJPY(ps[0].BrokerPositionID)
	if err != nil {
		t.Fatalf("PnLJPY: %v", err)
	}
	// (150.20 - 150.10) * 100 = 10 JPY profit (allow tiny FP slack)
	if abs(pnl-10.0) > 0.01 {
		t.Errorf("pnl expected ~10, got %v", pnl)
	}
}

func abs(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}

func TestPaper_ClosePositionSell_AtAsk_PnLPositive(t *testing.T) {
	// Open at bid 150.10. Price drops so close (ask) is 150.05.
	b := newPaper(t, 150.10, 150.13)
	_, _ = b.PlaceOrder(context.Background(), order.PlaceOrderRequest{
		Symbol: "USD_JPY", Side: order.SideSell, Type: order.OrderTypeMarket, Quantity: 100,
	})
	b.pricer = func(_ context.Context, _ string) (float64, float64, error) {
		return 150.02, 150.05, nil
	}
	ps, _ := b.GetOpenPositions(context.Background(), "USD_JPY")
	closeOrd, _ := b.ClosePosition(context.Background(), ps[0])
	if closeOrd.Price != 150.05 {
		t.Errorf("close ask: got %v", closeOrd.Price)
	}
	pnl, _ := b.PnLJPY(ps[0].BrokerPositionID)
	// SELL: entry 150.10, exit 150.05 → profit (150.10 - 150.05) * 100 = 5
	if abs(pnl-5.0) > 0.01 {
		t.Errorf("pnl expected ~5, got %v", pnl)
	}
}

func TestPaper_ClosePosition_UnknownID(t *testing.T) {
	b := newPaper(t, 150.10, 150.13)
	_, err := b.ClosePosition(context.Background(), position.Position{BrokerPositionID: "nope"})
	if err == nil {
		t.Fatalf("expected error")
	}
}

func TestPaper_ClosePosition_AlreadyClosed(t *testing.T) {
	b := newPaper(t, 150.10, 150.13)
	_, _ = b.PlaceOrder(context.Background(), order.PlaceOrderRequest{
		Symbol: "USD_JPY", Side: order.SideBuy, Type: order.OrderTypeMarket, Quantity: 100,
	})
	ps, _ := b.GetOpenPositions(context.Background(), "USD_JPY")
	if _, err := b.ClosePosition(context.Background(), ps[0]); err != nil {
		t.Fatalf("first close: %v", err)
	}
	if _, err := b.ClosePosition(context.Background(), ps[0]); err == nil {
		t.Fatalf("expected error for second close")
	}
}

// --- symbol-aware pip lookup ---
// PaperBroker must derive pip size per-position via market.PipSize(symbol) so
// a single broker instance can serve multiple JPY-quote symbols at once.
// Non-JPY-quote symbols (EUR_USD etc.) are intentionally out of scope here
// because PnLJPY needs a JPY conversion that lives in a future plan.

// mixedSymbolPricer returns distinct (bid, ask) per symbol.
func mixedSymbolPricer(prices map[string][2]float64) PricerFunc {
	return func(_ context.Context, sym string) (float64, float64, error) {
		p, ok := prices[sym]
		if !ok {
			return 0, 0, fmt.Errorf("no price for %s", sym)
		}
		return p[0], p[1], nil
	}
}

func TestPaper_MixedJPYQuoteSymbols_TPSLPipsUseSymbolPip(t *testing.T) {
	// JPY-quote symbols share pip = 0.01 so the numerical assertion is
	// identical regardless of whether the broker resolves pip per-call or
	// uses a single construction-time pip. The regression this test guards
	// is "the broker started ignoring market.PipSize(symbol) again" — once
	// non-JPY symbols are added (future plan) the same shape catches the
	// drift immediately.
	b := NewPaperBroker(PaperBrokerConfig{
		Clock: func() time.Time { return time.Date(2026, 5, 27, 10, 0, 0, 0, time.UTC) },
		Pricer: mixedSymbolPricer(map[string][2]float64{
			"USD_JPY": {150.10, 150.13},
			"EUR_JPY": {163.40, 163.45},
		}),
	})

	// USD_JPY BUY with TP 20 pips above fill (150.13 + 0.20 = 150.33)
	_, _ = b.PlaceOrder(context.Background(), order.PlaceOrderRequest{
		Symbol: "USD_JPY", Side: order.SideBuy, Type: order.OrderTypeIFDOCO,
		Quantity: 100, TakeProfit: 150.33, StopLoss: 150.03,
	})
	// EUR_JPY BUY with TP 25 pips above fill (163.45 + 0.25 = 163.70)
	_, _ = b.PlaceOrder(context.Background(), order.PlaceOrderRequest{
		Symbol: "EUR_JPY", Side: order.SideBuy, Type: order.OrderTypeIFDOCO,
		Quantity: 100, TakeProfit: 163.70, StopLoss: 163.20,
	})

	usd, _ := b.GetOpenPositions(context.Background(), "USD_JPY")
	eur, _ := b.GetOpenPositions(context.Background(), "EUR_JPY")
	if len(usd) != 1 || len(eur) != 1 {
		t.Fatalf("expected one position per symbol, got USD=%d EUR=%d", len(usd), len(eur))
	}
	if usd[0].TakeProfitPips != 20 || usd[0].StopLossPips != 10 {
		t.Errorf("USD_JPY TP/SL pips: %v / %v, want 20 / 10",
			usd[0].TakeProfitPips, usd[0].StopLossPips)
	}
	if eur[0].TakeProfitPips != 25 || eur[0].StopLossPips != 25 {
		t.Errorf("EUR_JPY TP/SL pips: %v / %v, want 25 / 25",
			eur[0].TakeProfitPips, eur[0].StopLossPips)
	}
}

func TestPaper_MixedJPYQuoteSymbols_SlippageUsesSymbolPip(t *testing.T) {
	// Slippage is `slippagePips * market.PipSize(req.Symbol)`. JPY-quote
	// symbols all share pip = 0.01 so the numbers below also match a
	// hypothetical broker-level pip, but the test pins the per-call source
	// of pip — a future broker change that swaps in a stored pip would
	// still pass for these symbols, yet fail the moment a non-JPY pair is
	// added.
	b := NewPaperBroker(PaperBrokerConfig{
		SlippagePips: 0.5,
		Clock:        func() time.Time { return time.Date(2026, 5, 27, 10, 0, 0, 0, time.UTC) },
		Pricer: mixedSymbolPricer(map[string][2]float64{
			"USD_JPY": {150.10, 150.13},
			"EUR_JPY": {163.40, 163.45},
		}),
	})
	usdOrd, _ := b.PlaceOrder(context.Background(), order.PlaceOrderRequest{
		Symbol: "USD_JPY", Side: order.SideBuy, Type: order.OrderTypeMarket, Quantity: 100,
	})
	eurOrd, _ := b.PlaceOrder(context.Background(), order.PlaceOrderRequest{
		Symbol: "EUR_JPY", Side: order.SideBuy, Type: order.OrderTypeMarket, Quantity: 100,
	})
	if abs(usdOrd.Price-150.135) > 1e-6 {
		t.Errorf("USD_JPY fill: %v, want 150.135 (ask + 0.5 pip)", usdOrd.Price)
	}
	if abs(eurOrd.Price-163.455) > 1e-6 {
		t.Errorf("EUR_JPY fill: %v, want 163.455 (ask + 0.5 pip)", eurOrd.Price)
	}
}

func TestPaper_TPSLPipsCalculatedAtFill(t *testing.T) {
	b := newPaper(t, 150.10, 150.13)
	_, _ = b.PlaceOrder(context.Background(), order.PlaceOrderRequest{
		Symbol:     "USD_JPY",
		Side:       order.SideBuy,
		Type:       order.OrderTypeIFDOCO,
		Quantity:   100,
		TakeProfit: 150.33, // 20 pips above fill
		StopLoss:   150.03, // 10 pips below fill
	})
	ps, _ := b.GetOpenPositions(context.Background(), "USD_JPY")
	if ps[0].TakeProfitPips != 20 {
		t.Errorf("tp pips: %v", ps[0].TakeProfitPips)
	}
	if ps[0].StopLossPips != 10 {
		t.Errorf("sl pips: %v", ps[0].StopLossPips)
	}
}

func TestPaper_PlaceOrder_QuantityValidation(t *testing.T) {
	b := newPaper(t, 150.10, 150.13)
	_, err := b.PlaceOrder(context.Background(), order.PlaceOrderRequest{
		Symbol: "USD_JPY", Side: order.SideBuy, Type: order.OrderTypeMarket, Quantity: 0,
	})
	if err == nil {
		t.Errorf("expected quantity err")
	}
}

func TestPaper_GetTicker(t *testing.T) {
	b := newPaper(t, 150.10, 150.13)
	tk, err := b.GetTicker(context.Background(), "USD_JPY")
	if err != nil {
		t.Fatalf("GetTicker: %v", err)
	}
	if tk.Bid != 150.10 || tk.Ask != 150.13 {
		t.Errorf("ticker: %+v", tk)
	}
}

func TestPaper_ConcurrentPlaceAndQuery(t *testing.T) {
	b := newPaper(t, 150.10, 150.13)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = b.PlaceOrder(context.Background(), order.PlaceOrderRequest{
				Symbol: "USD_JPY", Side: order.SideBuy, Type: order.OrderTypeMarket, Quantity: 100,
			})
		}()
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = b.GetOpenPositions(context.Background(), "USD_JPY")
		}()
	}
	wg.Wait()
	ps, _ := b.GetOpenPositions(context.Background(), "USD_JPY")
	if len(ps) != 20 {
		t.Errorf("expected 20 positions, got %d", len(ps))
	}
}

// Restore: bot 再起動時に DB の OPEN ポジションを PaperBroker メモリへ戻すパス。
// これがないと再起動直後の TP/SL/MaxHold 決済で「unknown position」エラーが出る。
func TestPaper_Restore_PutsPositionBackInMemory(t *testing.T) {
	b := NewPaperBroker(PaperBrokerConfig{
		Pricer: func(ctx context.Context, _ string) (float64, float64, error) {
			return 158.00, 158.03, nil
		},
	})
	restored := []position.Position{{
		BrokerPositionID: "paper-pos-42",
		Symbol:           "USD_JPY",
		Side:             order.SideBuy,
		Quantity:         100,
		EntryPrice:       158.00,
		TakeProfitPips:   5.0,
		StopLossPips:     3.0,
		MaxHoldMinutes:   30,
		StrategyConfigID: "cfg-prev",
		Status:           position.StatusOpen,
	}}
	b.Restore(restored)

	ps, err := b.GetOpenPositions(context.Background(), "USD_JPY")
	if err != nil || len(ps) != 1 {
		t.Fatalf("expected 1 restored position, got %d, err=%v", len(ps), err)
	}
	if ps[0].BrokerPositionID != "paper-pos-42" {
		t.Errorf("BrokerPositionID: %q", ps[0].BrokerPositionID)
	}
	// Restore 前は unknown position で失敗していた ClosePosition が通る
	if _, err := b.ClosePosition(context.Background(), ps[0]); err != nil {
		t.Errorf("ClosePosition on restored position: %v", err)
	}
}

func TestPaper_Restore_NextIDAdvancesToAvoidCollision(t *testing.T) {
	b := NewPaperBroker(PaperBrokerConfig{
		Pricer: func(ctx context.Context, _ string) (float64, float64, error) {
			return 158.00, 158.03, nil
		},
	})
	// 復元データの最大 ID が 100。新規約定で 100 以下を再利用すると衝突する。
	b.Restore([]position.Position{{
		BrokerPositionID: "paper-pos-100", Symbol: "USD_JPY", Side: order.SideBuy,
		Quantity: 100, EntryPrice: 158.00, Status: position.StatusOpen,
	}})
	ord, err := b.PlaceOrder(context.Background(), order.PlaceOrderRequest{
		Symbol: "USD_JPY", Side: order.SideBuy, Type: order.OrderTypeMarket, Quantity: 100,
	})
	if err != nil {
		t.Fatalf("PlaceOrder: %v", err)
	}
	if n, ok := parsePaperIDSuffix(ord.OrderID); !ok || n <= 100 {
		t.Errorf("new order ID %q did not advance past restored max 100", ord.OrderID)
	}
}

// Slippage / fee tests ------------------------------------------

// newPaperWithCosts builds a PaperBroker that applies execution cost (slippage
// in pips on every fill, fee in JPY on every close).
func newPaperWithCosts(t *testing.T, bid, ask, slippagePips, feeJPY float64) *PaperBroker {
	t.Helper()
	clk := time.Date(2026, 5, 15, 10, 0, 0, 0, time.UTC)
	return NewPaperBroker(PaperBrokerConfig{
		SlippagePips:   slippagePips,
		FeeJPYPerTrade: feeJPY,
		Clock:          func() time.Time { return clk },
		Pricer: func(_ context.Context, _ string) (float64, float64, error) {
			return bid, ask, nil
		},
	})
}

func TestPaper_Slippage_PlaceBuyAddsToAsk(t *testing.T) {
	// bid 150.10 / ask 150.13, slip 0.5 pips → BUY fills at 150.13 + 0.005 = 150.135
	b := newPaperWithCosts(t, 150.10, 150.13, 0.5, 0)
	ord, err := b.PlaceOrder(context.Background(), order.PlaceOrderRequest{
		Symbol: "USD_JPY", Side: order.SideBuy, Type: order.OrderTypeMarket, Quantity: 100,
	})
	if err != nil {
		t.Fatalf("PlaceOrder: %v", err)
	}
	want := 150.135
	if absDiff(ord.Price, want) > 1e-9 {
		t.Errorf("BUY fill with slip: got %v want %v", ord.Price, want)
	}
}

func TestPaper_Slippage_PlaceSellSubtractsFromBid(t *testing.T) {
	// bid 150.10 / ask 150.13, slip 0.5 pips → SELL fills at 150.10 - 0.005 = 150.095
	b := newPaperWithCosts(t, 150.10, 150.13, 0.5, 0)
	ord, _ := b.PlaceOrder(context.Background(), order.PlaceOrderRequest{
		Symbol: "USD_JPY", Side: order.SideSell, Type: order.OrderTypeMarket, Quantity: 100,
	})
	want := 150.095
	if absDiff(ord.Price, want) > 1e-9 {
		t.Errorf("SELL fill with slip: got %v want %v", ord.Price, want)
	}
}

func TestPaper_Slippage_CloseBuySubtractsFromBid(t *testing.T) {
	// Open BUY at 150.13 (no slip baseline). Then close at bid 150.20 - 0.005 = 150.195
	b := newPaperWithCosts(t, 150.10, 150.13, 0.5, 0)
	ord, _ := b.PlaceOrder(context.Background(), order.PlaceOrderRequest{
		Symbol: "USD_JPY", Side: order.SideBuy, Type: order.OrderTypeMarket, Quantity: 100,
	})
	// Move price up for the close.
	b.SwapPricer(func(_ context.Context, _ string) (float64, float64, error) {
		return 150.20, 150.23, nil
	})
	open, _ := b.GetOpenPositions(context.Background(), "USD_JPY")
	if len(open) != 1 {
		t.Fatalf("expected 1 open, got %d", len(open))
	}
	closeOrd, err := b.ClosePosition(context.Background(), open[0])
	if err != nil {
		t.Fatalf("ClosePosition: %v", err)
	}
	want := 150.195
	if absDiff(closeOrd.Price, want) > 1e-9 {
		t.Errorf("BUY close with slip: got %v want %v", closeOrd.Price, want)
	}
	_ = ord
}

func TestPaper_Slippage_CloseSellAddsToAsk(t *testing.T) {
	// SELL close = buy back at ask + slip. Open at bid 150.10 (no slip baseline),
	// then close at ask 150.05 + 0.005 = 150.055.
	b := newPaperWithCosts(t, 150.10, 150.13, 0.5, 0)
	_, _ = b.PlaceOrder(context.Background(), order.PlaceOrderRequest{
		Symbol: "USD_JPY", Side: order.SideSell, Type: order.OrderTypeMarket, Quantity: 100,
	})
	b.SwapPricer(func(_ context.Context, _ string) (float64, float64, error) {
		return 150.02, 150.05, nil
	})
	open, _ := b.GetOpenPositions(context.Background(), "USD_JPY")
	closeOrd, _ := b.ClosePosition(context.Background(), open[0])
	want := 150.055
	if absDiff(closeOrd.Price, want) > 1e-9 {
		t.Errorf("SELL close with slip: got %v want %v", closeOrd.Price, want)
	}
}

func TestPaper_Fee_SubtractedFromPnL(t *testing.T) {
	// BUY 100 units @ 150.13, close @ 150.20 (bid, no slip in this test).
	// gross PnL = (150.20 - 150.13) * 100 = 7 JPY; fee 3 JPY → net = 4 JPY.
	b := newPaperWithCosts(t, 150.10, 150.13, 0, 3.0)
	_, _ = b.PlaceOrder(context.Background(), order.PlaceOrderRequest{
		Symbol: "USD_JPY", Side: order.SideBuy, Type: order.OrderTypeMarket, Quantity: 100,
	})
	b.SwapPricer(func(_ context.Context, _ string) (float64, float64, error) {
		return 150.20, 150.23, nil
	})
	open, _ := b.GetOpenPositions(context.Background(), "USD_JPY")
	_, _ = b.ClosePosition(context.Background(), open[0])
	pnl, err := b.PnLJPY(open[0].BrokerPositionID)
	if err != nil {
		t.Fatalf("PnLJPY: %v", err)
	}
	want := 4.0
	if absDiff(pnl, want) > 1e-9 {
		t.Errorf("net PnL with fee: got %v want %v", pnl, want)
	}
}

// Sanity: when slip=0 fee=0 the existing tests continue to pass — covered by
// TestPaper_PlaceBuy_FillsAtAsk etc above (newPaper has slip=0 fee=0).

func TestParsePaperIDSuffix(t *testing.T) {
	cases := []struct {
		in   string
		want int64
		ok   bool
	}{
		{"paper-pos-42", 42, true},
		{"paper-ord-1", 1, true},
		{"paper-pos-0", 0, true},
		{"no-number", 0, false},
		{"", 0, false},
		{"paper-pos-", 0, false},
	}
	for _, c := range cases {
		got, ok := parsePaperIDSuffix(c.in)
		if ok != c.ok || got != c.want {
			t.Errorf("parsePaperIDSuffix(%q) = (%d, %v), want (%d, %v)", c.in, got, ok, c.want, c.ok)
		}
	}
}
