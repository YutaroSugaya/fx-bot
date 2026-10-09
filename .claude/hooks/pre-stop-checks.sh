#!/usr/bin/env bash
# Claude Code Stop hook: enforces architecture rules + runs full check-backend
# before letting Claude end the turn.
#
# FXBOT_PRESTOP_CHECKS is set to "on" by default via .claude/settings.json env,
# so the hook is always active. To temporarily disable it for one session,
# launch Claude Code with `FXBOT_PRESTOP_CHECKS=off claude`.
#
# Steps (in order):
#   1. Opt-in gate (default on via settings.json).
#   2. Collect changed files, then run a Stop-hook secret scan.
#   3. Collect changed backend Go files and DB migration files
#      (working tree + last commit).
#   4. Quick exit if neither Go nor migration files changed — keeps docs-only
#      sessions fast.
#   5. Static architecture violation checks (grep-based, fast).
#      Violations exit 2 with stderr message pointing at the relevant
#      docs/architecture/layers/*.md so Claude can self-review on next turn.
#   6. DB migration checks: naming, up/down pair, version sequence, docs.
#   7. Layer-doc mapping log: for each changed relevant file, print the docs
#      Claude should consult.
#   8. make check-backend (go test -race + vet + build).
#   9. Extra checks: DB integration tests for DB changes, frontend build for API
#      surface changes.
#
# Exit codes:
#   0 → all checks pass (or no backend Go/migration changes), Claude may stop.
#   2 → block stop, stderr explains why.
set -euo pipefail

# fail-closed: env 未設定でも検査を走らせる。無効化は明示的な FXBOT_PRESTOP_CHECKS=off のみ。
if [ "${FXBOT_PRESTOP_CHECKS:-on}" != "on" ]; then
    exit 0
fi

if [ -z "${CLAUDE_PROJECT_DIR:-}" ]; then
    echo "BLOCKED: CLAUDE_PROJECT_DIR is not set" >&2
    exit 2
fi

cd "${CLAUDE_PROJECT_DIR}" || { echo "cd project failed" >&2; exit 2; }

mkdir -p runtime/logs
log="runtime/logs/pre-stop-checks.log"

{
    echo "=== pre-stop checks $(date +%FT%T%z) ==="
} > "$log"

# ----------------------------------------------------------------------------
# Step 0: hook stdin (session_id / stop_hook_active) + 無限ループガード
# ----------------------------------------------------------------------------
# Stop hook が直せない理由で fail し続けると、exit 2 の度に Claude が再試行し
# セッションが永久に終了できない (業界既知の Stop-hook 無限ループ)。
# 同一セッションで MAX_CONSECUTIVE_BLOCKS 回連続ブロックしたら、警告を残して
# 通過させる (品質ゲートは CI / pre-push が二重化済み)。
hook_stdin="$(cat 2>/dev/null || true)"
session_id=""
stop_hook_active="false"
if command -v jq >/dev/null 2>&1 && [ -n "$hook_stdin" ]; then
    session_id="$(printf '%s' "$hook_stdin" | jq -r '.session_id // empty' 2>/dev/null || true)"
    stop_hook_active="$(printf '%s' "$hook_stdin" | jq -r '.stop_hook_active // false' 2>/dev/null || echo false)"
fi
[ -z "$session_id" ] && session_id="$(printf '%s' "$hook_stdin" | grep -oE '"session_id"[[:space:]]*:[[:space:]]*"[^"]+"' | head -1 | sed -E 's/.*"([^"]+)"$/\1/' || true)"
[ -z "$session_id" ] && session_id="nosession"

MAX_CONSECUTIVE_BLOCKS=3
block_count_file="runtime/logs/pre-stop-block-count.${session_id}"
block_count=0
[ -f "$block_count_file" ] && block_count="$(cat "$block_count_file" 2>/dev/null || echo 0)"

# ブロックして stop を拒否する唯一の経路。連続上限に達したら fail-open with warning。
block_stop() {
    local reason="$1"
    if [ "$stop_hook_active" = "true" ] && [ "$block_count" -ge "$MAX_CONSECUTIVE_BLOCKS" ]; then
        {
            echo "WARNING: pre-stop check '${reason}' は ${block_count} 回連続で fail。"
            echo "無限ループ防止のため通過させる。CI / .githooks/pre-push が最終ゲート。"
            echo "未解決の失敗内容は $log を参照。"
        } | tee -a "$log" >&2
        rm -f "$block_count_file"
        exit 0
    fi
    echo "$((block_count + 1))" > "$block_count_file"
    echo "BLOCKED: $reason" >&2
    exit 2
}

# ----------------------------------------------------------------------------
# Step 1: collect changed files (working tree + session 内の全 commit)
# ----------------------------------------------------------------------------
# 最初の Stop 時の HEAD をセッション開始 SHA として記録し、session-start..HEAD 全体を
# 検査範囲にする(セッション内で複数 commit しても古い commit が検査を逃れない)。
session_sha_file="runtime/logs/session-start-sha.${session_id}"
if [ ! -f "$session_sha_file" ]; then
    git rev-parse HEAD > "$session_sha_file" 2>/dev/null || true
fi
session_start_sha="$(cat "$session_sha_file" 2>/dev/null || true)"
session_range_files=""
if [ -n "$session_start_sha" ] && git cat-file -e "${session_start_sha}^{commit}" 2>/dev/null; then
    session_range_files=$(git diff --name-only "${session_start_sha}..HEAD" 2>/dev/null || true)
fi

changed_files=$( {
    git diff --name-only HEAD 2>/dev/null
    git diff --name-only HEAD~..HEAD 2>/dev/null
    echo "$session_range_files"
    git ls-files --others --exclude-standard 2>/dev/null
} | sed '/^$/d' | sort -u || true )

changed_go=$(echo "$changed_files" | grep -E '\.go$' | grep '^backend/' || true)
changed_migration_files=$(echo "$changed_files" | grep -E '^backend/migrations/' || true)
changed_db_go=$(echo "$changed_go" | grep -E '^backend/internal/adapter/repository/|^backend/internal/port/repository\.go$|^backend/cmd/migrate/' || true)
changed_api_go=$(echo "$changed_go" | grep -E '^backend/internal/app/handler/|^backend/internal/usecase/query/' || true)

# ----------------------------------------------------------------------------
# Step 2: secret scan for all changed files, including docs/config-only sessions
# ----------------------------------------------------------------------------
secret_patterns=(
    'GMO_API_KEY=[A-Za-z0-9]'
    'GMO_API_SECRET=[A-Za-z0-9]'
    'API_SECRET=[A-Za-z0-9]'
    'AKIA[0-9A-Z]{16}'
    '-----BEGIN [A-Z ]*PRIVATE KEY-----'
    'xox[baprs]-[A-Za-z0-9-]{10,}'
    'ghp_[A-Za-z0-9]{30,}'
    'sk-ant-[A-Za-z0-9_-]{20,}'
    'sk-[A-Za-z0-9]{20,}'
)
secret_errors=()
changed_added_lines=$(git diff --no-color --unified=0 HEAD 2>/dev/null | grep -E '^\+[^+]' || true)

for pat in "${secret_patterns[@]}"; do
    # here-string: `echo | grep -q` だと pipefail 下で大きな diff の echo が SIGPIPE で落ち、検出を素通りする。
    if [ -n "$changed_added_lines" ] && grep -E -q -e "$pat" <<<"$changed_added_lines"; then
        secret_errors+=("added diff contains potential secret pattern: $pat")
    fi
done

while IFS= read -r f; do
    [ -z "$f" ] && continue
    case "$f" in
        .env|*/.env|*.pem|*.key|*.p12)
            if [ -e "$f" ]; then
                secret_errors+=("secret-like file changed: $f")
            fi
            ;;
    esac

    # Untracked files are not part of `git diff HEAD`; scan their current
    # contents directly when they are small text-like files.
    if git ls-files --others --exclude-standard -- "$f" | grep -Fxq "$f" && [ -f "$f" ]; then
        for pat in "${secret_patterns[@]}"; do
            if grep -E -Iq -e "$pat" "$f"; then
                secret_errors+=("untracked file contains potential secret pattern: $f ($pat)")
            fi
        done
    fi
done <<< "$changed_files"

if [ ${#secret_errors[@]} -gt 0 ]; then
    {
        echo
        echo "=== SECRET SCAN FAILED (${#secret_errors[@]}) ==="
        printf '  %s\n' "${secret_errors[@]}"
        echo
        echo "Remove the secret-like content/file before stopping."
    } | tee -a "$log" >&2
    block_stop "potential secret leak"
fi

# ----------------------------------------------------------------------------
# Step 2.5: enforcement 自己防衛 (warn-once 方式)
# ----------------------------------------------------------------------------
# hooks / settings / allowlist / git hooks は agent が編集できてしまう。
# 同一セッション内で変更を検知したら 1 回だけ block して差分の確認を強制する
# (意図的な変更なら次の stop でそのまま通過する。変更セットが変わると再警告)。
enforcement_changed=$(echo "$changed_files" | grep -E '^\.claude/hooks/|^\.claude/settings.*\.json$|^\.githooks/' || true)
if [ -n "$enforcement_changed" ]; then
    enforcement_digest=$(printf '%s' "$enforcement_changed" | /sbin/md5 -q 2>/dev/null || printf '%s' "$enforcement_changed" | md5sum | cut -d' ' -f1)
    enforcement_ack="runtime/logs/enforcement-ack.${session_id}.${enforcement_digest}"
    if [ ! -f "$enforcement_ack" ]; then
        touch "$enforcement_ack"
        {
            echo
            echo "=== ENFORCEMENT FILES CHANGED THIS SESSION (warn-once) ==="
            echo "$enforcement_changed" | sed 's/^/  /'
            echo
            echo "hook / settings / git-hooks への変更を検知。ルール (CLAUDE.md) の"
            echo "弱体化でないか git diff で確認すること。意図的な変更であれば、"
            echo "そのまま再度 stop すれば通過する (このセッション中の再警告なし)。"
        } | tee -a "$log" >&2
        block_stop "enforcement files changed — confirm the diff is intentional"
    fi
    echo "enforcement change acknowledged: $(echo "$enforcement_changed" | tr '\n' ' ')" >> "$log"
fi

# Early exit: no Go or migration changes → skip architecture checks + tests entirely.
# This keeps docs-only / config-only sessions fast (sub-second) while still
# preserving the safety net the moment any backend code or DB migration is touched.
if [ -z "$changed_go" ] && [ -z "$changed_migration_files" ]; then
    echo "no backend Go or migration changes — skip architecture + test checks" >> "$log"
    rm -f "$block_count_file"
    exit 0
fi

{
    echo
    echo "--- changed relevant files (working tree + last commit) ---"
    if [ -n "$changed_go" ]; then
        echo "[go]"
        echo "$changed_go" | sed 's/^/  /'
    fi
    if [ -n "$changed_migration_files" ]; then
        echo "[migrations]"
        echo "$changed_migration_files" | sed 's/^/  /'
    fi
} >> "$log"

# ----------------------------------------------------------------------------
# Step 5: static architecture violation checks
# ----------------------------------------------------------------------------
violations=()
violation_detail=""

add_violation() {
    local msg="$1" files="$2"
    violations+=("$msg")
    violation_detail+="\n  $msg"$'\n'"$(echo "$files" | sed 's/^/    /')"$'\n'
}

# (a) domain must not import I/O frameworks
v=$(grep -rEln '"github.com/jackc/pgx|"net/http"|"log/slog"' backend/internal/domain/ 2>/dev/null || true)
if [ -n "$v" ]; then
    add_violation "domain layer imports forbidden I/O framework → see docs/architecture/layers/domain.md" "$v"
fi

# (b) domain / port must not import upper layers (adapter / app / usecase)
v=$(grep -rEln "fx-bot/backend/internal/adapter|fx-bot/backend/internal/app|fx-bot/backend/internal/usecase" backend/internal/domain/ backend/internal/port/ 2>/dev/null || true)
if [ -n "$v" ]; then
    add_violation "domain/port imports upper layer (adapter/app/usecase) → see docs/architecture/layers/{domain,port}.md" "$v"
fi

# (c) port must not import config (R1 guardrail)
v=$(grep -rln "fx-bot/backend/internal/config" backend/internal/port/ 2>/dev/null || true)
if [ -n "$v" ]; then
    add_violation "port imports config — breaks R1 guardrail → see docs/architecture/layers/port.md §config 依存の禁止" "$v"
fi

# (d) usecase production code must not import concrete adapter packages
v=$(grep -rln "fx-bot/backend/internal/adapter" backend/internal/usecase/ --include='*.go' 2>/dev/null | grep -v '_test.go' || true)
if [ -n "$v" ]; then
    add_violation "usecase production imports adapter directly → see docs/architecture/layers/usecase.md" "$v"
fi

# (e) handler must not import repository adapter
v=$(grep -rln "fx-bot/backend/internal/adapter/repository" backend/internal/app/handler/ 2>/dev/null || true)
if [ -n "$v" ]; then
    add_violation "handler imports repository adapter → see docs/architecture/layers/handler.md" "$v"
fi

# (f) production code must not have "live_config"/"paper_config" string literals
v=$(grep -rln '"live_config"\|"paper_config"' backend/internal/ backend/cmd/ --include='*.go' 2>/dev/null \
    | grep -v '_test.go' \
    | grep -v 'config/bot_config.go' \
    | grep -v 'port/repository.go' || true)
if [ -n "$v" ]; then
    add_violation "production code has \"live_config\"/\"paper_config\" literal — use config.Mode.IsLive() / IsPaper()" "$v"
fi

# (g) cmd/bot/main.go business-logic helpers (heuristic: very long functions/files)
# Soft warning only — informational, not a blocking violation.

# (h) schema DDL must live in backend/migrations, not repository/port code.
if [ -n "$changed_db_go" ]; then
    v=$(git diff --no-color HEAD -- backend/internal/adapter/repository backend/internal/port/repository.go backend/cmd/migrate 2>/dev/null \
        | grep -Ei '^\+[^+].*(create table|alter table|drop table|create index|drop index|add column|comment on|truncate)' || true)
    if [ -n "$v" ]; then
        add_violation "schema DDL added outside backend/migrations → create a paired migration instead" "$v"
    fi
fi

if [ ${#violations[@]} -gt 0 ]; then
    {
        echo
        echo "=== ARCHITECTURE VIOLATIONS (${#violations[@]}) ==="
        printf '%b' "$violation_detail"
        echo
        echo "Re-read the linked docs/architecture/layers/*.md and fix the offending"
        echo "imports / literals before stopping."
    } | tee -a "$log" >&2
    block_stop "architecture rule violations"
fi

{
    echo
    echo "--- architecture rules: OK ---"
} >> "$log"

# ----------------------------------------------------------------------------
# Step 6: DB migration consistency checks
# ----------------------------------------------------------------------------
if [ -n "$changed_migration_files" ]; then
    migration_errors=()
    migration_files=$(find backend/migrations -maxdepth 1 -type f 2>/dev/null | sort || true)
    up_versions=()

    if [ -z "$migration_files" ]; then
        migration_errors+=("backend/migrations has no migration files after changes")
    fi

    while IFS= read -r f; do
        [ -z "$f" ] && continue
        base="${f##*/}"

        if ! [[ "$base" =~ ^[0-9]{4}_[a-z0-9_]+\.(up|down)\.sql$ ]]; then
            migration_errors+=("$f has invalid migration filename; expected NNNN_name.(up|down).sql")
            continue
        fi

        case "$f" in
            *.up.sql)
                pair="${f%.up.sql}.down.sql"
                if [ ! -e "$pair" ]; then
                    migration_errors+=("$f is missing paired down migration: $pair")
                fi
                up_versions+=("${base%%_*}")
                ;;
            *.down.sql)
                pair="${f%.down.sql}.up.sql"
                if [ ! -e "$pair" ]; then
                    migration_errors+=("$f is missing paired up migration: $pair")
                fi
                ;;
        esac
    done <<< "$migration_files"

    if [ ${#up_versions[@]} -gt 0 ]; then
        version_list=$(printf '%s\n' "${up_versions[@]}" | sort)
        unique_versions=$(printf '%s\n' "${up_versions[@]}" | sort -u)

        while IFS= read -r version; do
            [ -z "$version" ] && continue
            count=$(printf '%s\n' "$version_list" | grep -c "^${version}$" || true)
            if [ "$count" -ne 1 ]; then
                migration_errors+=("migration version $version has $count up files; expected exactly 1")
            fi
        done <<< "$unique_versions"

        expected=1
        while IFS= read -r version; do
            [ -z "$version" ] && continue
            actual=$((10#$version))
            if [ "$actual" -ne "$expected" ]; then
                expected_version=$(printf '%04d' "$expected")
                migration_errors+=("migration versions must be contiguous from 0001; expected $expected_version but found $version")
                expected=$((actual + 1))
            else
                expected=$((expected + 1))
            fi
        done <<< "$unique_versions"
    fi

    while IFS= read -r f; do
        [ -z "$f" ] && continue
        [ ! -e "$f" ] && continue
        base="${f##*/}"
        if ! [[ "$base" =~ ^[0-9]{4}_[a-z0-9_]+\.(up|down)\.sql$ ]]; then
            migration_errors+=("$f has invalid migration filename; expected NNNN_name.(up|down).sql")
        fi
    done <<< "$changed_migration_files"

    if ! echo "$changed_files" | grep -qx 'docs/runtime/DATA_MODEL.md'; then
        migration_errors+=("migration changed but docs/runtime/DATA_MODEL.md was not changed")
    fi
    if ! echo "$changed_files" | grep -qx 'docs/workflows/MIGRATIONS.md'; then
        migration_errors+=("migration changed but docs/workflows/MIGRATIONS.md was not changed")
    fi

    {
        echo
        echo "--- migration docs/checks ---"
        echo "Consult: docs/workflows/MIGRATIONS.md, docs/runtime/DATA_MODEL.md, docs/architecture/PR_CHECKLIST.md"
        if echo "$changed_files" | grep -qx 'docs/workflows/MIGRATIONS.md'; then
            echo "MIGRATIONS.md changed: yes"
        else
            echo "MIGRATIONS.md changed: no"
        fi
        if echo "$changed_files" | grep -qx 'docs/runtime/DATA_MODEL.md'; then
            echo "DATA_MODEL.md changed: yes"
        else
            echo "DATA_MODEL.md changed: no"
        fi
    } >> "$log"

    if [ ${#migration_errors[@]} -gt 0 ]; then
        {
            echo
            echo "=== MIGRATION CHECKS FAILED (${#migration_errors[@]}) ==="
            printf '  %s\n' "${migration_errors[@]}"
            echo
            echo "Read docs/workflows/MIGRATIONS.md and update docs/runtime/DATA_MODEL.md."
        } | tee -a "$log" >&2
        block_stop "migration checks failed"
    fi
fi

# ----------------------------------------------------------------------------
# Step 7: changed-files → layer-doc mapping (so Claude knows what to consult)
# ----------------------------------------------------------------------------
if [ -n "$changed_go" ] || [ -n "$changed_migration_files" ]; then
    {
        echo
        echo "--- changed files → relevant docs ---"
        printf '%s\n%s\n' "$changed_go" "$changed_migration_files" | sed '/^$/d' | while IFS= read -r f; do
            case "$f" in
                backend/migrations/*)
                    echo "  $f  → docs/workflows/MIGRATIONS.md, docs/runtime/DATA_MODEL.md, docs/architecture/PR_CHECKLIST.md";;
                backend/internal/app/handler/types.go)
                    echo "  $f  → docs/architecture/layers/handler.md, docs/integrations/API_CONTRACT.md";;
                backend/internal/app/handler/*)
                    echo "  $f  → docs/architecture/layers/handler.md";;
                backend/internal/usecase/command/*)
                    echo "  $f  → docs/architecture/layers/usecase.md (Command 規約)";;
                backend/internal/usecase/query/*)
                    echo "  $f  → docs/architecture/layers/usecase.md (Query 規約), docs/integrations/API_CONTRACT.md";;
                backend/internal/usecase/*)
                    echo "  $f  → docs/architecture/layers/usecase.md";;
                backend/internal/domain/*)
                    echo "  $f  → docs/architecture/layers/domain.md";;
                backend/internal/port/repository.go)
                    echo "  $f  → docs/architecture/layers/port.md, docs/workflows/MIGRATIONS.md, docs/runtime/DATA_MODEL.md";;
                backend/internal/port/*)
                    echo "  $f  → docs/architecture/layers/port.md";;
                backend/internal/adapter/repository/*)
                    echo "  $f  → docs/architecture/layers/adapter.md, docs/workflows/MIGRATIONS.md, docs/runtime/DATA_MODEL.md";;
                backend/internal/adapter/*)
                    echo "  $f  → docs/architecture/layers/adapter.md";;
                backend/internal/safety/*)
                    echo "  $f  → docs/architecture/layers/safety.md";;
                backend/internal/app/*)
                    echo "  $f  → docs/runtime/RUNTIME.md (goroutine/mutex/counters)";;
                backend/internal/config/*)
                    echo "  $f  → docs/runtime/CONFIG.md";;
                backend/cmd/bot/*)
                    echo "  $f  → docs/ARCHITECTURE.md (wiring), docs/runtime/RUNTIME.md";;
                backend/cmd/migrate/*)
                    echo "  $f  → docs/workflows/MIGRATIONS.md, docs/runtime/DATA_MODEL.md";;
                *)
                    echo "  $f  → (no specific layer doc)";;
            esac
        done
    } >> "$log"
fi

# ----------------------------------------------------------------------------
# Step 8: make check-backend (test -race + vet + build)
# ----------------------------------------------------------------------------
{
    echo
    echo "--- make check-backend ---"
} >> "$log"
if ! make check-backend >> "$log" 2>&1; then
    tail -40 "$log" >&2
    block_stop "make check-backend failed"
fi

# ----------------------------------------------------------------------------
# Step 9: targeted checks for API / DB blast radius
# ----------------------------------------------------------------------------
if [ -n "$changed_api_go" ]; then
    {
        echo
        echo "--- make check-frontend (API surface changed) ---"
    } >> "$log"
    if ! make check-frontend >> "$log" 2>&1; then
        tail -40 "$log" >&2
        block_stop "make check-frontend failed after API-surface changes"
    fi
fi

if [ -n "$changed_migration_files" ] || [ -n "$changed_db_go" ]; then
    {
        echo
        echo "--- make test-integration (DB-related changes) ---"
    } >> "$log"
    if [ -z "${INTEGRATION_TEST_DB_URL:-}" ]; then
        {
            echo
            echo "=== DB INTEGRATION CHECK BLOCKED ==="
            echo "DB-related files changed, but INTEGRATION_TEST_DB_URL is not set."
            echo "Start postgres with make db-up, export INTEGRATION_TEST_DB_URL, then rerun:"
            echo "  make test-integration"
        } | tee -a "$log" >&2
        block_stop "DB integration test environment missing"
    fi
    if ! make test-integration >> "$log" 2>&1; then
        tail -40 "$log" >&2
        block_stop "make test-integration failed after DB-related changes"
    fi
fi

rm -f "$block_count_file"
echo "pre-stop checks passed (no architecture/migration violations, tests green)"
exit 0
