#!/usr/bin/env python3
"""App under test for the startup-mock-capture regression guard.

Models an application that fetches a secret from AWS Secret Manager
exactly ONCE at process startup — BEFORE it begins serving inbound
requests — and reuses it for the lifetime of the process. botocore /
boto3 use urllib3 under the hood, so the urllib3 GET below is the same
shape as a real Secret Manager init call.

Because the fetch fires at import time (before Flask accepts any inbound
request), keploy captures it while `firstReqSeen` is still false: it is a
"startup mock" that owns no test window. The regression this guards is
that such a once-per-boot mock must be persisted into EVERY recorded test
set rather than dropped by the syncMock buffer reapers. See the Go unit
tests in pkg/agent/proxy/syncMock for the deterministic coverage.
"""
import sys

import urllib3
from flask import Flask

HOST = sys.argv[1] if len(sys.argv) > 1 else "127.0.0.1"
PORT = int(sys.argv[2]) if len(sys.argv) > 2 else 9091

_http = urllib3.PoolManager(num_pools=1, maxsize=1)


def _fetch_secret() -> str:
    r = _http.request(
        "GET",
        f"http://{HOST}:{PORT}/secret",
        timeout=urllib3.Timeout(connect=5.0, read=10.0),
    )
    return r.data.decode()


# STARTUP: the one-shot "AWS Secret Manager init call", fired at import
# time before Flask serves anything. This is the outbound call whose mock
# must survive into every recorded test set.
SECRET = _fetch_secret()
print(f"[app] startup secret fetched: {SECRET!r}", flush=True)

app = Flask(__name__)


@app.route("/health")
def health():
    return "ok", 200


@app.route("/value")
def value():
    return SECRET, 200


if __name__ == "__main__":
    app.run(host="0.0.0.0", port=8000, debug=False, use_reloader=False)
