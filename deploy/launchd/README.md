# deploy/launchd — bot 常駐化 + 外部死活監視 (macOS)

> ⚠️ **このインストールは人間が手動で実行する**。AI エージェントに稼働中の bot を再起動させない。
> `launchctl load` で bot を常駐させる前に、手動起動中の bot を止める必要がある。**OPEN ポジが無い時間帯に**
> 実施すること。

## なぜ必要か

ratchet (トレーリング出口) と MaxHold は **bot プロセスが生きていることが前提** の出口。
terminal から `make start` で手動起動しているだけだと supervisor が無く、bot が落ちたまま気付かないことがある。
ratchet が armed になった後に bot が死ぬと利確 floor (peak − giveback) が消え、broker 側の静的 OCO
(遠い SL / TP) まで値が往復し得る。broker 側 OCO が最後の守りとして残るので損失は SL で止まるが、
取れていたはずの利益を失う。

## 常駐の方式は 2 つ — どちらか一方だけ load する

| 方式 | plist | 動かすもの |
|---|---|---|
| bot だけ常駐 | `deploy/launchd/com.fxbot.bot.plist`(`scripts/bot-supervisor.sh`) | bot のみ。KeepAlive でクラッシュ時に再起動する (正常 exit では再起動しない。再起動間隔は最低 30 秒)。postgres と dashboard は別に起動しておく |
| スタック一括 | `tools/launchd/com.fxbot.stack.plist`(`tools/launchd/fxbot-stack.sh`) | postgres コンテナ + `make start`(bot + dashboard)。ログインやプロセス死のあとに丸ごと戻す |

**`com.fxbot.bot` と `com.fxbot.stack` を両方 load しない**。どちらも bot を起動するので、片方の
`make start`(kill-stale)がもう片方の bot を落とし、supervisor がポートの空きを待って起動し直す、という取り合いになる。

どちらの方式とも併用できるもの:

- `com.fxbot.healthcheck.plist` … 5 分ごとに `scripts/bot-healthcheck.sh` を実行し、bot の `/healthz` (BasicAuth の外にある
  liveness endpoint) が 200 + `ok` 応答を返さなければ macOS 通知 (と、`HEALTHCHECK_WEBHOOK` があれば webhook) を出す。read-only・DB には触らない。
- `com.fxbot.night-review.plist` … 夜間の read-only レポート (下記)。

## repo の置き場所 (macOS の TCC)

launchd から起動したプロセスは、macOS のプライバシー保護 (TCC) により `~/Desktop`・`~/Documents`・`~/Downloads`
配下のファイルを実行・読み取りできないことがある。**repo はこれらの外に clone する** (例: `~/src/fx-bot`)。
下の手順はスクリプトを `~/.fxbot/` にコピーしてスクリプト自体の実行は回避するが、bot は repo の `.env` / `configs/` /
`backend/` を読むので、repo が保護されたフォルダにあると失敗しうる。

plist 内のパスはプレースホルダ (`/Users/USERNAME/...`、repo は `/Users/USERNAME/Desktop/fx-bot`)。
下の `sed` で repo の実際の場所と自分の `$HOME` に置き換える (repo root で実行する)。

## インストール手順: bot だけ常駐 (人間が手動)

```sh
# repo root で実行する
REPO="$(pwd)"

# 1) スクリプトを ~/.fxbot/ にコピー
mkdir -p ~/.fxbot/logs
cp scripts/bot-supervisor.sh scripts/bot-healthcheck.sh ~/.fxbot/
chmod +x ~/.fxbot/bot-supervisor.sh ~/.fxbot/bot-healthcheck.sh

# 2) plist のプレースホルダを repo の場所と $HOME に置き換えてコピー
for f in com.fxbot.bot com.fxbot.healthcheck; do
  sed -e "s#/Users/USERNAME/Desktop/fx-bot#$REPO#g" -e "s#/Users/USERNAME#$HOME#g" \
    "deploy/launchd/$f.plist" > ~/Library/LaunchAgents/$f.plist
done
plutil -lint ~/Library/LaunchAgents/com.fxbot.bot.plist ~/Library/LaunchAgents/com.fxbot.healthcheck.plist

# 3) 死活監視だけ先に有効化 (bot には触らない)
launchctl load ~/Library/LaunchAgents/com.fxbot.healthcheck.plist

# 4) bot 常駐化は OPEN ポジが無いことを確認してから:
#    手動起動中の `make start` を Ctrl+C で止め、
launchctl load ~/Library/LaunchAgents/com.fxbot.bot.plist
```

`bot-supervisor.sh` は `.env` を読み込んでから `backend/` で `go run ./cmd/bot` を起動し、ログを `~/.fxbot/logs/` に書く。
bot の API ポート (既定 8080) が既に LISTEN 中なら別の bot が動いているとみなし、空くまで待つ (二重起動 = 二重発注の防止)。
postgres は起動しないので、先に `make db-up` しておく (compose の `restart: unless-stopped` により Docker の再起動では戻るが、
`make stop` の `docker compose down` のあとは戻らない)。

## インストール手順: スタック一括 (人間が手動)

```sh
# repo root で実行する
REPO="$(pwd)"
sed -e "s#/Users/USERNAME/Desktop/fx-bot#$REPO#g" -e "s#/Users/USERNAME#$HOME#g" \
  tools/launchd/com.fxbot.stack.plist > ~/Library/LaunchAgents/com.fxbot.stack.plist
# plist の EnvironmentVariables.FXBOT_REPO も上の置換で repo の場所になる
mkdir -p ~/.fxbot/logs
plutil -lint ~/Library/LaunchAgents/com.fxbot.stack.plist
launchctl load ~/Library/LaunchAgents/com.fxbot.stack.plist
```

- load した瞬間と、以後のログインのたびに起動する。**その時点の bot_config / `.env` のまま起動する** (live の設定なら live で動く)。
- Docker Desktop はログイン時に起動する設定にしておく (スクリプトは daemon を最大 5 分待つ)。
- 中身は `make start` なので、ポート 8080 / 3000 を LISTEN しているプロセスを止める。DB に OPEN / CLOSING の建玉があると
  kill-stale で止まり、KeepAlive が再試行を続ける (その間も broker の OCO は残る)。
- ログは `~/.fxbot/logs/stack-launchd.log`(スクリプトが起動前に落ちたときは `stack-launchd.err.log`)。

## 無効化

```sh
launchctl unload ~/Library/LaunchAgents/com.fxbot.bot.plist      # または com.fxbot.stack.plist
launchctl unload ~/Library/LaunchAgents/com.fxbot.healthcheck.plist
```

KeepAlive が付いているので、unload せずにプロセスを kill しても launchd が起動し直す。

## 既知の制限

常駐化しても、bot 側の出口 (ratchet / MaxHold) は bot が生きている間しか効かない。ratchet が arm した時点で broker 側の SL を
建値方向へ引き上げる (GMO の注文変更 API) 仕組みは未実装。それが入るまで、常駐化は最低限の保険にとどまる。

## 夜間の定点観測レポート (com.fxbot.night-review)

毎日 04:30 (ローカル時刻) に read-only の集計レポートを `~/.fxbot/reports/` に生成する
(`scripts/night_review.sql` を postgres コンテナで実行するだけ。DB 書込・config 変更・bot 操作はしない)。

```sh
# repo root で実行する
REPO="$(pwd)"
cp scripts/night-review-cron.sh scripts/night_review.sql ~/.fxbot/
chmod +x ~/.fxbot/night-review-cron.sh
sed -e "s#/Users/USERNAME/Desktop/fx-bot#$REPO#g" -e "s#/Users/USERNAME#$HOME#g" \
  deploy/launchd/com.fxbot.night-review.plist > ~/Library/LaunchAgents/com.fxbot.night-review.plist
launchctl load ~/Library/LaunchAgents/com.fxbot.night-review.plist

# 動作確認 (手動 1 回実行)
~/.fxbot/night-review-cron.sh && ls ~/.fxbot/reports/
```
