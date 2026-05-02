"""Minimal Flask app for the http-stale-pool-race regression test.

Run via gunicorn with the sync worker (default) and `--keep-alive 2`. The
short keep-alive is the load-bearing config: it lets gunicorn close idle
connections faster than the bursty client's idle gap, which exercises the
upstream-pool half-close race in keploy's HTTP/1.1 ingress proxy.
"""
from flask import Flask, jsonify

app = Flask(__name__)


@app.get("/api/health")
def health():
    return jsonify(status="ok")


@app.get("/api/echo")
def echo():
    # Body is intentionally tiny — the test cares about per-request
    # success/fail, not throughput. A small response keeps the kernel
    # send buffer from masking write-failures on stale conns.
    return jsonify(value="hello", note="stale-pool-regression")


if __name__ == "__main__":
    app.run(host="0.0.0.0", port=8080)
