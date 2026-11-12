# Per-process warm-up app: N worker processes share one port (SO_REUSEPORT),
# like Puma or gunicorn workers. Each verifies a Bearer HS256 JWT with signing
# keys it fetches from a JWKS endpoint on its first request and then keeps:
# which request makes the fetch depends only on which worker it reached.
# Standard library only.
import base64, hashlib, hmac, http.server, json, os, socket, sys, urllib.request

JWKS_URL = os.environ["JWKS_URL"]
keys = None


def b64d(s):
    return base64.urlsafe_b64decode(s + "=" * (-len(s) % 4))


def load_keys(path):
    global keys
    if keys is None:
        # The guard reads which request made each worker's fetch, made or
        # failed.
        print("jwks attempt pid", os.getpid(), "path", path, flush=True)
        try:
            with urllib.request.urlopen(JWKS_URL, timeout=5) as r:
                keys = {k["kid"]: b64d(k["k"]) for k in json.load(r)["keys"]}
            print("jwks loaded pid", os.getpid(), "path", path, flush=True)
        except Exception as e:  # kept unset: the next request fetches again
            print("jwks fetch failed:", e, flush=True)
    return keys or {}


def verify(tok, path):
    h, p, sig = tok.split(".")
    key = load_keys(path).get(json.loads(b64d(h)).get("kid"))
    if key is None:
        return False
    want = hmac.new(key, f"{h}.{p}".encode(), hashlib.sha256).digest()
    return hmac.compare_digest(want, b64d(sig))


class H(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        auth = self.headers.get("Authorization", "")
        ok = auth.startswith("Bearer ") and verify(auth[7:], self.path)
        body = json.dumps({"path": self.path, "authorized": ok}).encode()
        self.send_response(200 if ok else 401)
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
