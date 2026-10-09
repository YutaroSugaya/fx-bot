#!/usr/bin/env bash
# Claude Code Stop hook: enforces code → docs sync.
#
# Direction is the inverse of pre-stop-checks.sh: when source files that
# encode a spec are changed, the corresponding spec doc MUST be updated
# in the same Stop window (uncommitted working tree OR last commit).
#
# Triggers and required docs:
#
#   backend/migrations/*.sql                              → DATA_MODEL.md + MIGRATIONS.md
#   backend/internal/adapter/repository/queries/*.sql     → DATA_MODEL.md + MIGRATIONS.md
#   backend/internal/port/repository.go                   → layers/usecase.md OR layers/port.md
#   backend/internal/domain/position/state.go             → STATE_MACHINE.md
#   backend/internal/safety/*.go                          → layers/safety.md
#   backend/internal/usecase/command/*.go (NEW files)     → layers/usecase.md
#   backend/internal/adapter/<NEW dir>/*.go               → layers/adapter.md
#
# Allowlist: .claude/hooks/spec-sync-allowlist.txt — one regex per line.
# Test files (*_test.go) are auto-excluded.
#
# FXBOT_DOCS_SYNC_CHECKS is set to "on" by default via .claude/settings.json
# env. To temporarily disable: launch with `FXBOT_DOCS_SYNC_CHECKS=off claude`.
#
# Exit codes:
#   0 → no missing docs updates (or hook disabled).
#   2 → block stop, stderr lists the (source file, required doc) pairs.

set -euo pipefail

# fail-closed: pre-stop-checks.sh と同じく、env 未設定でも検査を走らせる
# (無効化は明示的な FXBOT_DOCS_SYNC_CHECKS=off のみ)。
if [ "${FXBOT_DOCS_SYNC_CHECKS:-on}" != "on" ]; then
    exit 0
fi

if [ -z "${CLAUDE_PROJECT_DIR:-}" ]; then
    echo "BLOCKED: CLAUDE_PROJECT_DIR is not set" >&2
    exit 2
fi

cd "${CLAUDE_PROJECT_DIR}" || { echo "cd project failed" >&2; exit 2; }

mkdir -p runtime/logs
log="runtime/logs/docs-sync-check.log"
{
    echo "=== docs-sync check $(date +%FT%T%z) ==="
} > "$log"

# 無限ループガード (pre-stop-checks.sh と同型): 同一セッションで連続ブロックが
# 上限に達したら警告して通過させる。
hook_stdin="$(cat 2>/dev/null || true)"
session_id=""
stop_hook_active="false"
if command -v jq >/dev/null 2>&1 && [ -n "$hook_stdin" ]; then
    session_id="$(printf '%s' "$hook_stdin" | jq -r '.session_id // empty' 2>/dev/null || true)"
    stop_hook_active="$(printf '%s' "$hook_stdin" | jq -r '.stop_hook_active // false' 2>/dev/null || echo false)"
fi
[ -z "$session_id" ] && session_id="nosession"

MAX_CONSECUTIVE_BLOCKS=3
block_count_file="runtime/logs/docs-sync-block-count.${session_id}"
block_count=0
[ -f "$block_count_file" ] && block_count="$(cat "$block_count_file" 2>/dev/null || echo 0)"

block_stop() {
    local reason="$1"
    if [ "$stop_hook_active" = "true" ] && [ "$block_count" -ge "$MAX_CONSECUTIVE_BLOCKS" ]; then
        {
            echo "WARNING: docs-sync check '${reason}' は ${block_count} 回連続で fail。"
            echo "無限ループ防止のため通過させる。docs drift は $log に記録済み。"
        } | tee -a "$log" >&2
        rm -f "$block_count_file"
        exit 0
    fi
    echo "$((block_count + 1))" > "$block_count_file"
    echo "BLOCKED: $reason" >&2
    exit 2
}

# ----------------------------------------------------------------------------
# Step 1: collect changed files (working tree + last commit + untracked)
# ----------------------------------------------------------------------------
changed_files=$( {
    git diff --name-only HEAD 2>/dev/null
    git diff --name-only HEAD~..HEAD 2>/dev/null
    git ls-files --others --exclude-standard 2>/dev/null
} | sort -u || true )

# Strip test files — they shouldn't trigger spec doc updates.
changed_files=$(echo "$changed_files" | grep -vE '_test\.go$' || true)

if [ -z "$changed_files" ]; then
    echo "no source changes — skip docs-sync check" >> "$log"
    rm -f "$block_count_file"
    exit 0
fi

# Apply user-defined allowlist (regex per line).
allowlist=".claude/hooks/spec-sync-allowlist.txt"
if [ -f "$allowlist" ]; then
    while IFS= read -r pattern; do
        # Skip blank lines and comments.
        case "$pattern" in
            ''|\#*) continue ;;
        esac
        # Strip inline comments (everything after first " #").
        pattern="${pattern%% #*}"
        [ -z "$pattern" ] && continue
        # 不正な regex 行は警告して無視する (grep の exit 2 を握り潰すと changed_files が
        # 空になり全検査が素通りするため、regex を先に検証する)。
        set +e
        printf '' | grep -qE -e "$pattern" 2>/dev/null
        regex_rc=$?
        set -e
        if [ "$regex_rc" -ge 2 ]; then
            echo "WARNING: invalid allowlist regex skipped: $pattern" | tee -a "$log" >&2
            continue
        fi
        changed_files=$(echo "$changed_files" | grep -vE "$pattern" || true)
    done < "$allowlist"
fi

if [ -z "$changed_files" ]; then
    echo "all changes allowlisted — skip docs-sync check" >> "$log"
    rm -f "$block_count_file"
    exit 0
fi

# Quick set membership: returns 0 if doc was changed, 1 otherwise.
doc_changed() {
    echo "$changed_files" | grep -qx "$1"
}

# ----------------------------------------------------------------------------
# Step 2: evaluate triggers
# ----------------------------------------------------------------------------
violations=()

add_violation() {
    local src="$1" docs="$2"
    violations+=("  $src")
    violations+=("    → expected docs change: $docs")
}

# (1) migrations: pre-stop-checks.sh already enforces this — keep here as
#     back-stop only for the case where pre-stop is disabled.
mig_changed=$(echo "$changed_files" | grep -E '^backend/migrations/.*\.sql$' || true)
if [ -n "$mig_changed" ]; then
    if ! doc_changed "docs/runtime/DATA_MODEL.md"; then
        while IFS= read -r f; do
            [ -z "$f" ] && continue
            add_violation "$f" "docs/runtime/DATA_MODEL.md"
        done <<< "$mig_changed"
    fi
    if ! doc_changed "docs/workflows/MIGRATIONS.md"; then
        while IFS= read -r f; do
            [ -z "$f" ] && continue
            add_violation "$f" "docs/workflows/MIGRATIONS.md"
        done <<< "$mig_changed"
    fi
fi

# (2) sqlc query files: same docs as migrations (= schema spec).
q_changed=$(echo "$changed_files" | grep -E '^backend/internal/adapter/repository/queries/.*\.sql$' || true)
if [ -n "$q_changed" ]; then
    if ! doc_changed "docs/runtime/DATA_MODEL.md"; then
        while IFS= read -r f; do
            [ -z "$f" ] && continue
            add_violation "$f" "docs/runtime/DATA_MODEL.md (sqlc query = schema spec)"
        done <<< "$q_changed"
    fi
    if ! doc_changed "docs/workflows/MIGRATIONS.md"; then
        while IFS= read -r f; do
            [ -z "$f" ] && continue
            add_violation "$f" "docs/workflows/MIGRATIONS.md (sqlc workflow)"
        done <<< "$q_changed"
    fi
fi

# (3) port/repository.go: interface spec for usecase/handler.
if echo "$changed_files" | grep -qx 'backend/internal/port/repository.go'; then
    if ! doc_changed "docs/architecture/layers/usecase.md" \
        && ! doc_changed "docs/architecture/layers/port.md"; then
        add_violation "backend/internal/port/repository.go" \
            "docs/architecture/layers/usecase.md OR docs/architecture/layers/port.md"
    fi
fi

# (4) position state machine.
if echo "$changed_files" | grep -qx 'backend/internal/domain/position/state.go'; then
    if ! doc_changed "docs/runtime/STATE_MACHINE.md"; then
        add_violation "backend/internal/domain/position/state.go" \
            "docs/runtime/STATE_MACHINE.md"
    fi
fi

# (5) safety package: emergency_stop + timeouts API surface.
safety_changed=$(echo "$changed_files" | grep -E '^backend/internal/safety/.*\.go$' || true)
if [ -n "$safety_changed" ]; then
    # Accept any layers/*.md update that mentions safety — but require at
    # minimum layers/safety.md if it exists.
    if [ -f "docs/architecture/layers/safety.md" ] \
        && ! doc_changed "docs/architecture/layers/safety.md"; then
        while IFS= read -r f; do
            [ -z "$f" ] && continue
            add_violation "$f" "docs/architecture/layers/safety.md"
        done <<< "$safety_changed"
    fi
fi

# (6) NEW usecase command files: layers/usecase.md.
#     "NEW" = untracked file (= not yet in HEAD).
new_usecase=$( git ls-files --others --exclude-standard 2>/dev/null \
    | grep -E '^backend/internal/usecase/command/.*\.go$' \
    | grep -vE '_test\.go$' || true )
if [ -n "$new_usecase" ]; then
    if ! doc_changed "docs/architecture/layers/usecase.md"; then
        while IFS= read -r f; do
            [ -z "$f" ] && continue
            add_violation "$f" "docs/architecture/layers/usecase.md (new command file)"
        done <<< "$new_usecase"
    fi
fi

# (7) NEW adapter subdirectories: layers/adapter.md.
#     Detect a "new adapter dir" as an untracked .go file under a
#     backend/internal/adapter/<subdir>/ where <subdir> didn't have any
#     tracked file before this turn.
new_adapter_files=$( git ls-files --others --exclude-standard 2>/dev/null \
    | grep -E '^backend/internal/adapter/[^/]+/.*\.go$' \
    | grep -vE '_test\.go$' || true )
if [ -n "$new_adapter_files" ]; then
    while IFS= read -r f; do
        [ -z "$f" ] && continue
        # Parent dir of f, e.g. backend/internal/adapter/artifact
        dir=$(dirname "$f")
        # Is there ANY tracked file under that dir already?
        tracked=$(git ls-files -- "$dir" 2>/dev/null || true)
        if [ -z "$tracked" ]; then
            if ! doc_changed "docs/architecture/layers/adapter.md"; then
                add_violation "$f" "docs/architecture/layers/adapter.md (new adapter subdir: $dir)"
            fi
        fi
    done <<< "$new_adapter_files"
fi

# (8) NEW top-level package under backend/internal/: 既存トリガーが知らない
#     新パッケージも検査対象に自動包含する。新設パッケージは
#     アーキテクチャ上の位置づけを docs/ARCHITECTURE.md か layers/*.md に書く。
new_internal_files=$( git ls-files --others --exclude-standard 2>/dev/null \
    | grep -E '^backend/internal/[^/]+/[^/]+\.go$' \
    | grep -vE '_test\.go$' || true )
if [ -n "$new_internal_files" ]; then
    layers_doc_changed=$(echo "$changed_files" | grep -E '^docs/(ARCHITECTURE\.md|architecture/layers/.*\.md)$' || true)
    seen_new_pkgs=""
    while IFS= read -r f; do
        [ -z "$f" ] && continue
        pkg_dir=$(dirname "$f")
        case " $seen_new_pkgs " in *" $pkg_dir "*) continue ;; esac
        seen_new_pkgs="$seen_new_pkgs $pkg_dir"
        tracked=$(git ls-files -- "$pkg_dir" 2>/dev/null || true)
        if [ -z "$tracked" ] && [ -z "$layers_doc_changed" ]; then
            add_violation "$f" "docs/ARCHITECTURE.md or docs/architecture/layers/*.md (new internal package: $pkg_dir)"
        fi
    done <<< "$new_internal_files"
fi

# ----------------------------------------------------------------------------
# Step 3: report
# ----------------------------------------------------------------------------
{
    echo
    echo "--- changed files (post-allowlist, post-test-strip) ---"
    echo "$changed_files" | sed 's/^/  /'
} >> "$log"

if [ ${#violations[@]} -gt 0 ]; then
    {
        echo
        echo "=== SPEC DOC SYNC CHECK BLOCKED ==="
        echo "Source changed but the spec doc that codifies its contract was not"
        echo "updated in this Stop window. Either:"
        echo "  - update the listed doc to reflect the change, OR"
        echo "  - add a regex to .claude/hooks/spec-sync-allowlist.txt if this is"
        echo "    a spec-non-changing edit (typo / log message / formatting)."
        echo
        printf '%s\n' "${violations[@]}"
        echo
    } | tee -a "$log" >&2
    block_stop "docs sync drift"
fi

rm -f "$block_count_file"
echo "docs-sync check passed (no spec/doc drift)" >> "$log"
exit 0
