#!/usr/bin/env python3
# docker-slow.py SOCKET DELAY DIR: a Docker Engine API on a unix socket that
# answers /_ping and /version at once, holds /events open, and answers each
# list of containers (/containers/json) with none, only DELAY seconds after it
# is asked: a daemon slow to list (a Docker Desktop waking its VM). It touches
# DIR/asked when a list is first asked and DIR/listed when one is first
# answered, and writes its pid to DIR/docker.pid.
import os, socketserver, sys, time
from http.server import BaseHTTPRequestHandler

sock, delay, out = sys.argv[1], float(sys.argv[2]), sys.argv[3]


def mark(name):
    open(os.path.join(out, name), "a").close()


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, fmt, *args):
        sys.stderr.write("%.3f %s\n" % (time.time(), fmt % args))

    def answer(self, body, ctype="application/json"):
        b = body.encode()
        self.send_response(200)
        self.send_header("Content-Type", ctype)
        self.send_header("Api-Version", "1.47")
        self.send_header("Content-Length", str(len(b)))
        self.end_headers()
        self.wfile.write(b)

    def do_HEAD(self):
        self.send_response(200)
        self.send_header("Api-Version", "1.47")
        self.send_header("Content-Length", "0")
        self.end_headers()

    def do_GET(self):
        path = self.path.split("?")[0]
        if path.endswith("/_ping"):
            return self.answer("OK", "text/plain")
        if path.endswith("/version"):
            return self.answer('{"ApiVersion":"1.47","MinAPIVersion":"1.24","Version":"27.0.0"}')
        if path.endswith("/events"):
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Api-Version", "1.47")
            self.send_header("Transfer-Encoding", "chunked")
            self.end_headers()
            self.wfile.flush()
            while True:
                time.sleep(3600)
        if path.endswith("/containers/json"):
            mark("asked")
            time.sleep(delay)
            mark("listed")
            return self.answer("[]")
        self.send_response(404)
        self.send_header("Content-Length", "0")
        self.end_headers()


class Server(socketserver.ThreadingMixIn, socketserver.UnixStreamServer):
    daemon_threads = True

    def get_request(self):
        conn, _ = super().get_request()
        return conn, ("unix", 0)


try:
    os.unlink(sock)
except FileNotFoundError:
    pass
with open(os.path.join(out, "docker.pid"), "w") as f:
    f.write(str(os.getpid()))
server = Server(sock, Handler)
os.chmod(sock, 0o666)
server.serve_forever()
