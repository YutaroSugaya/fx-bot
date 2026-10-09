-- Roll back 0002_early_exit.up.sql.
ALTER TABLE positions
    DROP COLUMN early_exit_target_pips,
    DROP COLUMN early_exit_window_minutes;
