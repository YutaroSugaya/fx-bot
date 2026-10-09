# fx-bot Makefile — すべてプロジェクトルートから実行する前提。
#
# 主要ターゲット:
#   make start    — Docker Desktop + postgres + migrate + bot + frontend を一括起動
#                   (Docker daemon が落ちてれば自動で起こす。Ctrl+C で bot/fe は止まる)
#   make stop     — bot + frontend + postgres + Docker Desktop を停止
#                   (Claude Code セッションは止めない)
#   make help     — 全ターゲット一覧
#
# Note: macOS の make 3.81 でも動くよう `.ONESHELL` には依存していない。
#       マルチライン処理は `\` + `;` でつないでいる。

SHELL := /bin/bash

# Project-root .env を最初に読み込む。`?=` のデフォルトより優先される
# ように `include` で取り込み、`export` で全変数を子プロセスに渡す。
# .env が無い場合は `-include` の `-` で黙って継続。
-include .env
# bot.mode は configs/bot_config.yaml が SSOT(BOT_MODE env は読まれない)。
export GMO_API_KEY GMO_API_SECRET DATABASE_URL CLAUDE_CLI_PATH \
       EVENT_CALENDAR_PATH DASHBOARD_ALLOWED_HOSTS BACKTEST_DATABASE_URL BOT_PROJECT_ROOT \
       LIVE_TRADING_ENABLED LIVE_CONFIRM_SYMBOL LIVE_CONFIRM_SYMBOLS \
       BOT_CONFIG_PATH HARD_LIMITS_PATH ACTIVE_CONFIG_PATH NEXT_CONFIG_PATH \
       PROMPT_PATH SUMMARY_INPUT_PATH AI_OUTPUT_DIR EMERGENCY_FLAG_PATH \
       MIGRATIONS_DIR API_ADDR \
       DASHBOARD_USER DASHBOARD_PASS DASHBOARD_ALLOWED_ORIGINS DASHBOARD_AUTH_DISABLE \
       LOG_LEVEL \
       BOT_DEBUG_FORCE_ADVISOR INTEGRATION_TEST_DB_URL

# 以下はフォールバックのデフォルト値。.env で定義されていればそちらが優先される。
DATABASE_URL ?= postgres://fxbot:fxbot@localhost:5432/fxbot?sslmode=disable

# bot に渡す API バインドアドレス。frontend の next.config.js も localhost:8080 を見る。
API_ADDR ?= 127.0.0.1:8080

# frontend dev サーバの port。3000 が他プロセスで詰まっている時は
# `make start FE_PORT=3001` のように上書きできる。
FE_PORT ?= 3000

# go.mod は backend/ にあるので、`go run` 系は cd backend して走らせる。
# その結果 CWD が backend/ になるので、bot が configs/* を読みに行く相対パスを
# 一括で `../` 始まりに揃える。
BOT_CONFIG_PATH      ?= ../configs/bot_config.yaml
HARD_LIMITS_PATH     ?= ../configs/hard_limits.yaml
ACTIVE_CONFIG_PATH   ?= ../configs/strategy_config.active.yaml
NEXT_CONFIG_PATH     ?= ../configs/strategy_config.next.yaml
PROMPT_PATH          ?= ../prompts/generate_strategy_config.md
SUMMARY_INPUT_PATH   ?= ../runtime/ai_input/latest_summary.json
AI_OUTPUT_DIR        ?= ../runtime/ai_output
EMERGENCY_FLAG_PATH  ?= ../runtime/emergency_stop.flag
MIGRATIONS_DIR       ?= migrations
CLAUDE_CLI_PATH      ?= claude
# DB backup の保存先 (scripts/db-backup.sh と backup-* / restore-* ターゲットが使う)。
FXBOT_BACKUP_DIR     ?= $(HOME)/.fxbot/backups

.PHONY: help start stop kill-stale \
        docker-up db-up db-down db-logs \
        migrate-up migrate-down migrate-status config-drift \
        backup-now backup-install backup-list restore-drill backup-restore \
        backend frontend \
        test test-cover test-integration vet fmt \
        sqlc-generate \
        build clean clean-runtime \
        check-backend check-frontend check

help: ## 全ターゲット一覧
	@echo "Usage: make <target>"
	@echo ""
	@echo "Common:"
	@echo "  make start          — postgres + migrate + bot + frontend を一括起動 (古いプロセスは先に kill)"
	@echo "  make stop           — bot/frontend を停止し postgres コンテナを down、Docker Desktop も終了"
	@echo "  make kill-stale     — port 8080/3000 を握っている古いプロセスを落とすだけ"
	@echo ""
	@echo "Individual:"
	@echo "  make db-up          — postgres を起動 (healthcheck 待ち)"
	@echo "  make db-down        — postgres を停止"
	@echo "  make db-logs        — postgres ログを tail"
	@echo "  make migrate-up     — DB マイグレーション適用"
	@echo "  make migrate-down   — DB マイグレーションを 1 つ revert"
	@echo "  make migrate-status — 適用状態を表示"
	@echo "  make backend        — bot のみ foreground 起動"
	@echo "  make frontend       — frontend のみ foreground 起動"
	@echo ""
	@echo "Dev:"
	@echo "  make test           — go test -race -count=1 ./..."
	@echo "  make test-cover     — カバレッジ付き"
	@echo "  make vet            — go vet ./..."
	@echo "  make fmt            — go fmt ./..."
	@echo "  make build          — bin/bot, bin/migrate を出力"
	@echo "  make clean          — bin/, .run/ を削除"
	@echo "  make clean-runtime  — runtime/ai_input, runtime/ai_output, emergency_stop.flag を削除"

# ---------------------------------------------------------------------------
# postgres
# ---------------------------------------------------------------------------

docker-up: ## Docker Desktop が落ちていれば起動して daemon の応答を待つ
	@if ! docker info >/dev/null 2>&1; then \
	  echo "==> docker daemon is down, launching Docker Desktop..."; \
	  open -a Docker; \
	  for i in $$(seq 1 60); do \
	    docker info >/dev/null 2>&1 && break; \
	    sleep 1; \
	  done; \
	  if ! docker info >/dev/null 2>&1; then \
	    echo "!! docker did not become ready in 60s"; exit 1; \
	  fi; \
	  echo "==> docker daemon ready"; \
	fi

db-up: docker-up ## postgres を起動して healthcheck を待つ
	docker compose up -d postgres
	@echo "==> waiting for postgres healthcheck..."
	@for i in $$(seq 1 30); do \
	  if docker compose ps postgres 2>/dev/null | grep -q healthy; then \
	    echo "==> postgres ready"; exit 0; \
	  fi; sleep 1; \
	done; \
	echo "!! postgres did not become healthy in 30s"; exit 1

db-down:
	docker compose down

db-logs:
	docker compose logs -f postgres

# ---------------------------------------------------------------------------
# migrations
# ---------------------------------------------------------------------------

migrate-up: db-up
	cd backend && go run ./cmd/migrate up

migrate-down:
	cd backend && go run ./cmd/migrate down

config-drift: ## 指定 YAML と DB の active raw_yaml の差分を検出 (read-only SELECT)。例: make config-drift FILE=configs/trend_v4_USD_JPY.yaml
	@# コメント/空行を除いて比較する。seed 後に YAML を変えて未 re-seed なら DRIFT として出る。
	@f="$(FILE)"; \
	 if [ -z "$$f" ] || [ ! -f "$$f" ]; then echo "usage: make config-drift FILE=configs/<name>.yaml" 1>&2; exit 2; fi; \
	 id=$$(sed -nE 's/^config_id:[[:space:]]*"?([^"#[:space:]]+)"?.*/\1/p' "$$f" | head -1); \
	 if [ -z "$$id" ]; then echo "no top-level config_id in $$f" 1>&2; exit 2; fi; \
	 tmp=$$(mktemp); trap 'rm -f "$$tmp"' EXIT; \
	 docker exec fxbot-postgres psql -U fxbot -d fxbot -tAc \
	   "SELECT raw_yaml FROM strategy_configs WHERE config_id='$$id' AND status='active'" > "$$tmp" 2>/dev/null || true; \
	 if [ ! -s "$$tmp" ]; then echo "(no active DB row for $$id)"; exit 0; fi; \
	 if diff <(grep -vE '^[[:space:]]*#|^[[:space:]]*$$' "$$f") <(grep -vE '^[[:space:]]*#|^[[:space:]]*$$' "$$tmp"); then \
	   echo "OK    $$id == $$f"; \
	 else \
	   echo "DRIFT $$id vs $$f (re-seed: bash scripts/seed_active_config.sh --apply $$f)"; exit 1; \
	 fi

migrate-status:
	cd backend && go run ./cmd/migrate status

# ---------------------------------------------------------------------------
# DB backup (scripts/db-backup.sh。保存先は FXBOT_BACKUP_DIR、既定 ~/.fxbot/backups)
# launchd で定期実行する場合は make backup-install で ~/.fxbot/ にコピーしたものを使う
# (macOS TCC は launchd から ~/Desktop 配下のスクリプトを実行させない)。
# ---------------------------------------------------------------------------

backup-now: ## fxbot DB を pg_dump して $(FXBOT_BACKUP_DIR) に保存 (手動 1 回)
	FXBOT_BACKUP_DIR="$(FXBOT_BACKUP_DIR)" bash scripts/db-backup.sh

backup-install: ## launchd 用に scripts/db-backup.sh を ~/.fxbot/ へコピー
	mkdir -p $(HOME)/.fxbot/logs
	install -m 0755 scripts/db-backup.sh $(HOME)/.fxbot/db-backup.sh
	@echo "==> installed $(HOME)/.fxbot/db-backup.sh (launchd の plist はこれを指す)"

backup-list: ## $(FXBOT_BACKUP_DIR) にある dump 一覧 (新しい順)
	@ls -lhrt "$(FXBOT_BACKUP_DIR)"/ 2>/dev/null | tail -30 || echo "(no backups yet)"

restore-drill: ## 最新 backup を fxbot_test へ復元して件数検証 (live fxbot には触れない)
	@# 復元先は fxbot_test 固定。live (fxbot) は絶対に対象にしない。
	@latest=$$(ls -1t "$(FXBOT_BACKUP_DIR)"/fxbot-*.sql.gz 2>/dev/null | head -1); \
	 if [ -z "$$latest" ]; then echo "no backup found in $(FXBOT_BACKUP_DIR)/" 1>&2; exit 1; fi; \
	 echo "==> restore-drill: $$latest → fxbot_test (live fxbot は不可侵)"; \
	 docker exec fxbot-postgres psql -U fxbot -d postgres -v ON_ERROR_STOP=1 \
	   -c "DROP DATABASE IF EXISTS fxbot_test; CREATE DATABASE fxbot_test;" >/dev/null; \
	 gunzip -c "$$latest" | docker exec -i fxbot-postgres psql -U fxbot -d fxbot_test >/dev/null 2>&1 || true; \
	 trades=$$(docker exec fxbot-postgres psql -U fxbot -d fxbot_test -tAc "SELECT count(*) FROM trades" 2>/dev/null | tr -d '[:space:]'); \
	 if [ -n "$$trades" ] && [ "$$trades" -ge 0 ] 2>/dev/null; then \
	   echo "==> restore-drill OK: fxbot_test に復元、trades=$$trades 件 (backup は復元可能)"; \
	 else \
	   echo "!! restore-drill FAILED: fxbot_test の trades を読めません — backup が壊れている可能性" 1>&2; exit 1; \
	 fi

backup-restore: ## 最新 dump で fxbot DB を上書き復元 (--clean 付き plain dump を psql で流し込む・対話確認必須)
	@latest=$$(ls -1t "$(FXBOT_BACKUP_DIR)"/fxbot-*.sql.gz 2>/dev/null | head -1); \
	 if [ -z "$$latest" ]; then echo "no backup found in $(FXBOT_BACKUP_DIR)/" 1>&2; exit 1; fi; \
	 if curl -s -m 2 -o /dev/null http://$(API_ADDR)/healthz 2>/dev/null; then \
	   echo "==> ABORT: bot が稼働中 ($(API_ADDR)/healthz 応答あり)。復元は bot 停止後に。" 1>&2; \
	   echo "    bot の停止は人間が行うこと (CLAUDE.md: AI は bot を止めない)。" 1>&2; \
	   exit 1; \
	 fi; \
	 echo "==> restore target: $$latest"; \
	 echo "==> this will DROP and recreate ALL fxbot data."; \
	 printf "==> 続行するには RESTORE と入力 (それ以外で中止): "; \
	 if ! read -r answer </dev/tty; then \
	   echo "no interactive tty — aborting (人間の対話確認が必須)" 1>&2; exit 1; \
	 fi; \
	 if [ "$$answer" != "RESTORE" ]; then echo "aborted" 1>&2; exit 1; fi; \
	 gunzip -c "$$latest" | docker exec -i fxbot-postgres psql -U fxbot -d fxbot; \
	 echo "==> restore complete from $$latest"

# ---------------------------------------------------------------------------
# individual processes
# ---------------------------------------------------------------------------

backend: ## bot だけ foreground 起動
	cd backend && go run ./cmd/bot

frontend: ## frontend だけ foreground 起動 (初回は npm install も走る)
	@cd frontend && \
	 if [ ! -d node_modules ]; then \
	   echo "==> installing frontend deps (first run)"; \
	   npm install --silent --no-audit --no-fund; \
	 fi; \
	 npm run dev -- -p $(FE_PORT)

# ---------------------------------------------------------------------------
# orchestration: make start / make stop
# ---------------------------------------------------------------------------

kill-stale: ## 古い bot/frontend プロセスや port 8080/3000 占有プロセスを落とす (OPEN ポジ時は FORCE=1 必須)
	@# 0) OPEN/CLOSING の live ポジションがある間は FORCE=1 を要求する。
	@#    bot を止めると主出口 ratchet/MaxHold が無効化されるため、誤 kill を防ぐ。read-only SELECT のみ。
	@open=$$(docker exec fxbot-postgres psql -U fxbot -d fxbot -tAc \
	   "SELECT count(*) FROM positions WHERE status IN ('OPEN','CLOSING')" 2>/dev/null | tr -d '[:space:]'); \
	 if [ -n "$$open" ] && [ "$$open" != "0" ]; then \
	   if [ "$(FORCE)" != "1" ]; then \
	     echo "!! $$open 件の OPEN/CLOSING ポジションがあります。"; \
	     echo "!! bot を止めると ratchet/MaxHold が効かなくなり、遠い SL まで往復し得ます。"; \
	     echo "!! 意図的に止めるなら: make kill-stale FORCE=1  (または make start FORCE=1)"; \
	     exit 1; \
	   fi; \
	   echo "==> FORCE=1: OPEN/CLOSING $$open 件あるが続行します"; \
	 fi
	@# 1) .run/*.pid に記録されている前回のプロセス
	@if [ -d .run ]; then \
	  for f in .run/*.pid; do \
	    [ -f "$$f" ] || continue; \
	    pid=$$(cat "$$f" 2>/dev/null); \
	    if [ -n "$$pid" ] && kill -0 $$pid 2>/dev/null; then \
	      echo "==> killing previous $$f -> $$pid"; \
	      pkill -P $$pid 2>/dev/null || true; \
	      kill $$pid 2>/dev/null || true; \
	    fi; \
	  done; \
	  rm -rf .run; \
	fi
	@# 2) bot api ($(API_ADDR)) と frontend ($(FE_PORT)) を握っているプロセスを落とす
	@bot_port=$$(echo "$(API_ADDR)" | sed 's/.*://'); \
	 for p in $$bot_port $(FE_PORT); do \
	   pids=$$(lsof -tiTCP:$$p -sTCP:LISTEN 2>/dev/null || true); \
	   if [ -n "$$pids" ]; then \
	     echo "==> port $$p busy (pids: $$pids) — terminating"; \
	     kill $$pids 2>/dev/null || true; \
	     for i in 1 2 3 4 5; do \
	       sleep 0.3; \
	       still=$$(lsof -tiTCP:$$p -sTCP:LISTEN 2>/dev/null || true); \
	       [ -z "$$still" ] && break; \
	     done; \
	     still=$$(lsof -tiTCP:$$p -sTCP:LISTEN 2>/dev/null || true); \
	     if [ -n "$$still" ]; then \
	       echo "==> port $$p still busy — SIGKILL $$still"; \
	       kill -9 $$still 2>/dev/null || true; \
	       sleep 0.5; \
	     fi; \
	     final=$$(lsof -tiTCP:$$p -sTCP:LISTEN 2>/dev/null || true); \
	     if [ -n "$$final" ]; then \
	       state=$$(ps -p $$final -o state= 2>/dev/null | tr -d ' '); \
	       echo "!! port $$p still held by $$final (state=$$state)"; \
	       echo "!! kernel-stuck process — reboot needed to free this port"; \
	       echo "!! 回避策: make start FE_PORT=3001  のように違う port を使ってください"; \
	       exit 1; \
	     fi; \
	   fi; \
	 done

start: kill-stale migrate-up ## bot + frontend を一括起動。Ctrl+C で両方止まる。古いプロセスがあれば先に落とす。
	@mkdir -p .run
	@echo "==> starting bot + frontend"
	@echo "==> bot api: http://$(API_ADDR)"
	@echo "==> dashboard: http://localhost:$(FE_PORT)"
	@echo "==> Ctrl+C to stop both"
	@echo ""
	@cd frontend && \
	  if [ ! -d node_modules ]; then \
	    echo "==> [setup] installing frontend deps (first run)"; \
	    npm install --silent --no-audit --no-fund; \
	  fi
	@echo "==> [fe ] clearing .next cache (Ctrl+C kills can corrupt it -> GET / 500)"
	@rm -rf frontend/.next
	@set -m; \
	 mkdir -p "$(CURDIR)/runtime/logs"; : > "$(CURDIR)/runtime/logs/bot_stdout.log"; \
	 ( cd backend && go run ./cmd/bot 2>&1 | tee -a "$(CURDIR)/runtime/logs/bot_stdout.log" | awk '{print "[bot] "$$0; fflush()}' ) & \
	   BOT_PID=$$!; echo $$BOT_PID > .run/bot.pid; \
	   printf '{"bot_pid":%s,"tty":"%s","started_at":"%s","user":"%s","host":"%s"}\n' \
	     "$$BOT_PID" "$$(tty 2>/dev/null || echo unknown)" "$$(date -u +%FT%TZ)" "$$(whoami)" "$$(hostname)" > .run/owner.json; \
	 ( cd frontend && npm run dev -- -p $(FE_PORT) 2>&1 | awk '{print "[fe ] "$$0; fflush()}' ) & \
	   FE_PID=$$!; echo $$FE_PID > .run/fe.pid; \
	 trap 'echo; echo "==> stopping..."; \
	       kill $$BOT_PID $$FE_PID 2>/dev/null || true; \
	       pkill -P $$BOT_PID 2>/dev/null || true; \
	       pkill -P $$FE_PID 2>/dev/null || true; \
	       rm -rf .run; exit 0' INT TERM; \
	 wait

stop: ## bot + frontend + postgres + Docker Desktop を停止 (Claude セッションは止めない)
	@kill_tree() { local p=$$1; for c in $$(pgrep -P $$p 2>/dev/null); do kill_tree $$c; done; kill $$p 2>/dev/null || true; }; \
	 acted=0; \
	 if [ -d .run ]; then \
	   for f in .run/*.pid; do \
	     [ -f "$$f" ] || continue; \
	     pid=$$(cat "$$f" 2>/dev/null); \
	     if [ -n "$$pid" ] && kill -0 $$pid 2>/dev/null; then \
	       echo "==> killing $$pid + descendants (from $$f)"; \
	       kill_tree $$pid; \
	       acted=1; \
	     fi; \
	   done; \
	   rm -rf .run; \
	 fi; \
	 bot_port=$$(echo "$(API_ADDR)" | sed 's/.*://'); \
	 for p in $$bot_port $(FE_PORT); do \
	   pids=$$(lsof -tiTCP:$$p -sTCP:LISTEN 2>/dev/null || true); \
	   if [ -n "$$pids" ]; then \
	     echo "==> port $$p still busy (pids: $$pids) — terminating"; \
	     kill $$pids 2>/dev/null || true; \
	     sleep 0.3; \
	     still=$$(lsof -tiTCP:$$p -sTCP:LISTEN 2>/dev/null || true); \
	     [ -n "$$still" ] && kill -9 $$still 2>/dev/null || true; \
	     acted=1; \
	   fi; \
	 done; \
	 if docker info >/dev/null 2>&1; then \
	   running=$$(docker compose ps -q postgres 2>/dev/null); \
	   if [ -n "$$running" ]; then \
	     echo "==> stopping postgres..."; \
	     docker compose down >/dev/null 2>&1 || true; \
	     acted=1; \
	   fi; \
	   if pgrep -x Docker >/dev/null 2>&1; then \
	     echo "==> quitting Docker Desktop..."; \
	     osascript -e 'quit app "Docker"' >/dev/null 2>&1 || true; \
	     acted=1; \
	   fi; \
	 else \
	   echo "(docker daemon not running — skipping postgres / Docker Desktop quit)"; \
	 fi; \
	 if [ "$$acted" = "0" ]; then \
	   echo "nothing to stop (no .run/, no port holders)"; \
	 else \
	   echo "==> stopped"; \
	 fi

# ---------------------------------------------------------------------------
# dev
# ---------------------------------------------------------------------------

test:
	cd backend && go test -race -count=1 ./...

test-cover:
	cd backend && go test -race -count=1 -cover ./...

# Integration tests: `go build -tags integration` で囲まれた *_integration_test.go
# のみ実行。前提:
#   - postgres が起動 (make db-up)
#   - INTEGRATION_TEST_DB_URL=postgres://...?sslmode=disable が export 済み
#     (未設定なら個別 test が t.Skip() するので失敗はしないが意味もない →
#      Makefile target としても "ran nothing" を明示するため WARN を出して
#      実際の go test 呼び出しはスキップする)
#
# Safety: integration suite は全テーブルを空にする。DATABASE_URL と同じ DSN や
# DB 名が _test で終わらない DSN では実行を拒否する (詳細は docs/workflows/TESTING.md §6)。
test-integration:
	@if [ -z "$$INTEGRATION_TEST_DB_URL" ]; then \
	  echo ">>> INTEGRATION_TEST_DB_URL is not set — skipping integration tests."; \
	  echo "    Set it to a *_test DB DSN, e.g.:"; \
	  echo "    export INTEGRATION_TEST_DB_URL='postgres://fxbot:fxbot@localhost:5432/fxbot_test?sslmode=disable'"; \
	  echo "    See docs/workflows/TESTING.md §6 for details."; \
	  exit 0; \
	fi; \
	if [ "$$INTEGRATION_TEST_DB_URL" = "$$DATABASE_URL" ]; then \
	  echo ">>> REFUSING TO RUN: INTEGRATION_TEST_DB_URL is identical to DATABASE_URL." 1>&2; \
	  echo "    The integration suite TRUNCATEs every table — use a separate test DB" 1>&2; \
	  echo "    (e.g. fxbot_test). See docs/workflows/TESTING.md §6." 1>&2; \
	  exit 1; \
	fi; \
	case "$$INTEGRATION_TEST_DB_URL" in \
	  *_test|*_test\?*) : ;; \
	  *) echo ">>> REFUSING TO RUN: INTEGRATION_TEST_DB_URL db name must end in _test." 1>&2; \
	     echo "    got: $$INTEGRATION_TEST_DB_URL — use e.g. .../fxbot_test?sslmode=disable" 1>&2; \
	     exit 1 ;; \
	esac; \
	echo ">>> integration tests against $$INTEGRATION_TEST_DB_URL"; \
	cd backend && go test -race -count=1 -tags=integration ./internal/adapter/...

vet:
	cd backend && go vet ./...

fmt:
	cd backend && go fmt ./...

# sqlc-generate regenerates internal/adapter/repository/dbgen from
# internal/adapter/repository/queries/*.sql using migrations/ as the
# schema source of truth. Run whenever you add/edit a .sql query.
# Requires: `brew install sqlc` (or `go install github.com/sqlc-dev/sqlc/cmd/sqlc@latest`).
sqlc-generate:
	cd backend && sqlc generate

build:
	mkdir -p bin
	cd backend && go build -o ../bin/bot ./cmd/bot
	cd backend && go build -o ../bin/migrate ./cmd/migrate
	@echo "==> built: bin/bot, bin/migrate"

clean:
	rm -rf bin .run

clean-runtime:
	rm -rf runtime/ai_input runtime/ai_output runtime/emergency_stop.flag
	@echo "==> cleaned runtime artefacts"

# ---------------------------------------------------------------------------
# check targets — Claude Code Stop hook / Codex / 手動 で使う統一エントリ
# ---------------------------------------------------------------------------
# check-backend: 既存 test (-race) と vet を compose。build verify は
#   `go build ./...` (package compile check, artefact 無し) を別途呼ぶ。
#   既存 `build` ターゲット (bin/bot 生成) は意図的に使わない。
# check-frontend: Next.js の本番ビルド。ESLint は未設定なので含めない。
# check: backend + frontend 両方を順番に。

check-backend: test vet ## hook 用: test -race + vet + package compile check
	cd backend && go build ./...

check-frontend: ## frontend production build (Next.js)
	cd frontend && npm run build

check: check-backend check-frontend ## backend + frontend 両方
