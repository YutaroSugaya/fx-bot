#!/usr/bin/env bash
# provision_backtest_db.sh — create the ISOLATED fxbot_backtest database and migrate its schema.
#
# Why: the 2015-2023 multi-regime history (for offline backtests across regimes)
# must NOT land in the live money DB (CLAUDE.md: "live DB は read-only"). It goes into a
# separate fxbot_backtest DB in the same postgres instance. The live candles table is never
# touched; offline backtests just point DATABASE_URL at this DB.
#
# Idempotent: re-running is safe (CREATE DATABASE is skipped if it exists; migrate up is a no-op
# when already at head). Touches ONLY fxbot_backtest — never the live fxbot DB.
#
# Usage:
#   bash scripts/provision_backtest_db.sh
# Then:
#   export BACKTEST_DATABASE_URL='postgres://fxbot:...@localhost:5432/fxbot_backtest?sslmode=disable'
#
# Env: FXBOT_PG_CONTAINER (default fxbot-postgres, the docker-compose.yml container) /
#      FXBOT_DB_USER (default fxbot). The live DB name must be 'fxbot' (used to derive the DSN below).
set -euo pipefail

cd "$(dirname "$0")/.."

CONTAINER="${FXBOT_PG_CONTAINER:-fxbot-postgres}"
DB_USER="${FXBOT_DB_USER:-fxbot}"

# Load DATABASE_URL from .env (live DSN — used only to derive the backtest DSN + host/user).
if [ -f .env ]; then
  set -a; . ./.env; set +a
fi
: "${DATABASE_URL:?DATABASE_URL must be set (in .env) to derive the backtest DSN}"

# Derive the backtest DSN by swapping the db name (…/fxbot?… or …/fxbot) -> …/fxbot_backtest.
BACKTEST_DATABASE_URL="$(printf '%s' "$DATABASE_URL" | sed -E 's#/fxbot(\?|$)#/fxbot_backtest\1#')"
if [ "$BACKTEST_DATABASE_URL" = "$DATABASE_URL" ]; then
  echo "ERROR: could not derive fxbot_backtest DSN from DATABASE_URL (expected db name 'fxbot')." >&2
  echo "       DATABASE_URL=$DATABASE_URL" >&2
  exit 1
fi
echo "==> backtest DSN: $(printf '%s' "$BACKTEST_DATABASE_URL" | sed -E 's#://[^@]+@#://***@#')"

# 1. CREATE DATABASE fxbot_backtest (idempotent) via the postgres container, connected to the
#    'postgres' maintenance DB so we never write into the live fxbot DB.
echo "==> ensuring fxbot_backtest database exists"
if docker exec "$CONTAINER" psql -U "$DB_USER" -d postgres -tAc \
     "SELECT 1 FROM pg_database WHERE datname='fxbot_backtest'" | grep -q 1; then
  echo "    fxbot_backtest already exists — skipping CREATE"
else
  docker exec "$CONTAINER" psql -U "$DB_USER" -d postgres -c "CREATE DATABASE fxbot_backtest"
  echo "    created fxbot_backtest"
fi

# 2. Migrate the schema into fxbot_backtest (candles table + the rest; empty trading tables are
#    harmless). cmd/migrate reads DATABASE_URL, so override it for THIS invocation only.
echo "==> migrating schema into fxbot_backtest"
( cd backend && DATABASE_URL="$BACKTEST_DATABASE_URL" go run ./cmd/migrate up )

echo ""
echo "==> done. Export this for the ingest + backtest steps:"
echo "    export BACKTEST_DATABASE_URL='$BACKTEST_DATABASE_URL'"
