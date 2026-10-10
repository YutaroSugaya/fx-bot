# AGENTS.md — fx-bot で作業する全 AI エージェント共通ルール

> このファイルは Codex / その他 AI エージェント向けの入口。Claude Code は [CLAUDE.md](CLAUDE.md) を読む。
> **両者は同一の絶対ルールを共有する。SSOT は [CLAUDE.md](CLAUDE.md)** — 矛盾したら CLAUDE.md が優先。
> 設計契約は [docs/README.md](docs/README.md) / [docs/architecture/PR_CHECKLIST.md](docs/architecture/PR_CHECKLIST.md)。

live モードで実際の資金を発注できる自動 FX 取引 bot。live で動かす前提で扱い、間違いは金銭損失に直結する。下記は会話の許可より優先される。

## 絶対にやらないこと

1. **live DB を書き換えない**。`SELECT` のみ。例外は `make migrate-up` と人間が個別指定した `UPDATE` だけ。
2. **`go test -tags integration` を実行しない**(接続先 DB の全テーブルを truncate する)。`go vet -tags integration` で型確認のみ。
   タグ無し `go test ./...` は安全。`INTEGRATION_TEST_DB_URL` は必ず `_test` DB を指す(integration テストはガード付きの `make test-integration` だけが流す)。
3. **DB を wipe しない**: `docker compose down -v` / `rm -rf .docker-data/postgres` / `DROP SCHEMA ... CASCADE` 禁止。
4. **postgres / bot を止めない・再起動しない**。必要なら人間に依頼して待つ。
5. **検証中の戦略パラメータと active config を動かさない**。retune はオフラインで行い、live 投入は人間判断。
6. **live 発注の TP/SL は必ず broker(GMO)側に置く**。bot 内監視のみ禁止。

## やってよい安全修正

`max_trades` / qty 既定値の安全側への変更 / doc / コスト記録 / migration / backtest / テスト追加。
hook(`.claude/hooks/`・`.claude/settings*.json`・`.githooks/`)は修正案を出すまで。直接の編集は Claude Code の PreToolUse hook が拒否し、適用は人間が行う。

## 開発規律

- **t_wada 流 TDD(Red→Green→Refactor)を厳守**。「壊れたルールは直してから enforce」(順序厳守)。
- 層規約(domain 純粋性 / handler→repo 禁止 / usecase は port 経由 / CQRS / R1)を維持。
- PR 前に `make check-backend`(test -race + vet + build)を通す。
- 機械的 enforcement は `.claude/hooks/`(Stop / PreToolUse)と CI。advisory は本ファイル + CLAUDE.md。
