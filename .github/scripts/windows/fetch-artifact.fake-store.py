"""The artifact API and blob store as fetch-artifact.ps1 meets them, for its
tests (fetch-artifact.tests.ps1): the run's artifact list, the zip endpoint's
302 to a signed URL (a new token each call), and a blob that serves byte
ranges, with the faults a long download over a lossy path meets.

    python3 -I fetch-artifact.fake-store.py CONFIG.json

CONFIG names the blob, its digest, the faults, and the files to write: the
port it listens on (port_file) and a JSON line per request (log_file).
"""
import json
import os
import select
import socket
import sys
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

cfg = json.load(open(sys.argv[1]))
blob = open(cfg["blob"], "rb").read()
faults = cfg.get("faults", {})
lock = threading.Lock()
state = {"tokens": 0, "served": {}, "broken": set(), "stalled": set()}


def log(**kw):
    with lock:
        with open(cfg["log_file"], "a") as f:
            f.write(json.dumps(kw) + "\n")


class Store(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *a):
        pass

    def reply(self, code, body=b"", headers=()):
        self.send_response(code)
        for k, v in headers:
            self.send_header(k, v)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        path, _, query = self.path.partition("?")
        if path.startswith("/repos/"):
            if self.headers.get("Authorization") != "Bearer " + cfg["token"]:
                log(kind="api", path=path, code=401)
                return self.reply(401, b'{"message":"Bad credentials"}')
            if path.endswith("/artifacts") and "/runs/" in path:
                log(kind="list", path=path, query=query)
                arts = [] if faults.get("no_artifact") else [{
                    "id": 7, "name": cfg["name"], "size_in_bytes": len(blob),
                    "digest": "sha256:" + cfg["digest"] if cfg.get("digest") else None,
                    "expired": False}]
                return self.reply(200, json.dumps({"total_count": len(arts), "artifacts": arts}).encode())
            if path.endswith("/artifacts/7/zip"):
                with lock:
                    state["tokens"] += 1
                    tok = state["tokens"]
                log(kind="sign", token=tok)
                return self.reply(302, headers=[("Location", "http://127.0.0.1:%d/blob/%d" % (self.server.server_port, tok))])
            return self.reply(404)
        if path.startswith("/blob/"):
            return self.blob(int(path.rsplit("/", 1)[1]))
        return self.reply(404)

    def blob(self, tok):
        rng = self.headers.get("Range", "")
        start, end = 0, len(blob) - 1
        ranged = rng.startswith("bytes=")
        if ranged:
            a, _, b = rng[6:].partition("-")
            start, end = int(a), (int(b) if b else len(blob) - 1)
        expire = faults.get("expire_after_bytes")
        with lock:
            spent = state["served"].get(tok, 0)
        if expire is not None and spent >= expire:
            log(kind="blob", token=tok, start=start, end=end, code=403, sent=0)
            return self.reply(403, b"<Error><Code>AuthenticationFailed</Code><Message>Signed expiry time has passed</Message></Error>")
        if start in faults.get("fail_always_from", []):
            log(kind="blob", token=tok, start=start, end=end, code=500, sent=0)
            return self.reply(500, b"ServerBusy")
        data = bytearray(blob[start:end + 1])
        corrupt = faults.get("corrupt_tokens")
        if (corrupt == "all" or (corrupt and tok in corrupt)) and start == 0 and data:
            data[0] ^= 0xFF
        if faults.get("ignore_range") or not ranged:
            code, data = 200, bytearray(blob)
            self.send_response(200)
        else:
            code = 206
            self.send_response(206)
            self.send_header("Content-Range", "bytes %d-%d/%d" % (start, end, len(blob)))
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        cut = len(data)
        stall = False
        with lock:
            if start == faults.get("break_once_from") and start not in state["broken"]:
                state["broken"].add(start)
                cut = len(data) // 2
            if start == faults.get("stall_once_from") and start not in state["stalled"]:
                state["stalled"].add(start)
                cut, stall = len(data) // 3, True
            if expire is not None:
                cut = min(cut, max(0, expire - state["served"].get(tok, 0)))
        sent = 0
        try:
            # Small writes, so that a token's bytes are counted as they go.
            while sent < cut:
                n = min(4096, cut - sent)
                self.wfile.write(data[sent:sent + n])
                self.wfile.flush()
                sent += n
                with lock:
                    state["served"][tok] = state["served"].get(tok, 0) + n
                    if expire is not None and state["served"][tok] >= expire:
                        cut = sent
            if stall:
                # Nothing more until the client gives up on it and closes.
                while True:
                    readable, _, _ = select.select([self.connection], [], [], 0.5)
                    if readable and not self.connection.recv(1, socket.MSG_PEEK):
                        break
        except (BrokenPipeError, ConnectionResetError):
            pass
        finally:
            log(kind="blob", token=tok, start=start, end=end, code=code, sent=sent)
        if sent < len(data):
            self.close_connection = True
            try:
                self.connection.shutdown(2)
            except OSError:
                pass


srv = ThreadingHTTPServer(("127.0.0.1", 0), Store)
srv.daemon_threads = True
with open(cfg["port_file"] + ".tmp", "w") as f:
    f.write(str(srv.server_port))
os.replace(cfg["port_file"] + ".tmp", cfg["port_file"])
srv.serve_forever()
