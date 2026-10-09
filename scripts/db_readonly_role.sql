-- db_readonly_role.sql — AI セッション用 SELECT-only ロール
--
-- 目的: 「live DB は read-only」ルールを credential 層で物理的に強制する。
-- PreToolUse deny hook は literal マッチなので迂回可能だが、このロールには
-- そもそも書込権限が無いため、hook を破っても実害ゼロになる (defense in depth の最下層)。
--
-- 適用 (人間が実行。AI は実行しない — ロール作成は live DB への変更のため):
--   docker exec -i fxbot-postgres psql -U fxbot -d fxbot < scripts/db_readonly_role.sql
--   docker exec -it fxbot-postgres psql -U fxbot -d fxbot -c "\password fxbot_ro"
--
-- 適用後、.env に追記 (パスワードは \password で設定したもの):
--   DATABASE_URL_RO=postgres://fxbot_ro:<password>@localhost:5432/fxbot?sslmode=disable
--
-- 以後の運用ルール (CLAUDE.md): AI の調査 SQL は必ず DATABASE_URL_RO を使う。
-- migration / 人間指定の UPDATE のみ従来の DATABASE_URL を使う。
--
-- 冪等: 再実行しても安全 (IF NOT EXISTS / 再 GRANT は no-op)。
-- DB 名 fxbot・所有ロール fxbot・コンテナ名 fxbot-postgres は docker-compose.yml の既定値。
-- 違う名前で動かしているなら、下の GRANT / ALTER DEFAULT PRIVILEGES と上のコマンドを書き換える。

DO $$
BEGIN
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'fxbot_ro') THEN
        -- パスワードは作成後に人間が \password fxbot_ro で設定する
        CREATE ROLE fxbot_ro LOGIN;
    END IF;
END
$$;

GRANT CONNECT ON DATABASE fxbot TO fxbot_ro;
GRANT USAGE ON SCHEMA public TO fxbot_ro;
GRANT SELECT ON ALL TABLES IN SCHEMA public TO fxbot_ro;
GRANT SELECT ON ALL SEQUENCES IN SCHEMA public TO fxbot_ro;

-- 将来の migration で増えるテーブルにも自動で SELECT を付与
-- (fxbot ロールが作成したオブジェクトに限る = bot の migration 経由は全て対象)。
ALTER DEFAULT PRIVILEGES FOR ROLE fxbot IN SCHEMA public
    GRANT SELECT ON TABLES TO fxbot_ro;
ALTER DEFAULT PRIVILEGES FOR ROLE fxbot IN SCHEMA public
    GRANT SELECT ON SEQUENCES TO fxbot_ro;

-- 検証 (適用後に人間が実行):
--   psql "postgres://fxbot_ro:<pw>@localhost:5432/fxbot?sslmode=disable" \
--     -c "SELECT count(*) FROM trades"                  -- → 成功するはず
--   psql "postgres://fxbot_ro:<pw>@localhost:5432/fxbot?sslmode=disable" \
--     -c "UPDATE positions SET id = id WHERE false"     -- → permission denied になるはず
