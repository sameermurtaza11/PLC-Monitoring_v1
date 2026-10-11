-- =====================================================================
-- PLC Monitoring — migration 003: login accounts and sessions
--
-- ADDS two tables; plc_metadata / pv_metadata / pv_latest / pv_history are
-- not touched. Safe to run more than once.
--
-- Run:  $env:PGCLIENTENCODING = "UTF8"
--       psql -U postgres -d plc_monitoring -f migrations/003_users.sql
--
-- There is deliberately NO default account. Create the first administrator
-- with a password you supply:   go run ./cmd/adduser -user NAME -role admin
--
-- NOTE (Version 01): users.password holds the password in PLAIN TEXT, by
-- decision. All reading and writing of it goes through internal/auth/password.go,
-- so storing a hash later is a change in that one file plus a data migration.
-- =====================================================================

BEGIN;

-- Refuse to continue if a different "users" table is already there, instead
-- of silently mixing layouts or touching someone's data.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM information_schema.tables
               WHERE table_schema = 'public' AND table_name = 'users')
       AND NOT EXISTS (SELECT 1 FROM information_schema.columns
               WHERE table_schema = 'public' AND table_name = 'users' AND column_name = 'password')
    THEN
        RAISE EXCEPTION 'A table "users" with a different layout already exists. Back it up and migrate it by hand; this script will not modify it.';
    END IF;
END $$;

CREATE TABLE IF NOT EXISTS users (
    id          BIGSERIAL    PRIMARY KEY,
    username    VARCHAR(100) NOT NULL UNIQUE,
    password    TEXT         NOT NULL,
    role        VARCHAR(20)  NOT NULL
                CONSTRAINT users_role_check CHECK (role IN ('operator', 'supervisor', 'admin')),
    created_on  TIMESTAMPTZ  NOT NULL DEFAULT now(),
    last_online TIMESTAMPTZ  NULL          -- most recent successful login, NOT presence
);

-- Usernames are unique ignoring case ("Admin" and "admin" are the same
-- account) — login, create and edit all use this rule.
CREATE UNIQUE INDEX IF NOT EXISTS users_username_lower_idx ON users (lower(username));

-- Server-side sessions. The browser holds a random token; only its SHA-256
-- hash is stored here, so a copy of this table cannot be used to log in.
CREATE TABLE IF NOT EXISTS sessions (
    token_hash BYTEA       PRIMARY KEY,
    user_id    BIGINT      NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS sessions_user_idx    ON sessions (user_id);
CREATE INDEX IF NOT EXISTS sessions_expires_idx ON sessions (expires_at);

COMMIT;
