-- Roll back 0003_ratchet_takeprofit.up.sql.
ALTER TABLE positions
    DROP COLUMN ratchet_armed,
    DROP COLUMN peak_unrealized_pips,
    DROP COLUMN ratchet_giveback_pips,
    DROP COLUMN ratchet_arm_pips;
