#!/usr/bin/env python3
"""App under test for the repeated-headers guard. Standard library only.

GET /tenant validates the tenant the way a production service did: every
X_tenant line must name a known tenant. A client that sends the header on two
lines (`X_tenant: acme` twice) passes; the same request with its lines folded
into one (`X_tenant: acme,acme`) is rejected with a 400
"Tenant : acme,acme not valid tenant", which is what replay used to send.

GET /login calls the upstream, which answers with two Set-Cookie lines, and
returns the cookies it received. At replay the upstream is gone and the mock
answers instead, so the response only matches when the mock serves both
lines rather than one folded Set-Cookie.
"""
import http.client
import json
import sys
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

PORT = int(sys.argv[1]) if len(sys.argv) > 1 else 8000
UPSTREAM_PORT = int(sys.argv[2]) if len(sys.argv) > 2 else 9092
TENANTS = {"acme", "globex"}


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def _reply(self, status, payload):
        body = json.dumps(payload, sort_keys=True).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        if self.path == "/health":
            return self._reply(200, {"ok": True})
        if self.path == "/tenant":
            lines = self.headers.get_all("X_tenant") or []
            invalid = [v for v in lines if v.strip() not in TENANTS]
            if not lines or invalid:
                return self._reply(400, {"error": "Tenant : %s not valid tenant" % ",".join(lines)})
            return self._reply(200, {"tenant_lines": lines})
        if self.path == "/login":
            conn = http.client.HTTPConnection("127.0.0.1", UPSTREAM_PORT, timeout=10)
            try:
                conn.request("GET", "/session")
                resp = conn.getresponse()
                resp.read()
                cookies = resp.headers.get_all("Set-Cookie") or []
            finally:
                conn.close()
            return self._reply(200, {"cookies": cookies})
        return self._reply(404, {"error": "not found"})

    def log_message(self, fmt, *args):
        sys.stdout.write("[app] " + (fmt % args) + "\n")
        sys.stdout.flush()


if __name__ == "__main__":
    print("[app] listening on :%d" % PORT, flush=True)
    ThreadingHTTPServer(("0.0.0.0", PORT), Handler).serve_forever()
