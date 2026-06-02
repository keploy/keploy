"""Minimal Flask app for the http-stale-pool-race regression test.

Run via gunicorn with `--worker-class gthread --threads 4 --keep-alive 2`.
gthread is required: gunicorn's default `sync` worker silently disables
HTTP keep-alive regardless of the `--keep-alive` flag, which means
connections never persist across the bursty client's idle gap and the
race we're trying to reproduce can't fire. gthread honors `--keep-alive`
and closes idle pooled connections after 2s — exactly the asymmetric
timeout that exposes keploy's upstream-pool half-close race.
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
