-- night_review.sql — 定点観測クエリ (read-only)
--
-- 「設定を変えた結果、判断の質がどう動いたか」を日次で数値で追うための集計軸。
-- DB の時刻カラムは UTC 保存なので、日付は Asia/Tokyo へ変換して表示する。
--
-- 使い方: scripts/night_review.sh [SINCE]
--   SINCE 省略時は直近 30 時間。例: scripts/night_review.sh '2026-06-02 09:00:00+00'
-- psql 変数 :since (timestamptz) を受け取る。SELECT のみ — DB を一切変更しない。

\set QUIET on
\pset border 2
\timing off

\echo '================ 1. 日次サマリ (JST 日付別: 件数 / 勝率 / pips / JPY) ================'
SELECT
  to_char(closed_at AT TIME ZONE 'Asia/Tokyo', 'YYYY-MM-DD')        AS jst_date,
  count(*)                                                          AS trades,
  count(*) FILTER (WHERE profit_loss_jpy > 0)                       AS wins,
  round(100.0 * count(*) FILTER (WHERE profit_loss_jpy > 0) / count(*), 0) AS win_pct,
  round(sum(profit_loss_pips)::numeric, 1)                          AS pips,
  round(sum(profit_loss_jpy)::numeric, 0)                           AS jpy
FROM trades
WHERE closed_at >= :'since'
GROUP BY 1 ORDER BY 1 DESC;

\echo ''
\echo '================ 2. レジーム別パフォーマンス (誤分類/弱trend の検出) ================'
SELECT
  coalesce(sc.market_regime_type, '(none)')                         AS regime,
  count(*)                                                          AS trades,
  round(100.0 * count(*) FILTER (WHERE t.profit_loss_jpy > 0) / count(*), 0) AS win_pct,
  round(avg(sc.market_regime_confidence)::numeric, 2)               AS avg_conf,
  round(avg(t.profit_loss_pips)::numeric, 1)                        AS avg_pips,
  round(sum(t.profit_loss_jpy)::numeric, 0)                         AS sum_jpy
FROM trades t
JOIN strategy_configs sc ON sc.config_id = t.strategy_config_id
WHERE t.closed_at >= :'since'
GROUP BY 1 ORDER BY trades DESC;

\echo ''
\echo '================ 3. クローズ理由の内訳 (max_hold/SL 偏重を検出) ================'
SELECT
  close_reason,
  count(*)                                                          AS n,
  round(100.0 * count(*) / sum(count(*)) OVER (), 0)                AS pct,
  round(avg(profit_loss_pips)::numeric, 1)                          AS avg_pips,
  round(sum(profit_loss_jpy)::numeric, 0)                           AS sum_jpy
FROM trades
WHERE closed_at >= :'since'
GROUP BY close_reason ORDER BY n DESC;

\echo ''
\echo '================ 4. 利益取りこぼし (peak含み益 vs 確定結果) ================'
-- peak と result の差 = ratchet/early_exit が機能していれば縮むはずの「逃した利益」。
-- max_hold 行で gave_back が大きい = 利益保護が効いていない証拠。
SELECT
  t.close_reason,
  count(*)                                                          AS n,
  round(avg(p.peak_unrealized_pips)::numeric, 1)                    AS avg_peak,
  round(avg(t.profit_loss_pips)::numeric, 1)                        AS avg_result,
  round(avg(p.peak_unrealized_pips - t.profit_loss_pips)::numeric, 1) AS avg_gave_back,
  count(*) FILTER (WHERE p.ratchet_armed)                           AS ratchet_armed_n
FROM trades t
JOIN positions p ON p.id = t.position_id
WHERE t.closed_at >= :'since'
GROUP BY t.close_reason ORDER BY n DESC;

\echo ''
\echo '================ 5. TP 到達性 (設定TP vs 実際に届いた peak) ================'
-- avg_peak が avg_tp に遠く届かない = TP が値動きに対して過大 (届かない設定)。
-- tp_reach_pct = peak が TP の何%まで届いたか。低いほど TP 過大。
SELECT
  count(*)                                                          AS trades,
  round(avg(p.take_profit_pips)::numeric, 1)                        AS avg_tp,
  round(avg(p.stop_loss_pips)::numeric, 1)                          AS avg_sl,
  round(avg(p.peak_unrealized_pips)::numeric, 1)                    AS avg_peak,
  round((100.0 * avg(p.peak_unrealized_pips) / nullif(avg(p.take_profit_pips), 0))::numeric, 0) AS tp_reach_pct,
  count(*) FILTER (WHERE t.close_reason = 'take_profit')            AS tp_hits
FROM trades t
JOIN positions p ON p.id = t.position_id
WHERE t.closed_at >= :'since';

\echo ''
\echo '================ 6. シグナル棄却の内訳 (何がスロットルか) ================'
SELECT
  -- 末尾の可変部 (until 時刻 / count=N) を削って理由をグルーピング
  regexp_replace(regexp_replace(reason, ' until .*$', ''), ' \(count=[0-9]+\)$', '') AS reason_group,
  count(*) AS n
FROM signal_rejections
WHERE created_at >= :'since'
GROUP BY 1 ORDER BY n DESC LIMIT 15;
