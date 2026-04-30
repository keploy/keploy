-- Seed data for the chaos-broken-parser e2e test.
--
-- The harness runs ~100 SELECT statements against this table while a
-- deliberately panicking Postgres parser is wired in front of keploy's
-- proxy. The rows themselves are arbitrary; only that the statements
-- SUCCEED end-to-end is load-bearing — that success is the evidence
-- that the supervisor recovered the panic and the dispatcher fell
-- through to the raw byte relay (globalPassThrough).
CREATE TABLE IF NOT EXISTS chaos_probe (
    id   INTEGER PRIMARY KEY,
    note TEXT    NOT NULL
);

INSERT INTO chaos_probe (id, note) VALUES
    (1, 'panic-firewall-ok'),
    (2, 'passthrough-ok'),
    (3, 'db-conn-survives')
ON CONFLICT (id) DO NOTHING;
