-- Reverse of 0001_init.up.sql. CASCADE handles the FK / junction-table
-- ordering — no need to drop in dependency order.
-- WARNING: this deletes every table and all data in it (trade history
-- included). Back up the database before running it.

DROP TABLE IF EXISTS candles                          CASCADE;
DROP TABLE IF EXISTS signal_rejections                CASCADE;
DROP TABLE IF EXISTS trade_signals                    CASCADE;
DROP TABLE IF EXISTS trades                           CASCADE;
DROP TABLE IF EXISTS position_state_events            CASCADE;
DROP TABLE IF EXISTS recovered_positions              CASCADE;
DROP TABLE IF EXISTS manual_positions                 CASCADE;
DROP TABLE IF EXISTS positions_live                   CASCADE;
DROP TABLE IF EXISTS positions                        CASCADE;
DROP TABLE IF EXISTS advisor_run_errors               CASCADE;
DROP TABLE IF EXISTS advisor_run_io                   CASCADE;
DROP TABLE IF EXISTS ai_advisor_runs                  CASCADE;
DROP TABLE IF EXISTS market_summaries                 CASCADE;
DROP TABLE IF EXISTS config_validation_events         CASCADE;
DROP TABLE IF EXISTS strategy_config_parse_failures   CASCADE;
DROP TABLE IF EXISTS strategy_config_activations      CASCADE;
DROP TABLE IF EXISTS strategy_config_rejections       CASCADE;
DROP TABLE IF EXISTS strategy_configs                 CASCADE;
