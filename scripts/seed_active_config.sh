#!/usr/bin/env bash
# seed_active_config.sh — strategy config YAML を DB の active config として据える。
# ------------------------------------------------------------------------
# bot は起動時に DB の strategy_configs(status='active', mode=<bot.mode>)を symbol ごとに 1 本読む。
# active が無いシンボルはログを出さずに何も建てない(warn が出るのは DB の読込・検証に失敗したときだけで、
# そのとき paper は続行・live は起動しない)。seed 済みかは起動ログの active_config_loaded_from_db で確かめる。
# このスクリプトは:
#   1. backend の cmd/config-check で、起動時と同じ検証(parse + schema / hard_limits /
#      戦略 whitelist)を掛ける。1 本でも落ちたら何もしない。
#   2. 同じ (mode, symbol) の既存 active を expired にし、渡した config を active で upsert する
#      (行は消さない。過去の trade の外部キーを保つ)。同じ config_id が別の mode / symbol で
#      既にあれば中止する(既存の行を別 mode に書き換えない。YAML の config_id を変えて渡す)。
#
# 使い方(repo root から。既定は dry-run = ROLLBACK で DB を変えない):
#   bash scripts/seed_active_config.sh configs/trend_v4_USD_JPY.yaml
#   bash scripts/seed_active_config.sh --apply configs/trend_v4_USD_JPY.yaml
#   bash scripts/seed_active_config.sh --apply --mode live_config configs/trend_v4_USD_JPY.yaml
#   bash scripts/seed_active_config.sh --print-sql configs/trend_v4_USD_JPY.yaml   # SQL を表示するだけ(DB に触らない)
#
# active config は起動時にしか読まれない。適用は bot を止めてから行い、その後 make start する。
#
# 環境変数: FXBOT_PG_CONTAINER(既定 fxbot-postgres)/ FXBOT_DB_USER(fxbot)/ FXBOT_DB_NAME(fxbot)
set -euo pipefail

cd "$(dirname "$0")/.."   # repo root

CONTAINER="${FXBOT_PG_CONTAINER:-fxbot-postgres}"
DB_USER="${FXBOT_DB_USER:-fxbot}"
DB_NAME="${FXBOT_DB_NAME:-fxbot}"

MODE="paper_config"
APPLY=0
PRINT_ONLY=0
FILES=()
while [ $# -gt 0 ]; do
  case "$1" in
    --apply) APPLY=1; shift ;;
    --dry-run) APPLY=0; shift ;;
    --print-sql) PRINT_ONLY=1; shift ;;
    --mode)
      [ $# -ge 2 ] || { echo ">> --mode には paper_config か live_config を渡す" >&2; exit 2; }
      MODE="$2"; shift 2 ;;
    -h|--help) sed -n 2,21p "$0"; exit 0 ;;
    -*) echo ">> 未知の引数 '$1'" >&2; exit 2 ;;
    *) FILES+=("$1"); shift ;;
  esac
done
case "$MODE" in
  paper_config|live_config) : ;;
  *) echo ">> --mode は paper_config か live_config(got '$MODE')" >&2; exit 2 ;;
esac
if [ ${#FILES[@]} -eq 0 ]; then
  echo ">> config の YAML を 1 本以上渡す(例: configs/trend_v4_USD_JPY.yaml)" >&2
  exit 2
fi

# 1) 起動時と同じ検証。backend/ から見たパスに直して渡す。
ARGS=()
for f in "${FILES[@]}"; do
  [ -f "$f" ] || { echo ">> ファイルが無い: $f" >&2; exit 2; }
  ARGS+=("$(cd "$(dirname "$f")" && pwd)/$(basename "$f")")
done
echo ">> validate: cmd/config-check(起動時と同じ検証)" >&2
if ! FIELDS="$(cd backend && go run ./cmd/config-check -hard-limits ../configs/hard_limits.yaml "${ARGS[@]}")"; then
  echo ">> ABORT: 検証に落ちた config がある。何も seed しない。" >&2
  exit 1
fi

now="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
SYMBOLS=()
while IFS=$'\t' read -r cid sym strat rtype rconf vfrom vuntil; do
  # SQL に埋め込む値は安全な文字だけに限る(クォートや空白を含む config_id 等は中止)。
  for v in "$cid" "$sym" "$strat" "$rtype" "$rconf" "$vfrom" "$vuntil"; do
    if ! [[ "$v" =~ ^[A-Za-z0-9._:+-]+$ ]]; then
      echo ">> ABORT: SQL に入れられない文字を含む値がある: '$v'(英数字と . _ : + - だけにする)" >&2
      exit 1
    fi
  done
  if [[ "$vuntil" < "$now" ]]; then
    echo ">> ABORT: $cid は valid_until=$vuntil で失効済み(失効した config では何も建たない)。" >&2
    exit 1
  fi
  for s in "${SYMBOLS[@]:-}"; do
    if [ "$s" = "$sym" ]; then
      echo ">> ABORT: $sym の config が 2 本ある(1 シンボル 1 本)。" >&2
      exit 1
    fi
  done
  SYMBOLS+=("$sym")
done <<< "$FIELDS"

# 本適用時は稼働中の bot が無いことを要求する(古い active を持ったまま走り続けるのを防ぐ)。
if [ "$APPLY" = "1" ] && [ -d .run ]; then
  for f in .run/*.pid; do
    [ -f "$f" ] || continue
    pid="$(cat "$f" 2>/dev/null || true)"
    if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null; then
      echo ">> ABORT: bot (pid $pid from $f) が稼働中。止めてから適用し、その後 make start。" >&2
      exit 1
    fi
  done
fi

if [ "$MODE" = "live_config" ]; then
  echo ">> ⚠️ live_config 側に seed する。live は bot_config・.env の確認 env・API キーが揃ったときだけ動く。" >&2
fi

# 2) SQL を組み立てる。YAML は dollar quote で丸ごと渡す。
SQL="BEGIN;"$'\n'
i=0
while IFS=$'\t' read -r cid sym strat rtype rconf vfrom vuntil; do
  yaml="$(cat "${FILES[$i]}")"
  i=$((i + 1))
  if [[ "$yaml" == *'$seedyaml$'* ]]; then
    echo ">> ABORT: ${FILES[$((i - 1))]} に区切り文字列 \$seedyaml\$ が含まれている。" >&2
    exit 1
  fi
  SQL+="DO \$guard\$ BEGIN
  IF EXISTS (SELECT 1 FROM strategy_configs
              WHERE config_id = '${cid}' AND (mode <> '${MODE}' OR symbol <> '${sym}')) THEN
    RAISE EXCEPTION 'config_id ${cid} already exists with another mode/symbol; give the YAML a new config_id';
  END IF;
END \$guard\$;
UPDATE strategy_configs SET status = 'expired'
 WHERE mode = '${MODE}' AND symbol = '${sym}' AND status = 'active' AND config_id <> '${cid}';
INSERT INTO strategy_configs
  (config_id, source, mode, symbol, enabled,
   market_regime_type, market_regime_confidence, strategy_name,
   valid_from, valid_until, raw_yaml, status)
VALUES
  ('${cid}', 'manual', '${MODE}', '${sym}', true,
   '${rtype}', ${rconf}, '${strat}', '${vfrom}', '${vuntil}',
   \$seedyaml\$${yaml}\$seedyaml\$, 'active')
ON CONFLICT (config_id) DO UPDATE SET
  status = 'active', enabled = true, source = EXCLUDED.source,
  market_regime_type = EXCLUDED.market_regime_type,
  market_regime_confidence = EXCLUDED.market_regime_confidence,
  strategy_name = EXCLUDED.strategy_name,
  valid_from = EXCLUDED.valid_from, valid_until = EXCLUDED.valid_until,
  raw_yaml = EXCLUDED.raw_yaml;
INSERT INTO strategy_config_activations (config_id, activated_at)
VALUES ('${cid}', now())
ON CONFLICT (config_id) DO UPDATE SET activated_at = EXCLUDED.activated_at;
"
done <<< "$FIELDS"
SQL+="SELECT mode, symbol, config_id, status, strategy_name, valid_until
  FROM strategy_configs WHERE status = 'active' ORDER BY mode, symbol;
"
if [ "$APPLY" = "1" ] && [ "$PRINT_ONLY" = "1" ]; then
  SQL+="COMMIT;"
  echo ">> SQL を表示するだけ(COMMIT 付き・DB には触らない) mode=${MODE}" >&2
elif [ "$APPLY" = "1" ]; then
  SQL+="COMMIT;"
  echo ">> APPLY (COMMIT — DB を変更) mode=${MODE}" >&2
else
  SQL+="ROLLBACK;"
  if [ "$PRINT_ONLY" = "1" ]; then
    echo ">> SQL を表示するだけ(ROLLBACK 付き・DB には触らない。自分の psql で本適用するなら --apply --print-sql) mode=${MODE}" >&2
  else
    echo ">> DRY-RUN (ROLLBACK — DB は変えない。本適用は --apply) mode=${MODE}" >&2
  fi
fi

if [ "$PRINT_ONLY" = "1" ]; then
  printf '%s\n' "$SQL"
  exit 0
fi

printf '%s\n' "$SQL" | docker exec -i "$CONTAINER" psql -U "$DB_USER" -d "$DB_NAME" -v ON_ERROR_STOP=1

if [ "$APPLY" = "1" ]; then
  echo ">> 次: make start(active config は起動時にしか読まれない)"
fi
