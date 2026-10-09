-- Early-exit MaxHold policy. In the final
-- early_exit_window_minutes BEFORE the soft MaxHold deadline, the bot
-- closes the position as soon as unrealized_pips >= early_exit_target_pips.
-- This caps the worst-case downside a fixed deadline force-close locks in
-- (a position can otherwise sit slightly red until the soft deadline closes it).
--
-- Both columns snapshot from active config at entry time (open positions
-- keep their entry-time exit settings even if the active config later
-- changes). Default 0 = feature disabled,
-- preserving the original MaxHold-only behavior for existing rows and
-- any new row whose source config omits the fields.

ALTER TABLE positions
    ADD COLUMN early_exit_window_minutes INTEGER NOT NULL DEFAULT 0,
    ADD COLUMN early_exit_target_pips    DOUBLE PRECISION NOT NULL DEFAULT 0;
