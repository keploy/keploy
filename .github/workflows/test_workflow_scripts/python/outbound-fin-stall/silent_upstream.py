#!/usr/bin/env python3
"""Fake HTTP upstream for the outbound-fin-stall regression test.

Models the production failure mode from issue #4173: an upstream that
keeps a keepalive TCP connection alive for the first request and then,
once the connection has been idle for IDLE_BUDGET seconds, **sends
FIN** (clean close) without warning. This is exactly the shape of an
AWS ALB hitting its 60s idle timeout — see
https://docs.aws.amazon.com/elasticloadbalancing/latest/application/load-balancer-troubleshooting.html

A urllib3 / botocore client that pooled the previous connection will
detect the FIN before reuse via wait_for_read(sock, timeout=0) and
reconnect cleanly. Keploy's outbound-capture relay sits between the
app and the upstream; before the fix in #4174 it would itself reuse
the dead pool entry and hang on src.Read until the application's own
read_timeout fired (60s for botocore default), producing a 60s tail
on every reused-but-dead connection.

Per-request mode is hard-coded to "fin" (graceful close after idle).
The silent / no-FIN variant exists in the local repro but is not
covered by this test because it would symmetrically hang both with
and without the fix — the bug we are gating on is specifically the
FIN-not-propagated case.
"""
import socket
import sys
import threading
import time

PORT = int(sys.argv[1]) if len(sys.argv) > 1 else 9090
IDLE_BUDGET = 5.0  # seconds


def handle(conn: socket.socket, addr) -> None:
    cid = f"{addr[0]}:{addr[1]}"
    print(f"[upstream] +conn {cid}", flush=True)
    request_count = 0
    buf = b""
    try:
        while True:
            # Wait briefly for the next request bytes; if the budget
            # elapses with no progress, treat the conn as idle and
            # send FIN. Real ALB behavior is wall-clock from the last
            # data byte, which this matches.
            conn.settimeout(IDLE_BUDGET)
            try:
                while b"\r\n\r\n" not in buf:
                    chunk = conn.recv(4096)
                    if not chunk:
                        print(
                            f"[upstream] {cid} client EOF after "
                            f"{request_count} reqs",
                            flush=True,
                        )
                        return
                    buf += chunk
            except socket.timeout:
                print(
                    f"[upstream] {cid} *** IDLE > {IDLE_BUDGET}s "
                    f"→ sending FIN (close)",
                    flush=True,
                )
                try:
                    conn.shutdown(socket.SHUT_WR)
                except OSError:
                    pass
                return

            head, _, rest = buf.partition(b"\r\n\r\n")
            cl = 0
            for line in head.split(b"\r\n"):
                if line.lower().startswith(b"content-length:"):
                    try:
                        cl = int(line.split(b":", 1)[1].strip())
                    except ValueError:
                        cl = 0
                    break
            conn.settimeout(5.0)
            try:
                while len(rest) < cl:
                    more = conn.recv(cl - len(rest))
                    if not more:
                        break
                    rest += more
            except socket.timeout:
                pass
            buf = rest[cl:]

            request_count += 1
            print(
                f"[upstream] {cid} req#{request_count} "
                f"({len(rest[:cl])} body bytes) → 200",
                flush=True,
            )
            body = b"OK\n"
            resp = (
                b"HTTP/1.1 200 OK\r\n"
                b"Content-Length: " + str(len(body)).encode() + b"\r\n"
                b"Connection: keep-alive\r\n"
                b"\r\n" + body
            )
            conn.sendall(resp)
    finally:
        try:
            conn.close()
        except OSError:
            pass


def main() -> None:
    s = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    s.bind(("0.0.0.0", PORT))
    s.listen(16)
    print(
        f"[upstream] listening on 0.0.0.0:{PORT}, idle={IDLE_BUDGET}s",
        flush=True,
    )
    while True:
        conn, addr = s.accept()
        t = threading.Thread(target=handle, args=(conn, addr), daemon=True)
        t.start()


if __name__ == "__main__":
    main()
