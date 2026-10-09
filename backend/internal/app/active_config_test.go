package app

import (
	"sync"
	"testing"

	"fx-bot/backend/internal/config"
)

func TestActiveConfigHolder_GetSetPerSymbol(t *testing.T) {
	h := &ActiveConfigHolder{}
	usd := &config.StrategyConfig{ConfigID: "cfg-usd", Symbol: "USD_JPY"}
	eur := &config.StrategyConfig{ConfigID: "cfg-eur", Symbol: "EUR_JPY"}

	h.Set("USD_JPY", usd)
	h.Set("EUR_JPY", eur)

	if got := h.Get("USD_JPY"); got == nil || got.ConfigID != "cfg-usd" {
		t.Errorf("Get(USD_JPY): got %+v, want cfg-usd", got)
	}
	if got := h.Get("EUR_JPY"); got == nil || got.ConfigID != "cfg-eur" {
		t.Errorf("Get(EUR_JPY): got %+v, want cfg-eur", got)
	}
}

func TestActiveConfigHolder_GetUnknownSymbolReturnsNil(t *testing.T) {
	h := &ActiveConfigHolder{}
	h.Set("USD_JPY", &config.StrategyConfig{ConfigID: "cfg-usd"})

	if got := h.Get("GBP_JPY"); got != nil {
		t.Errorf("Get(unknown): got %+v, want nil", got)
	}
}

func TestActiveConfigHolder_SetReplacesPreviousForSameSymbol(t *testing.T) {
	h := &ActiveConfigHolder{}
	h.Set("USD_JPY", &config.StrategyConfig{ConfigID: "old"})
	h.Set("USD_JPY", &config.StrategyConfig{ConfigID: "new"})

	if got := h.Get("USD_JPY"); got == nil || got.ConfigID != "new" {
		t.Errorf("Get after replace: got %+v, want new", got)
	}
}

func TestActiveConfigHolder_AllSnapshot(t *testing.T) {
	h := &ActiveConfigHolder{}
	h.Set("USD_JPY", &config.StrategyConfig{ConfigID: "u"})
	h.Set("EUR_JPY", &config.StrategyConfig{ConfigID: "e"})

	all := h.All()
	if len(all) != 2 {
		t.Fatalf("All: got %d entries, want 2", len(all))
	}
	if all["USD_JPY"].ConfigID != "u" || all["EUR_JPY"].ConfigID != "e" {
		t.Errorf("All: got %+v", all)
	}
}

func TestActiveConfigHolder_ConcurrentSetGet(t *testing.T) {
	// sync.Map ベースなので Mutex 無しで並列 read/write が安全であることを
	// race detector 配下で確認する。
	h := &ActiveConfigHolder{}
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			h.Set("USD_JPY", &config.StrategyConfig{ConfigID: "u"})
			h.Set("EUR_JPY", &config.StrategyConfig{ConfigID: "e"})
		}(i)
		go func() {
			defer wg.Done()
			_ = h.Get("USD_JPY")
			_ = h.Get("EUR_JPY")
		}()
	}
	wg.Wait()
}
