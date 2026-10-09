-- Ratchet TP (trailing take-profit)。未実現損益の peak を
-- OnTick で追跡し、peak が RatchetArmPips に達した後、peak から
-- RatchetGivebackPips だけ戻ったら MARKET close する。
-- 「伸びている間は持ち、勢いが死んだら確定」を機械化。
--
-- 4 列追加:
--   ratchet_arm_pips, ratchet_giveback_pips
--     → config snapshot。
--       Insert 時に active config の Exit セクションから凍結保存し、その後
--       active config が切り替わっても既存 position の動作は変えない。
--   peak_unrealized_pips, ratchet_armed
--     → runtime state。OnTick で UpdatePositionRatchetState が更新する。
--
-- すべて DEFAULT 0/false 付き = feature OFF。既存 row への影響なし。
-- ratchet_arm_pips=0 ⇔ feature disabled (validator 側で部分指定は禁止)。

ALTER TABLE positions
    ADD COLUMN ratchet_arm_pips      DOUBLE PRECISION NOT NULL DEFAULT 0,
    ADD COLUMN ratchet_giveback_pips DOUBLE PRECISION NOT NULL DEFAULT 0,
    ADD COLUMN peak_unrealized_pips  DOUBLE PRECISION NOT NULL DEFAULT 0,
    ADD COLUMN ratchet_armed         BOOLEAN          NOT NULL DEFAULT false;
