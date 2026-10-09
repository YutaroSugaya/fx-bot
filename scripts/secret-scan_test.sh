#!/usr/bin/env bash
# secret-scan_test.sh — scripts/secret-scan.sh(CI / pre-push)と scripts/pre-commit.sh の自己テスト。
# 一時 git リポジトリに偽の秘密を置き、検出されることを確かめる。偽の値は実行時に連結して作る
# (このファイル自体がスキャナに当たらないように)。
#
#   bash scripts/secret-scan_test.sh
set -uo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
scan="$here/secret-scan.sh"
precommit="$here/pre-commit.sh"
pass=0
fail=0

new_repo() {
  local d
  d="$(mktemp -d)"
  git -C "$d" init -q
  git -C "$d" config user.email t@example.invalid
  git -C "$d" config user.name t
  echo "$d"
}

# expect_scan <want: hit|clean> <label> <content> [file name]
expect_scan() {
  local want="$1" label="$2" content="$3" name="${4:-config.txt}" d got
  d="$(new_repo)"
  printf '%s\n' "$content" > "$d/$name"
  git -C "$d" add "$name"
  git -C "$d" commit -q -m init --no-verify
  if (cd "$d" && bash "$scan" >/dev/null 2>&1); then got=clean; else got=hit; fi
  rm -rf "$d"
  if [ "$got" = "$want" ]; then pass=$((pass + 1)); else fail=$((fail + 1)); echo "FAIL secret-scan: $label (want $want, got $got)"; fi
}

# expect_precommit <want: hit|clean> <label> <file-producer-command>
expect_precommit() {
  local want="$1" label="$2" producer="$3" d got
  d="$(new_repo)"
  (cd "$d" && eval "$producer" > staged.txt)
  git -C "$d" add staged.txt
  if (cd "$d" && bash "$precommit" >/dev/null 2>&1); then got=clean; else got=hit; fi
  rm -rf "$d"
  if [ "$got" = "$want" ]; then pass=$((pass + 1)); else fail=$((fail + 1)); echo "FAIL pre-commit: $label (want $want, got $got)"; fi
}

begin="-----BEGIN"
pk="$begin RSA PRIV""ATE KEY-----"
aws="AK""IA0123456789ABCDEF"
ant="sk-""ant-api03-$(printf 'A%.0s' $(seq 1 60))"
gmo_plain="GMO_API_""KEY=$(printf 'a%.0s' $(seq 1 32))"
gmo_symbols="GMO_API_""KEY=ab+cd/ef$(printf 'Z%.0s' $(seq 1 24))"

expect_scan hit   "private key block"             "$pk"
expect_scan hit   "AWS access key id"             "$aws"
expect_scan hit   "Anthropic key"                 "$ant"
expect_scan hit   "GMO key (alnum)"               "$gmo_plain"
expect_scan hit   "GMO key containing + and /"    "$gmo_symbols"
expect_scan clean "placeholder GMO key"           "GMO_API_""KEY=<your_api_key_here>"
expect_scan clean "empty GMO key"                 "GMO_API_""KEY="
expect_scan hit   "AWS key pasted into a doc"     "$aws" "NOTES.md"
expect_scan hit   "GMO key pasted into .env.example" "$gmo_plain" ".env.example"
expect_scan clean "doc that only names the patterns" "use sk-ant-api03-... and GMO_API_""KEY=<key>" "README.md"

expect_precommit hit   "AWS key at the top of a large diff" "echo '$aws'; seq 1 200000"
expect_precommit hit   "Anthropic key"                      "echo '$ant'"
expect_precommit hit   "private key block"                  "echo '$pk'"
expect_precommit hit   "quoted GMO key"                     "echo 'GMO_API_""KEY=\"$(printf 'a%.0s' $(seq 1 32))\"'"
expect_precommit hit   "GMO secret in YAML form"            "echo 'GMO_API_""SECRET: $(printf 'b%.0s' $(seq 1 32))'"
expect_precommit clean "ordinary file"                      "seq 1 1000"

echo "secret-scan_test: pass=$pass fail=$fail"
[ "$fail" -eq 0 ]
