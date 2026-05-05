#!/usr/bin/env python3
"""Client app for the outbound-fin-stall regression test (#4173).

urllib3 is the same library `botocore` (and therefore boto3) uses to
talk to AWS services. We open ONE pooled HTTP/1.1 keepalive
connection (maxsize=1) and fire two POSTs separated by a sleep that
exceeds the upstream's idle budget. urllib3 calls
wait_for_read(sock, timeout=0) before reusing the pooled conn — that
detects the upstream's FIN and reconnects cleanly. The test gates on
both calls returning 200 within the read_timeout; the call-2 latency
is the asymmetry the relay fix collapses from ~60s to milliseconds.
"""
import os
import sys
import time

import urllib3

URL_HOST = sys.argv[1] if len(sys.argv) > 1 else "127.0.0.1"
URL_PORT = int(sys.argv[2]) if len(sys.argv) > 2 else 9090
GAP = float(sys.argv[3]) if len(sys.argv) > 3 else 8.0

# Match botocore's defaults — read_timeout=60s is what bounds the
# regression's tail latency from the application side.
TIMEOUT = float(os.environ.get("APP_READ_TIMEOUT", "60.0"))

http = urllib3.PoolManager(num_pools=1, maxsize=1)
url = f"http://{URL_HOST}:{URL_PORT}/publish"


def call(label: str, payload: bytes) -> bool:
    """Returns True iff the call returned 2xx within the read timeout."""
    t0 = time.time()
    try:
        r = http.request(
            "POST",
            url,
            body=payload,
            headers={"Content-Type": "application/json"},
            timeout=urllib3.Timeout(connect=3.0, read=TIMEOUT),
        )
        elapsed = time.time() - t0
        ok = 200 <= r.status < 300
        print(
            f"[app] {label}: status={r.status} elapsed={elapsed:.3f}s "
            f"body={r.data!r}",
            flush=True,
        )
        return ok
    except Exception as e:
        elapsed = time.time() - t0
        print(
            f"[app] {label}: FAILED after {elapsed:.3f}s — "
            f"{type(e).__name__}: {e}",
            flush=True,
        )
        return False


def main() -> int:
    print(
        f"[app] target={URL_HOST}:{URL_PORT} gap={GAP}s "
        f"timeout={TIMEOUT}s (urllib3 PoolManager, maxsize=1)",
        flush=True,
    )
    ok1 = call("call-1", b'{"hello":1}')
    print(f"[app] sleeping {GAP}s ...", flush=True)
    time.sleep(GAP)
    ok2 = call("call-2", b'{"hello":2}')
    print("[app] done", flush=True)
    return 0 if (ok1 and ok2) else 1


if __name__ == "__main__":
    sys.exit(main())
