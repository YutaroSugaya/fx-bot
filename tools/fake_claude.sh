#!/usr/bin/env bash
# Test stand-in for `claude -p`. Ignores stdin/args and emits a fixed
# strategy_config YAML, to exercise the advisor path (invoke → parse → validate)
# without calling Claude. The values are not tuned to the current
# hard_limits.yaml, so the validator may reject the config — that is expected.
#
# valid_from = current minute (truncated), valid_until = +60 min (TTL 60 min).
# macOS only as written: `date -v+60M` is BSD date (GNU date would need -d '+60 min').

set -euo pipefail

# Drain stdin (the prompt + Input JSON) but don't use it.
cat > /dev/null

# Generate fresh timestamps in JST.
NOW_JST="$(TZ=Asia/Tokyo date '+%Y-%m-%dT%H:%M:00+09:00')"
FUTURE_JST="$(TZ=Asia/Tokyo date -v+60M '+%Y-%m-%dT%H:%M:00+09:00')"
CONFIG_ID="$(date -u +%Y%m%d-%H%M%S)-fake"

cat <<EOF
config_id: "${CONFIG_ID}"
generated_at: "${NOW_JST}"
valid_from: "${NOW_JST}"
valid_until: "${FUTURE_JST}"
symbol: USD_JPY
enabled: true
market_regime:
  type: range
  confidence: 0.65
  reason: "fake-claude: synthetic range hint for end-to-end test"
strategy:
  name: range_reversion
  timeframe: 1m
  trend_timeframe: 5m
entry:
  max_spread_pips: 0.4
  min_volatility_pips_5m: 1.5
  max_volatility_pips_5m: 6.0
  require_breakout: false
  direction: both
exit:
  take_profit_pips: 2.0
  stop_loss_pips: 2.5
  max_hold_minutes: 20
risk:
  quantity: 100
  max_open_positions: 1
  max_trades_in_this_window: 3
  max_loss_in_this_window_jpy: 300
no_trade:
  enabled: false
  reason: ""
EOF
