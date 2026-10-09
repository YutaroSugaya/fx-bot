import type { Status } from './types'

// pickActiveSymbol decides which symbol the dashboard should pin when a new
// /api/status arrives. The picker keeps a user-selected symbol stable across
// reloads while the user is still pointing at a known symbol, and falls back
// to the first symbol of the freshly-received list otherwise.
//
// Inputs:
//   currentSelection — what state.selectedSymbol holds right now (null on
//                      first paint, or a previously-picked symbol)
//   status           — the just-fetched /api/status payload, or null if the
//                      fetch failed
//
// Output: the symbol to set into state, or null if status didn't carry a
// usable symbols[] array.
export function pickActiveSymbol(currentSelection: string | null, status: Status | null): string | null {
  if (!status || !status.symbols || status.symbols.length === 0) {
    return currentSelection // no fresh data to base a decision on
  }
  const known = new Set(status.symbols.map((s) => s.symbol))
  if (currentSelection && known.has(currentSelection)) {
    return currentSelection
  }
  return status.symbols[0].symbol
}
