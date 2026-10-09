#!/usr/bin/env bash
# pretooluse-deny_test.sh — pretooluse-deny.sh の自動テスト。
#
# 使い方: bash .claude/hooks/pretooluse-deny_test.sh
# 各ケースは hook に Claude Code 形式の JSON を stdin で渡し、exit code を検証する。
#   deny  = exit 2 を期待
#   allow = exit 0 を期待
set -u

hook="$(cd "$(dirname "$0")" && pwd)/pretooluse-deny.sh"
pass=0
fail=0

run_case() {
  local expect="$1" name="$2" tool="$3" cmd="$4"
  local json rc
  json=$(printf '{"tool_name":"%s","tool_input":{"command":"%s"}}' "$tool" "$cmd")
  printf '%s' "$json" | bash "$hook" >/dev/null 2>&1
  rc=$?
  case "$expect" in
    deny)  [ "$rc" -eq 2 ] ;;
    allow) [ "$rc" -eq 0 ] ;;
  esac
  if [ $? -eq 0 ]; then
    pass=$((pass + 1))
    echo "  ok   [$expect] $name"
  else
    fail=$((fail + 1))
    echo "  FAIL [$expect, got rc=$rc] $name"
  fi
}

echo "=== existing rules (regression) ==="
run_case deny  "go test -tags integration"            Bash "cd backend && go test -tags integration ./..."
run_case deny  "go test -tags=integration"            Bash "go test -tags=integration ./internal/adapter/repository/"
run_case allow "go test (no tags)"                    Bash "cd backend && go test ./..."
run_case deny  "docker compose down"                  Bash "docker compose down -v"
run_case deny  "docker compose stop"                  Bash "docker compose stop postgres"
run_case allow "docker compose up"                    Bash "docker compose up -d postgres"
run_case allow "docker compose ps"                    Bash "docker compose ps postgres"
run_case deny  "rm -rf docker-data"                   Bash "rm -rf .docker-data/postgres"
run_case deny  "DROP SCHEMA"                          Bash "psql \\\"\$DATABASE_URL\\\" -c 'DROP SCHEMA public CASCADE'"
run_case deny  "DROP DATABASE"                        Bash "psql -c 'DROP DATABASE fxbot'"
run_case deny  "TRUNCATE"                             Bash "psql \\\"\$DATABASE_URL\\\" -c 'TRUNCATE trades'"
run_case deny  "pkill postgres"                       Bash "pkill -f postgres"
run_case allow "non-Bash tool"                        Read "DROP SCHEMA public CASCADE"
run_case allow "psql SELECT"                          Bash "psql \\\"\$DATABASE_URL\\\" -c 'SELECT count(*) FROM trades'"
run_case allow "migrate up"                           Bash "go run ./cmd/migrate up"

echo "=== bot プロセス停止禁止 ==="
run_case deny  "pkill exe/bot"                        Bash "pkill -f /var/folders/xx/exe/bot"
run_case deny  "pkill cmd/bot"                        Bash "pkill -f 'go run ./cmd/bot'"
run_case deny  "killall fxbot"                        Bash "killall fxbot"
run_case allow "pkill unrelated"                      Bash "pkill -f node_modules/.bin/vite"
run_case allow "make kill-stale (Makefile FORCE ガードに委譲)" Bash "make kill-stale"

echo "=== psql 書込系 deny (live DB read-only) ==="
run_case deny  "psql UPDATE live"                     Bash "psql 'postgres://fxbot:fxbot@localhost:5432/fxbot?sslmode=disable' -c 'UPDATE positions SET max_hold_minutes = 780 WHERE id = 169'"
run_case deny  "psql DELETE live"                     Bash "psql \\\"\$DATABASE_URL\\\" -c 'DELETE FROM strategy_configs WHERE id = 3'"
run_case deny  "psql INSERT live"                     Bash "psql \\\"\$DATABASE_URL\\\" -c \\\"INSERT INTO strategy_configs (config_id) VALUES ('x')\\\""
run_case deny  "psql ALTER live"                      Bash "psql \\\"\$DATABASE_URL\\\" -c 'ALTER TABLE trades ADD COLUMN x int'"
run_case deny  "docker exec psql DELETE"              Bash "docker exec fx-bot-postgres-1 psql -U fxbot -d fxbot -c 'DELETE FROM trades'"
run_case allow "psql UPDATE on fxbot_test"            Bash "psql 'postgres://fxbot:fxbot@localhost:5432/fxbot_test?sslmode=disable' -c 'UPDATE trades SET note = 1'"
run_case allow "psql UPDATE human-approved"           Bash "FXBOT_HUMAN_APPROVED_DB_WRITE=1 psql \\\"\$DATABASE_URL\\\" -c 'UPDATE bot_configs SET max_trades = 3'"
run_case allow "psql SELECT updated_at (no false-positive)" Bash "psql \\\"\$DATABASE_URL\\\" -c 'SELECT id, updated_at FROM positions ORDER BY updated_at'"
run_case allow "non-psql command containing UPDATE word"    Bash "git commit -m 'docs: UPDATE handling notes'"

echo "=== make 経由の間接 postgres停止 / schema巻き戻し / DB上書き ==="
run_case deny  "make db-down (= docker compose down)"  Bash "make db-down"
run_case deny  "make migrate-down (= cmd/migrate down)" Bash "make migrate-down"
run_case deny  "make backup-restore (DROP->CREATE->restore)" Bash "make backup-restore"
run_case deny  "make -C . db-down"                    Bash "make -C . db-down"
run_case deny  "compound that ends in make db-down"   Bash "make db-up && make db-down"
run_case deny  "make db-down followed by ;"           Bash "make db-down; echo done"
run_case deny  "make backup-restore followed by &&"   Bash "make backup-restore&& echo done"
run_case allow "make db-up"                           Bash "make db-up"
run_case allow "make migrate-up"                      Bash "make migrate-up"
run_case allow "make check-backend"                   Bash "make check-backend"
run_case allow "make test"                            Bash "make test ./..."

echo "=== migrate down 直接実行 (live は test/human 以外 deny) ==="
run_case deny  "go run ./cmd/migrate down (live)"     Bash "go run ./cmd/migrate down"
run_case allow "migrate down on _test DSN"            Bash "DATABASE_URL=postgres://fxbot@localhost/fxbot_test go run ./cmd/migrate down"
run_case allow "migrate down human-approved"          Bash "FXBOT_HUMAN_APPROVED_DB_WRITE=1 go run ./cmd/migrate down"

echo "=== psql -f / dropdb / pg_restore (ファイル/別バイナリ経由の破壊) ==="
run_case deny  "psql -f file (live)"                  Bash "psql postgres://fxbot@localhost/fxbot -f /tmp/wipe.sql"
run_case deny  "psql --file= (live)"                  Bash "psql postgres://fxbot@localhost/fxbot --file=/tmp/wipe.sql"
run_case deny  "dropdb live"                          Bash "dropdb fxbot"
run_case deny  "pg_restore --clean live"             Bash "pg_restore --clean -d fxbot /tmp/dump.tar"
run_case allow "psql -f on fxbot_test"                Bash "psql postgres://fxbot@localhost/fxbot_test -f /tmp/seed.sql"
run_case allow "psql -f human-approved"               Bash "FXBOT_HUMAN_APPROVED_DB_WRITE=1 psql postgres://fxbot@localhost/fxbot -f /tmp/fix.sql"
run_case allow "dropdb fxbot_test"                    Bash "dropdb fxbot_test"
run_case allow "pg_restore on _test"                  Bash "pg_restore --clean -d fxbot_test /tmp/dump.tar"
run_case allow "pg_dump backup (read-only, must allow)" Bash "pg_dump postgres://fxbot@localhost/fxbot -Fc -f /tmp/backup.dump"
run_case allow "psql -c SELECT with -f-looking text"  Bash "psql postgres://fxbot@localhost/fxbot -c 'SELECT 1'"

echo "=== plain docker でコンテナ停止/再起動 (compose 非経由) ==="
run_case deny  "docker stop fxbot-postgres"          Bash "docker stop fxbot-postgres"
run_case deny  "docker kill fxbot-postgres"          Bash "docker kill fxbot-postgres"
run_case deny  "docker restart fxbot-postgres"       Bash "docker restart fxbot-postgres"
run_case deny  "docker rm -f fxbot-postgres"         Bash "docker rm -f fxbot-postgres"
run_case deny  "docker pause fxbot-postgres"         Bash "docker pause fxbot-postgres"
run_case deny  "docker compose kill postgres"        Bash "docker compose kill postgres"
run_case deny  "docker compose restart postgres"     Bash "docker compose restart postgres"
run_case allow "docker exec fxbot-postgres SELECT"   Bash "docker exec fxbot-postgres psql -U fxbot -d fxbot -c 'SELECT 1'"
run_case allow "docker ps"                           Bash "docker ps"
run_case allow "docker logs fxbot-postgres"          Bash "docker logs fxbot-postgres"
run_case allow "docker restart unrelated app"        Bash "docker restart my-other-app"

echo "=== psql のパイプ/stdin 流し込み (SQL本文がgrep不可視) ==="
run_case deny  "cat x.sql | psql live"               Bash "cat /tmp/x.sql | psql postgres://fxbot@localhost/fxbot"
run_case deny  "psql live < x.sql"                   Bash "psql postgres://fxbot@localhost/fxbot < /tmp/x.sql"
run_case allow "cat x.sql | psql _test"              Bash "cat /tmp/x.sql | psql postgres://fxbot@localhost/fxbot_test"
run_case allow "psql SELECT with < operator"         Bash "psql postgres://fxbot@localhost/fxbot -c 'SELECT a FROM t WHERE a < 5'"
run_case allow "psql output piped to grep"           Bash "psql postgres://fxbot@localhost/fxbot -c 'SELECT 1' | grep 1"

echo "=== psql -f 連結形 (スペース無し) ==="
run_case deny  "psql -f/tmp/x.sql (no space)"        Bash "psql postgres://fxbot@localhost/fxbot -f/tmp/wipe.sql"

echo "=== read-only/_test の DROP/TRUNCATE 誤検知 + 脱出口 ==="
run_case allow "TRUNCATE on fxbot_test"              Bash "psql postgres://fxbot@localhost/fxbot_test -c 'TRUNCATE trades'"
run_case allow "DROP DATABASE fxbot_test"            Bash "psql postgres://fxbot@localhost/fxbot_test -c 'DROP DATABASE old_test'"
run_case allow "human-approved TRUNCATE"             Bash "FXBOT_HUMAN_APPROVED_DB_WRITE=1 psql postgres://fxbot@localhost/fxbot -c 'TRUNCATE staging'"

echo "=== Bash 経由で enforcement ファイルを書換え/削除 (防壁 disarm) ==="
run_case deny  "cat > deny.sh (heredoc 上書き)"      Bash "cat > .claude/hooks/pretooluse-deny.sh"
run_case deny  "printf > deny.sh (neuter)"           Bash "printf 'exit 0' > .claude/hooks/pretooluse-deny.sh"
run_case deny  "echo >> settings.json"               Bash "echo x >> .claude/settings.json"
run_case deny  "sed -i deny.sh"                       Bash "sed -i '' 's/exit 2/exit 0/' .claude/hooks/pretooluse-deny.sh"
run_case deny  "tee settings.json"                    Bash "echo x | tee .claude/settings.json"
run_case deny  "cp over deny.sh"                      Bash "cp /tmp/x.sh .claude/hooks/pretooluse-deny.sh"
run_case deny  "mv over deny.sh"                      Bash "mv /tmp/x.sh .claude/hooks/pretooluse-deny.sh"
run_case deny  "rm deny.sh"                           Bash "rm .claude/hooks/pretooluse-deny.sh"
run_case deny  "echo > .githooks/pre-push"            Bash "echo bad > .githooks/pre-push"
run_case allow "run deny test (2>&1 redirect)"       Bash "bash .claude/hooks/pretooluse-deny_test.sh 2>&1 | tail -6"
run_case allow "git add hook file"                   Bash "git add .claude/hooks/pretooluse-deny.sh"
run_case allow "cat (read) hook file"                Bash "cat .claude/hooks/pretooluse-deny.sh"
run_case allow "grep hook file"                      Bash "grep -n deny .claude/hooks/pretooluse-deny.sh"
run_case allow "chmod +x new hook"                   Bash "chmod +x .claude/hooks/pretooluse-deny-edit.sh"
run_case allow "write notes mentioning path to /tmp" Bash "echo 'see .claude/hooks/ for details' > /tmp/notes.txt"
run_case allow "rm unrelated tmp file"               Bash "rm /tmp/x.sql"

echo "=== tighter _test escape ==="
run_case deny  "psql DELETE live + unrelated _test word"  Bash "psql postgres://fxbot@localhost/fxbot -c 'DELETE FROM trades' && go vet ./foo_test.go"
run_case deny  "TRUNCATE live table named x_test"         Bash "psql postgres://fxbot@localhost/fxbot -c 'TRUNCATE trades_test'"
run_case deny  "dropdb live then ls *_test"               Bash "dropdb fxbot && ls internal_test"
run_case deny  "migrate down live, grep _test after"      Bash "go run ./cmd/migrate down; grep -r foo_test ."
run_case allow "docker exec psql -d fxbot_test TRUNCATE"  Bash "docker exec fxbot-postgres psql -U fxbot -d fxbot_test -c 'TRUNCATE trades'"
run_case allow "dropdb --if-exists fxbot_test"            Bash "dropdb --if-exists fxbot_test"
run_case deny  "abs-path redirect into hooks"              Bash "printf 'exit 0' > /Users/x/fx-bot/.claude/hooks/pretooluse-deny.sh"
long_tail=$(for i in $(seq 1 3000); do printf '%s\\n' 'xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx'; done)
run_case deny  "multi-line 240KB command (SIGPIPE)"         Bash "docker compose down -v\\n${long_tail}"

run_case deny  "make stop (compose down + quit Docker)"   Bash "make stop"
run_case deny  "make -s stop"                              Bash "make -s stop"
run_case allow "make test | grep stop"                     Bash "make test 2>&1 | grep stop"
run_case allow "make start"                                Bash "make start FE_PORT=3001"

echo
echo "pass=$pass fail=$fail"
[ "$fail" -eq 0 ] || exit 1
exit 0
