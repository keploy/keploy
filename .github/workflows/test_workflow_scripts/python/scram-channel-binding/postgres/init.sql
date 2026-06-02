-- Schema + seed data for the sample app. Runs via the postgres
-- entrypoint's docker-entrypoint-initdb.d/ hook before pg_hba.conf
-- starts forcing TLS+SCRAM, so we don't have to worry about the
-- init connection itself doing channel binding.

CREATE USER app WITH PASSWORD 'app-secret';

CREATE DATABASE app OWNER app;

\c app

CREATE TABLE users (
    id          SERIAL PRIMARY KEY,
    name        TEXT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

GRANT ALL PRIVILEGES ON TABLE users TO app;
GRANT ALL PRIVILEGES ON SEQUENCE users_id_seq TO app;

INSERT INTO users (name) VALUES
    ('alice'),
    ('bob'),
    ('carol');
