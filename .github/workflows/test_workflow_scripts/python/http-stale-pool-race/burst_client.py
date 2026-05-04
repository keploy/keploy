"""Bursty HTTP/1.1 keep-alive client that exposes the upstream-pool
half-close race in keploy's ingress proxy.

Shape: N persistent HTTP/1.1 connections, B bursts of R sequential
requests per connection, with a fixed idle gap between bursts that is
strictly longer than gunicorn's `--keep-alive 2`. During each idle
gap, gunicorn closes its end of every pooled connection. With the bug
present, keploy's two-goroutine io.Copy byte-pump doesn't notice the
FIN, and the next burst's first request on each conn vanishes silently
into a half-dead socket — surfaces here as either a write error
(BrokenPipeError) or a hung getresponse() that the per-request timeout
catches.

Threshold gate: the script exits non-zero if the observed drop rate
exceeds MAX_DROP_PCT (default 5%). Without the fix the drop rate is
~25-50% (one stale-conn first-req per post-idle-gap burst per pooled
conn). With the MSG_PEEK + replay-on-stale fix it is 0%.

Deliberately uses http.client (no urllib3 / requests) — those
libraries auto-retry on stale connections, which masks the bug.
http.client raises and lets the test see the failure raw.
"""
import http.client
import os
import sys
import time

HOST = "127.0.0.1"
PORT = 8080
N_CONNS = 5
BURSTS = 6
REQS_PER_BURST_PER_CONN = 3
IDLE_GAP_SEC = 5
TIMEOUT_SEC = 10
MAX_DROP_PCT = float(os.environ.get("MAX_DROP_PCT", "5"))


def open_conn():
    return http.client.HTTPConnection(HOST, PORT, timeout=TIMEOUT_SEC)


conns = [open_conn() for _ in range(N_CONNS)]

attempts = success = fail = 0
fail_reasons = {}

for burst in range(BURSTS):
    if burst > 0:
        # Idle gap longer than gunicorn --keep-alive 2 → backend
        # closes its half of every pooled conn during this sleep.
        # That is the load-bearing precondition for the bug.
        print(f"  idle gap {IDLE_GAP_SEC}s before burst {burst}")
        time.sleep(IDLE_GAP_SEC)

    print(f"-- burst {burst}")
    for r_idx in range(REQS_PER_BURST_PER_CONN):
        for c_idx, c in enumerate(conns):
            attempts += 1
            try:
                c.request("GET", "/api/echo")
                resp = c.getresponse()
                _ = resp.read()
                if resp.status == 200:
                    success += 1
                else:
                    fail += 1
                    key = f"http_{resp.status}"
                    fail_reasons[key] = fail_reasons.get(key, 0) + 1
                    print(f"  burst={burst} c={c_idx} r={r_idx} {key}")
            except Exception as exc:
                fail += 1
                key = f"err_{type(exc).__name__}"
                fail_reasons[key] = fail_reasons.get(key, 0) + 1
                print(f"  burst={burst} c={c_idx} r={r_idx} {key}: {exc}")
                # Reopen the connection so subsequent reqs in this
                # burst still have a usable socket. Mirrors what
                # envoy / nginx / Go's Transport do on stale-detect.
                try:
                    c.close()
                except Exception:
                    pass
                conns[c_idx] = open_conn()

drop_pct = 100.0 * fail / attempts if attempts > 0 else 0.0
print(
    f"\n=== summary attempts={attempts} success={success} fail={fail} "
    f"drop_pct={drop_pct:.1f}% reasons={fail_reasons}"
)

if drop_pct > MAX_DROP_PCT:
    print(
        f"::error::drop rate {drop_pct:.1f}% exceeds threshold "
        f"{MAX_DROP_PCT}% — half-close race regression"
    )
    sys.exit(1)

print(f"OK: drop rate {drop_pct:.1f}% within threshold {MAX_DROP_PCT}%")
