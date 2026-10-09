# CLAUDE.md — fx-bot 絶対ルール(AI 作業の事前制御)

> 毎セッションにロードされる事前制御。機械的 enforcement は `.claude/hooks/`(PreToolUse deny / Stop)と CI。
> 設計契約 = [docs/README.md](docs/README.md) / [docs/architecture/PR_CHECKLIST.md](docs/architecture/PR_CHECKLIST.md)。
> Claude Code はマシンローカルのルール用に gitignore された `CLAUDE.local.md` も読む。存在すればそちらも従う。

これは **live モードで実際の資金を発注できる自動 FX 取引 bot** のリポジトリ。
コードとルールは live で動かす前提で扱う(間違いは金銭損失に直結する)。

## 残すカタストロフ防御(どの戦略でも絶対に外さない)

- **emergency_stop 中は新規エントリー不可**(`runtime/emergency_stop.flag`、再開は `POST /api/emergency-resume`)。
  自動ループは各サイクル冒頭でこれを確認する。
- **live で TP/SL は必ず broker(GMO)側 OCO に置く**。Bot 内 OnTick 監視のみは禁止(bot 死で守りが消える)。
- **同 symbol 同 side の OPEN(external 含む)があれば新規 reject**(ナンピン禁止。cap とは独立)。
- **日次損失 cap / per-trade 損失 cap / スプレッドガード**は維持。
- ポジションは建玉時に `config_id` / TP / SL / MaxHold を**凍結保存**し、その後 playbook/config が
  切り替わっても既存ポジションには影響させない(open 玉の保護)。

## DB / プロセスの破壊禁止(AI 作業時の不変条件)

- **live DB は調査時 read-only**。Claude の調査 SQL は `SELECT` のみ(`.env` の `DATABASE_URL_RO` / ロール fxbot_ro を使う)。
  人間が個別指定した書込のみ `FXBOT_HUMAN_APPROVED_DB_WRITE=1` を前置(deny hook が素の書込を拒否)。
  ※ bot 本体の runtime 書込(trades / positions / playbook 等)は通常動作で別物。
- **`go test -tags integration` は絶対に実行しない**(`truncateAll` で接続先 DB の全テーブルを空にする)。コンパイル確認は `go vet -tags integration` のみ。
  通常の `go test ./...`(タグ無し)は安全。`INTEGRATION_TEST_DB_URL` は必ず `fxbot_test`(末尾 `_test`)。
  integration テストはガード付きの `make test-integration`(`_test` 以外の DB を拒否)だけが流し、DB 関連の変更時は Stop hook が自動で呼ぶ。
- **DB を wipe しない**(`docker compose down -v` / `rm -rf .docker-data/postgres` / `DROP SCHEMA public CASCADE` 禁止)。取引履歴は絶対に残す。
- **postgres / bot プロセスを止めない・再起動しない**(live 稼働中に DB を切ると `/api` が 500 になり dashboard が空になる)。
  調査は read-only、見つけた状態に戻す。再起動が必要なら**人間に依頼して待つ**。

## 開発規律

- **t_wada 流 TDD(Red → Green → Refactor)を厳守**。code-first 禁止。壊れたルールは直してから enforce。
- 層規約: domain 純粋性 / handler→repo 禁止 / usecase は port 経由 / CQRS / R1([docs/architecture/layers/](docs/architecture/layers/))。grep 違反ゼロ。
- migration は命名規則・up/down ペア・連番([docs/workflows/MIGRATIONS.md](docs/workflows/MIGRATIONS.md))。
- PR 前に `make check-backend` を通す([docs/architecture/PR_CHECKLIST.md](docs/architecture/PR_CHECKLIST.md))。
