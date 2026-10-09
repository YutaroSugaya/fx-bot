package app

import (
	"sync"

	"fx-bot/backend/internal/config"
)

// ActiveConfigHolder maps each symbol to its current active strategy config.
// Worker goroutines read on every tick; the advisor cycle replaces the
// per-symbol slot on each successful promotion. sync.Map keeps reads
// lock-free under steady-state load.
type ActiveConfigHolder struct {
	configs sync.Map // map[string]*config.StrategyConfig
}

// Get returns the active config for symbol, or nil if none has been set.
func (h *ActiveConfigHolder) Get(symbol string) *config.StrategyConfig {
	v, ok := h.configs.Load(symbol)
	if !ok {
		return nil
	}
	cfg, _ := v.(*config.StrategyConfig)
	return cfg
}

// Set replaces the active config for symbol.
func (h *ActiveConfigHolder) Set(symbol string, c *config.StrategyConfig) {
	h.configs.Store(symbol, c)
}

// All returns a snapshot of every symbol → active config currently held.
// The map is a copy; mutating it does not affect the holder.
func (h *ActiveConfigHolder) All() map[string]*config.StrategyConfig {
	out := map[string]*config.StrategyConfig{}
	h.configs.Range(func(k, v any) bool {
		sym, _ := k.(string)
		cfg, _ := v.(*config.StrategyConfig)
		out[sym] = cfg
		return true
	})
	return out
}
