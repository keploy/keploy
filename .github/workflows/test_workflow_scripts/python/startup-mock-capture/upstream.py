#!/usr/bin/env python3
"""Fake AWS Secret Manager upstream for the startup-mock-capture e2e.

Answers any request with a fixed secret JSON and closes the connection
(Connection: close, so the urllib3 client opens a fresh conn per boot —
no keepalive bookkeeping needed). Models the one-shot secret fetch an
application performs at boot before it begins serving traffic.
"""
import socket
import sys
import threading

PORT = int(sys.argv[1]) if len(sys.argv) > 1 else 9091

# The unique marker below is what the e2e greps for in each recorded
# test set's mocks.yaml to confirm the startup fetch was captured.
BODY = b'{"SecretString":"keploy-startup-secret-v1"}'


def handle(conn: socket.socket) -> None:
    try:
        conn.settimeout(5.0)
        # Drain the request head; the response is fixed regardless.
        try:
            conn.recv(4096)
        except socket.timeout:
            pass
        resp = (
            b"HTTP/1.1 200 OK\r\n"
            b"Content-Type: application/json\r\n"
            b"Content-Length: " + str(len(BODY)).encode() + b"\r\n"
            b"Connection: close\r\n"
            b"\r\n" + BODY
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
    print(f"[upstream] fake secret-manager listening on 0.0.0.0:{PORT}", flush=True)
    while True:
        conn, _ = s.accept()
        threading.Thread(target=handle, args=(conn,), daemon=True).start()


if __name__ == "__main__":
    main()
