# Per-process schema read: N worker processes share one port (SO_REUSEPORT),
# like Puma or gunicorn workers running an ORM. Each opens its database
# connection and reads the users table's columns (SHOW FULL FIELDS) on its
# first request, then keeps them, as Rails does without a schema cache: which
# request reads the schema depends only on which worker it reached.
# Needs pymysql (pure Python).
import http.server, json, os, socket, sys

import pymysql

conn = None
columns = None


def users_columns(path):
    global conn, columns
    if conn is None:
        conn = pymysql.connect(host=os.environ["DB_HOST"], port=int(os.environ["DB_PORT"]),
                               user="root", password="repro", database="app", autocommit=True)
    if columns is None:
        # The guard reads which request made each worker's schema read, made
        # or failed.
        print("schema attempt pid", os.getpid(), "path", path, flush=True)
        with conn.cursor() as cur:
            cur.execute("SHOW FULL FIELDS FROM `users`")
            columns = [r[0] for r in cur.fetchall()]
        print("schema loaded pid", os.getpid(), "path", path, flush=True)
    return columns


class H(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        try:
            cols = users_columns(self.path)
            uid = int(self.path.rsplit("s", 1)[-1])
            with conn.cursor() as cur:
                # Each request its own row: no read but the schema's repeats
                # across requests, so none but it is a warm-up.
                cur.execute("SELECT name FROM users WHERE id = %s", (uid,))
                row = cur.fetchone()
            body, code = json.dumps({"columns": cols, "name": row[0] if row else None}).encode(), 200
        except Exception as e:  # a schema read that failed is read again next time
            print("request failed:", e, flush=True)
            body, code = json.dumps({"error": str(e)}).encode(), 500
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *a):
        pass


class S(http.server.HTTPServer):
    def server_bind(self):
        self.socket.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEPORT, 1)
        super().server_bind()


if __name__ == "__main__":
    port, workers = int(sys.argv[1]), int(sys.argv[2])
    for _ in range(workers - 1):
        if os.fork() == 0:
            break
    print("worker", os.getpid(), flush=True)
    S(("0.0.0.0", port), H).serve_forever()
