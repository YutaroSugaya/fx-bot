package app

import "sync"

// PlaybookHolder maps each symbol to its current LLM "playbook" — the accumulated rules + lessons
// the reflection loop maintains and the decision cycle reads each tick. Mirrors ActiveConfigHolder:
// sync.Map keeps the decision-cycle read lock-free; the (infrequent) reflection cycle calls Set.
//
// The playbook is ADVISORY TEXT the decision LLM reads — never an executable config. It must never
// flow into strategy_configs / promotion / OnSignal; the decision cycle's hard geometry/qty come
// from config + the parser, not from this text.
type PlaybookHolder struct {
	rules sync.Map // map[string]string
}

// Get returns the current playbook rules for symbol, or "" if none set.
func (h *PlaybookHolder) Get(symbol string) string {
	v, ok := h.rules.Load(symbol)
	if !ok {
		return ""
	}
	s, _ := v.(string)
	return s
}

// Set replaces the playbook for symbol (called by the reflection cycle after a validated update).
func (h *PlaybookHolder) Set(symbol, rules string) {
	h.rules.Store(symbol, rules)
}
