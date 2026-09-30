#!/usr/bin/env python3
"""Upstream for the repeated-headers guard: GET /session answers with two
Set-Cookie lines. The second carries an Expires date, whose comma makes a
folded "a,b" Set-Cookie impossible to split back apart."""
import sys
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

PORT = int(sys.argv[1]) if len(sys.argv) > 1 else 9092


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def do_GET(self):
        body = b"ok"
        self.send_response(200)
        self.send_header("Set-Cookie", "session=abc123; Path=/; HttpOnly")
        self.send_header("Content-Type", "text/plain")
        self.send_header("Set-Cookie", "region=in-south; Path=/; Expires=Wed, 21 Oct 2037 07:28:00 GMT")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, fmt, *args):
        sys.stdout.write("[upstream] " + (fmt % args) + "\n")
        sys.stdout.flush()


if __name__ == "__main__":
    ThreadingHTTPServer(("127.0.0.1", PORT), Handler).serve_forever()
