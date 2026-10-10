# Skill: 再評価間隔 (next_advisor_run_in_minutes)

`strategy_config` を出すたびに、**「次に再評価してほしい分数」** を 1 つ返す。
Go の scheduler はこれを読んで、次の advisor cycle までの待ち時間を決める。

## ルール (10 or 30 の二択)

- **平時 = `30`**
- **急変・経済指標帯・volatile・トレンド明瞭 = `10`** (下記時刻帯テーブル参照)
- **`bot_state.emergency_stop = true`** のときだけ **`0`** を返す (= scheduler が `bot_config.ai_advisor.interval_minutes` に fallback)

それ以外の cooldown (45 / 60 / 90 / 120 など) は **使わない**。no_trade / unclear / consecutive_losses でも 30 のまま。
週末も 30 のまま (= bot 自体が weekdays_only で fire しない)。

**狙い (10 分化)**: 15 分間隔だと急騰の初動に config 更新が追いつかないことがある。
急変・指標帯は 10 分まで詰めて、tier 切替・方向転換・イベント config の延長判断を素早く回す。
ただし 10 分は CLI コストが上がるので、平時 (range / flat) は 30 を厳守し乱発しない。

## 時刻帯テーブル (JST、平日)

`time` (UTC) を JST に変換した曜日 + 時刻で判定。上から順で最初にマッチした帯を採用。

| 帯 | JST 時刻 | next_advisor_run_in_minutes | 根拠 |
|---|---|---|---|
| FOMC / 重要米指標 帯 | **22:00 - 24:00** (毎日) | **10** | NFP, CPI, FOMC, PCE 等 米経済指標の主要発表時刻 |
| 早朝 FOMC 帯 | **03:00 - 04:30** (水曜のみ) | **10** | FOMC 会合の声明・記者会見時刻 |
| イベント窓 (event_calendar) | イベント at の ±窓内 | **10** | policy:breakout の仕込み・追従を素早く回す |
| ロンドンオープン帯 | **16:00 - 17:30** | **10** | 欧州勢の参入で値動き加速 |
| 仲値 (TTM) | **09:50 - 10:05** | **10** | TTM 決定前後の急変リスク |
| 市況 `volatile` のとき | (任意の時刻) | **10** | ATR / spread 急拡大時は速く再評価 |
| **トレンド明瞭時** | (任意の時刻) | **10** | `summary_6h.trend_direction` が `up`/`down` かつ `summary_24h` と一致 → 押し目チャンスを取り逃さない |
| 上記以外 | それ以外の時刻 | **30** | 平時 baseline |

## 出力範囲 / バリデーション

- validator の許容値: **0 または 10〜480** の整数 (それ以外は reject)。この skill では 10 / 30 (emergency_stop 時のみ 0) だけを使う。
- 不明 / 判断不能なら `30` を返す。
- 値の選定理由は `market_regime.reason` 末尾に「(recheck: 10 min, 指標帯)」や「(recheck: 30 min, 平時)」のように 1 句添える。

## scheduler 側の挙動 (参考)

- default interval = `bot_config.ai_advisor.interval_minutes` (tracked config では 30)
- 値 < default interval (例 10) → **event** タグで短縮 fire (AdvisorRunSource = "event")
- 値 == default interval (30) → **auto** タグで通常 fire (AdvisorRunSource = "auto")
- 値 == 0 → default interval に fallback

## サブエージェント呼び出しは不要

この skill は時刻 + market_regime の volatile フラグだけで決まるので、メインが skill 05 出力前に直接判定する。
別タスクは launch しない。
