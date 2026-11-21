"""The app of the sampling slot-release check (run-sampling-slot-test.sh).

Each request is on its own connection. Endpoints:
  /order   answers at once: what the recording must capture.
  /hold    writes $SLOT_DIR/hold.in as it comes, then answers once
           $SLOT_DIR/hold.release exists: the probe a filter leaves out,
           in flight.
  /events  a stream (text/event-stream): its first event, then
           $SLOT_DIR/events.in, then its last event once
           $SLOT_DIR/events.release exists.
  /ready   answers at once: the check's readiness probe.
Every wait ends after 60 s, so a check that fails leaves nothing hanging.
"""

import json
import os
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

SLOT_DIR = os.environ["SLOT_DIR"]
PORT = int(os.environ.get("PORT", "8081"))


def mark(name):
    with open(os.path.join(SLOT_DIR, name), "w") as f:
        f.write("1")


def wait_for(name):
    path = os.path.join(SLOT_DIR, name)
    deadline = time.monotonic() + 60
    while not os.path.exists(path) and time.monotonic() < deadline:
        time.sleep(0.05)


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def answer(self):
        body = json.dumps({"path": self.path}).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        path = self.path.split("?", 1)[0]
        if path == "/hold":
            mark("hold.in")
            wait_for("hold.release")
            self.answer()
        elif path == "/events":
            self.close_connection = True
            self.send_response(200)
            self.send_header("Content-Type", "text/event-stream")
            self.send_header("Connection", "close")
            self.end_headers()
            self.wfile.write(b"data: one\n\n")
            self.wfile.flush()
            mark("events.in")
            wait_for("events.release")
            self.wfile.write(b"data: two\n\n")
            self.wfile.flush()
        else:
            self.answer()

    def log_message(self, fmt, *args):
        print("slot_app: " + fmt % args, flush=True)


if __name__ == "__main__":
    print(f"slot_app listening on :{PORT}", flush=True)
    ThreadingHTTPServer(("", PORT), Handler).serve_forever()
