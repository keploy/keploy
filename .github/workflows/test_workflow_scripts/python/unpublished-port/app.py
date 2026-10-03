#!/usr/bin/env python3
"""App under test for the unpublished-port guard. Standard library only.

Three listeners in one process, the shapes that found the problem:
  0.0.0.0:8095    GET /health, POST /relay -- /relay calls the two below
  0.0.0.0:8096    POST /echo -- the app's own in-container dependency
  127.0.0.1:8097  POST /echo -- the same, on loopback only, as an internal
                                server usually is

The docker command publishes 8095 and 8097. keploy forwards the ingress of
every port the app listens on, so each call to :8096 and :8097 is recorded as
an INCOMING test case on that port, and replay sends each test from the host
to its recorded port. 8096 is not published. 8097 is, as keploy used to advise,
and still cannot be reached: docker forwards a published port to the
container's own address, and nothing listens there on 8097.

At startup the app calls its own /echo on each, before it serves anything, the
way a self-check does: that makes tests on 8096 and 8097 the first ones in the
set, the tests the replay's readiness gate picks to probe.
"""
import json
import sys
import threading
import time
import urllib.request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

PUBLIC_PORT = 8095
PRIVATE_PORT = 8096
LOOPBACK_PORT = 8097


def reply(handler, status, payload):
    body = json.dumps(payload, sort_keys=True).encode()
    handler.send_response(status)
    handler.send_header("Content-Type", "application/json")
    handler.send_header("Content-Length", str(len(body)))
    handler.end_headers()
    handler.wfile.write(body)


def read_body(handler):
    n = int(handler.headers.get("Content-Length") or 0)
    return handler.rfile.read(n) if n else b""


def call_echo(payload, port=PRIVATE_PORT):
    req = urllib.request.Request(
        "http://127.0.0.1:%d/echo" % port,
        data=json.dumps(payload, sort_keys=True).encode(),
        headers={"Content-Type": "application/json"},
        method="POST",
    )
    with urllib.request.urlopen(req, timeout=10) as resp:
        return json.loads(resp.read())


class Private(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def do_POST(self):
        if self.path != "/echo":
            return reply(self, 404, {"error": "not found"})
        return reply(self, 200, {"echo": json.loads(read_body(self) or b"{}")})

    def log_message(self, fmt, *args):
        print("[app:%d] %s" % (self.server.server_address[1], fmt % args), flush=True)


class Public(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def do_GET(self):
        if self.path == "/health":
            return reply(self, 200, {"ok": True})
        return reply(self, 404, {"error": "not found"})

    def do_POST(self):
        if self.path != "/relay":
            return reply(self, 404, {"error": "not found"})
        payload = json.loads(read_body(self) or b"{}")
        return reply(self, 200, {
            "relayed": call_echo(payload),
            "internal": call_echo(payload, LOOPBACK_PORT),
        })

    def log_message(self, fmt, *args):
        print("[app:%d] %s" % (PUBLIC_PORT, fmt % args), flush=True)


def self_check(port):
    # Retried: under keploy record the port's ingress forwarder comes up a
    # moment after the app's own listen() returns, and a call in that moment
    # is refused. That is not what this fixture is about.
    for attempt in range(40):
        try:
            call_echo({"warmup": True}, port)
            return
        except OSError as e:
            print("[app] self-check of :%d attempt %d: %s" % (port, attempt + 1, e), flush=True)
            time.sleep(0.25)
    sys.exit("[app] self-check of :%d never succeeded" % port)


if __name__ == "__main__":
    for host, port in (("0.0.0.0", PRIVATE_PORT), ("127.0.0.1", LOOPBACK_PORT)):
        server = ThreadingHTTPServer((host, port), Private)
        threading.Thread(target=server.serve_forever, daemon=True).start()
    self_check(PRIVATE_PORT)
    self_check(LOOPBACK_PORT)
    print("[app] self-check passed; serving on :%d" % PUBLIC_PORT, flush=True)
    ThreadingHTTPServer(("0.0.0.0", PUBLIC_PORT), Public).serve_forever()
