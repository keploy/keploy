#!/usr/bin/env python3
"""App under test for the interim-responses guard. Standard library only.

Each endpoint sends one or more interim (1xx) responses before its final one:

GET /hints        a 103 Early Hints with a Link header, then a 200.
GET /processing   a 102 Processing, then a 200.
POST|PUT /upload  http.server answers "Expect: 100-continue" with a
                  100 Continue before it hands the request over; the
                  handler then reads the body and echoes it in a 201.
POST /deny        the 100 Continue as above, then the handler turns the
                  request down (401) without reading the body, then reads
                  the body and drops it, as Node's server does with a body
                  its handler left, and keeps the connection.
POST /reject      as Go's net/http does for a handler that does not read
                  the body: no 100 Continue, a 401 without the body, and the
                  connection closed after it. curl sends the body once it
                  stops waiting for the 100 (a second).

A recorder that took the first response it read for the answer recorded a
103 (or 102, or 100) with no body, and forwarded only that to the client.
One that cut the body off when the answer came recorded /deny or not
depending on which came first (the client, given the 100, sends the body),
and never recorded /reject.
"""
import json
import sys
from http import HTTPStatus
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

PORT = int(sys.argv[1]) if len(sys.argv) > 1 else 8000
LINK = "</style.css>; rel=preload; as=style"


class Handler(BaseHTTPRequestHandler):
    # Interim responses exist from HTTP/1.1 on; http.server sends the
    # 100 Continue only at this version.
    protocol_version = "HTTP/1.1"

    def _interim(self, status, headers=()):
        self.send_response_only(status)
        for name, value in headers:
            self.send_header(name, value)
        self.end_headers()  # flushes: the client has it before the answer

    def _reply(self, status, payload, headers=()):
        body = json.dumps(payload, sort_keys=True).encode()
        self.send_response(status)
        for name, value in headers:
            self.send_header(name, value)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        if self.path == "/health":
            return self._reply(200, {"ok": True})
        if self.path == "/hints":
            self._interim(HTTPStatus.EARLY_HINTS, [("Link", LINK)])
            return self._reply(200, {"page": "hinted"}, [("Link", LINK)])
        if self.path == "/processing":
            self._interim(HTTPStatus.PROCESSING)
            return self._reply(200, {"page": "processed"})
        return self._reply(404, {"error": "not found"})

    def _upload(self):
        if self.path != "/upload":
            return self._reply(404, {"error": "not found"})
        body = self.rfile.read(int(self.headers.get("Content-Length", 0)))
        return self._reply(201, {"method": self.command, "received": body.decode()})

    def handle_expect_100(self):
        if self.path == "/reject":
            return True  # no 100 Continue: the handler answers without the body
        return super().handle_expect_100()

    def do_POST(self):
        if self.path == "/reject":
            return self._reply(401, {"error": "rejected"}, [("Connection", "close")])
        if self.path == "/deny":
            self._reply(401, {"error": "unauthorized"})
            self.rfile.read(int(self.headers.get("Content-Length", 0)))
            return None
        return self._upload()

    do_PUT = _upload

    def log_message(self, fmt, *args):
        sys.stdout.write("[app] " + (fmt % args) + "\n")
        sys.stdout.flush()


if __name__ == "__main__":
    print("[app] listening on :%d" % PORT, flush=True)
    ThreadingHTTPServer(("0.0.0.0", PORT), Handler).serve_forever()
