-- Trailing STOP (損切り側 ratchet)。利確 ratchet (0003) の鏡像。
-- 含み損の trough (最悪値) を OnTick で追跡し、trough が -ratchet_arm_pips に
-- 達した後 (loss_ratchet_armed=true)、trough から ratchet_giveback_pips だけ
-- 戻ったら "ratchet_stoploss" で MARKET close する。「深い含み損から少し戻したら
-- 浅い傷で撤退」= broker OCO の満額 SL を待たずに損失を圧縮する。
--
-- arm/giveback は利確側と同じ ratchet_arm_pips / ratchet_giveback_pips を共用
-- する (同 pips の mirror) ため新しい snapshot 列は足さない。追加するのは
-- runtime state の 2 列のみ:
--   trough_unrealized_pips → OnTick で monotonic decreasing に追う最悪損 (pips)。
--   loss_ratchet_armed     → trough が -arm に達したら true。
--
-- どちらも DEFAULT 0/false = OFF 相当。既存 row への影響なし。
-- ratchet_arm_pips=0 の position は利確側同様に両側 disabled。
ALTER TABLE positions
    ADD COLUMN trough_unrealized_pips DOUBLE PRECISION NOT NULL DEFAULT 0,
    ADD COLUMN loss_ratchet_armed     BOOLEAN          NOT NULL DEFAULT false;
