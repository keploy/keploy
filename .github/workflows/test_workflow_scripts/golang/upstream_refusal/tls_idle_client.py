# Opens a TLS connection, completes the handshake at once, waits 2s, then
# makes one request: a pooled connection used a while after it was opened.
import socket
import ssl
import sys
import time

ctx = ssl.create_default_context()
ctx.check_hostname = False
ctx.verify_mode = ssl.CERT_NONE
raw = socket.create_connection(("127.0.0.1", int(sys.argv[1])), 5)
s = ctx.wrap_socket(raw, server_hostname="localhost")
time.sleep(2)
try:
    s.sendall(b"GET / HTTP/1.1\r\nHost: localhost\r\nConnection: close\r\n\r\n")
    reply = s.recv(64).split(b" ")
    print("TLSIDLE", reply[1].decode() if len(reply) > 1 else "empty")
except OSError as e:
    print("TLSIDLE", type(e).__name__)
