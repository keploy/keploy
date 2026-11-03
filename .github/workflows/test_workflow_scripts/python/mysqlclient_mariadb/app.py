# A small HTTP app on mysqlclient (MySQLdb, built on MariaDB Connector/C), as
# Frappe uses it: every request opens its own connection, runs its queries and
# closes it, so each test case holds a full handshake. GET /rows reads the JSON
# column (MariaDB sends its extended type info, format "json", in the column
# definition); GET /plain reads the other columns. GET /healthz touches no
# database. Env: APP_PORT (8096), DB_PORT (3306), DB_PASSWORD.
import json
import os

import MySQLdb
from http.server import BaseHTTPRequestHandler, HTTPServer

APP_PORT = int(os.environ.get("APP_PORT", "8096"))
DB_PORT = int(os.environ.get("DB_PORT", "3306"))
DB_PASSWORD = os.environ.get("DB_PASSWORD", "password")


def query(with_json):
    conn = MySQLdb.connect(host="127.0.0.1", port=DB_PORT, user="root",
                           password=DB_PASSWORD, database="demo", charset="utf8mb4")
    try:
        cur = conn.cursor()
        if with_json:
            cur.execute("SELECT id, name, data, note FROM t ORDER BY id")
        else:
            cur.execute("SELECT id, name, note FROM t ORDER BY id")
        rows = [[None if v is None else str(v) for v in r] for r in cur.fetchall()]
        cur.execute("SELECT COUNT(*) FROM t WHERE name = %s", ("a",))
        return {"rows": rows, "named_a": cur.fetchone()[0]}
    finally:
        conn.close()


class Handler(BaseHTTPRequestHandler):
    def do_GET(self):
        if self.path == "/healthz":
            body = b"ok"
        elif self.path == "/rows":
            body = json.dumps(query(True)).encode()
        elif self.path == "/plain":
            body = json.dumps(query(False)).encode()
        else:
            self.send_error(404)
            return
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *args):
        pass


if __name__ == "__main__":
    HTTPServer(("127.0.0.1", APP_PORT), Handler).serve_forever()
