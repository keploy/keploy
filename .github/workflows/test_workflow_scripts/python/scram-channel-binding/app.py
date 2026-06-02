"""
Flask demo app that exercises SCRAM-SHA-256-PLUS auth against Postgres
through psycopg2-binary (libpq + auditwheel-bundled OpenSSL).

The auditwheel bundling is the key reason this app exists as a keploy
e2e fixture: psycopg2-binary ships its own libcrypto-*.so.1.1 under
RTLD_LOCAL with a mangled SONAME, which makes the legacy LD_PRELOAD
channel-binding shim a silent no-op. The BPF cbshim attaches via
uprobe at the ELF-offset level and is unaffected — so this app
demonstrates the exact case where the BPF approach has to work for
SCRAM-PLUS to succeed across keploy's TLS MITM.

Every connection uses `channel_binding=require` so libpq is forced to
pick SCRAM-SHA-256-PLUS — there's no silent fallback to plain SCRAM.
If the cbshim isn't loaded / working, every endpoint that touches the
DB returns HTTP 500 with the postgres "SCRAM channel binding check
failed" message.

Endpoints (chosen to be easy to drive with curl / keploy record):
  GET  /healthz          -> 200 OK, no DB hit
  GET  /db/ping          -> SELECT 1
  GET  /users            -> SELECT * FROM users
  GET  /users/<id>       -> SELECT * FROM users WHERE id=...
  POST /users            -> INSERT INTO users (json body: {"name": "..."})
  GET  /users/<id>/audit -> SELECT now(), version() (mixed types)
"""

import json
import os

import psycopg2
import psycopg2.extras
from flask import Flask, jsonify, request, abort

app = Flask(__name__)


def conninfo():
    """channel_binding=require kills any libpq downgrade to plain SCRAM.
    Without the cbshim, libpq sends the MITM cert's hash to postgres,
    postgres compares to its own real cert's hash, mismatch → FATAL."""
    return (
        f"host={os.environ.get('PGHOST', 'postgres')} "
        f"port={os.environ.get('PGPORT', '5432')} "
        f"dbname={os.environ.get('PGDATABASE', 'app')} "
        f"user={os.environ.get('PGUSER', 'app')} "
        f"password={os.environ.get('PGPASSWORD', 'app-secret')} "
        f"sslmode=require "
        f"channel_binding=require"
    )


def connect():
    return psycopg2.connect(conninfo())


@app.route("/healthz")
def healthz():
    return jsonify(status="ok")


@app.route("/db/ping")
def db_ping():
    with connect() as conn, conn.cursor() as cur:
        cur.execute("SELECT 1 AS pong")
        row = cur.fetchone()
    return jsonify(pong=row[0])


@app.route("/users", methods=["GET"])
def list_users():
    with connect() as conn, conn.cursor(cursor_factory=psycopg2.extras.RealDictCursor) as cur:
        cur.execute("SELECT id, name, created_at FROM users ORDER BY id LIMIT 100")
        rows = cur.fetchall()
    return app.response_class(
        response=json.dumps(rows, default=str),
        mimetype="application/json",
    )


@app.route("/users/<int:user_id>", methods=["GET"])
def get_user(user_id):
    with connect() as conn, conn.cursor(cursor_factory=psycopg2.extras.RealDictCursor) as cur:
        cur.execute(
            "SELECT id, name, created_at FROM users WHERE id = %s",
            (user_id,),
        )
        row = cur.fetchone()
    if row is None:
        abort(404)
    return app.response_class(
        response=json.dumps(row, default=str),
        mimetype="application/json",
    )


@app.route("/users", methods=["POST"])
def create_user():
    body = request.get_json(force=True, silent=True) or {}
    name = body.get("name")
    if not name or not isinstance(name, str):
        abort(400, description="field 'name' (string) required")
    with connect() as conn, conn.cursor() as cur:
        cur.execute(
            "INSERT INTO users (name) VALUES (%s) RETURNING id, created_at",
            (name,),
        )
        new_id, created_at = cur.fetchone()
        conn.commit()
    return jsonify(id=new_id, name=name, created_at=str(created_at)), 201


@app.route("/users/<int:user_id>/audit", methods=["GET"])
def user_audit(user_id):
    """Mixed-type query — exercises libpq's type codecs across the
    SCRAM boundary. Verifies that more than just SELECT 1 works."""
    with connect() as conn, conn.cursor() as cur:
        cur.execute(
            "SELECT %s::int AS user_id, now() AS ts, version() AS pg_version",
            (user_id,),
        )
        user_id_out, ts, ver = cur.fetchone()
    return jsonify(user_id=user_id_out, ts=str(ts), pg_version=ver)


@app.errorhandler(psycopg2.OperationalError)
def handle_pg_op_error(exc):
    """Surface SCRAM channel-binding failures (and any other libpq
    operational error) as a 500 with the underlying message — makes
    the failure mode obvious during keploy record runs."""
    return jsonify(error=str(exc).strip()), 500


if __name__ == "__main__":
    app.run(host="0.0.0.0", port=int(os.environ.get("PORT", "8080")))
