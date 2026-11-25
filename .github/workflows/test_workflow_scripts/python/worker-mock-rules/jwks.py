# JWKS upstream: GET /.well-known/jwks.json answers one symmetric (oct) key;
# make_token.py signs with the same secret. Standard library only.
import base64, http.server, json, sys

SECRET = b"keploy-per-process-warmup-secret"
K = base64.urlsafe_b64encode(SECRET).rstrip(b"=").decode()
BODY = json.dumps({"keys": [{"kty": "oct", "kid": "k1", "alg": "HS256", "k": K}]}).encode()


class H(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        if self.path != "/.well-known/jwks.json":
            self.send_response(404)
            self.end_headers()
            return
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(BODY)))
        self.end_headers()
        self.wfile.write(BODY)

    def log_message(self, *a):
        pass


if __name__ == "__main__":
    http.server.ThreadingHTTPServer(("0.0.0.0", int(sys.argv[1])), H).serve_forever()
