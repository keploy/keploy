# A server that counts the TCP connections it accepts, one line each in the
# file named by argv[3]. The e2e asserts that an application's one connection
# reaches its destination as one connection under keploy record.
#
#   count_server.py plain     PORT COUNTFILE            echoes the first line back
#   count_server.py tls       PORT COUNTFILE CERT KEY   answers one HTTP request
#   count_server.py idleclose PORT COUNTFILE            closes a connection that
#                                                       sends nothing for 1s
#   count_server.py tlsdeadline PORT COUNTFILE CERT KEY like tls, but closes a
#                                                       connection whose TLS
#                                                       handshake has not
#                                                       started within 1s
import socket
import ssl
import sys
import threading
import time

mode, port, countfile = sys.argv[1], int(sys.argv[2]), sys.argv[3]
ctx = None
if mode in ("tls", "tlsdeadline"):
    ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
    ctx.load_cert_chain(sys.argv[4], sys.argv[5])

s = socket.socket()
s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
s.bind(("127.0.0.1", port))
s.listen(16)


def serve(c):
    try:
        if mode == "idleclose":
            time.sleep(1)  # an idle timeout: the client never spoke
        elif ctx is None:
            c.sendall(b"ECHO:" + c.recv(4096))
        else:
            if mode == "tlsdeadline":
                c.settimeout(1)  # a handshake timeout, as Netty's SslHandler has
            t = ctx.wrap_socket(c, server_side=True)
            t.settimeout(None)
            t.recv(4096)
            t.sendall(b"HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\nok")
            c = t
    except Exception:
        pass
    finally:
        c.close()


while True:
    c, _ = s.accept()
    with open(countfile, "a") as f:
        f.write("conn\n")
    threading.Thread(target=serve, args=(c,), daemon=True).start()
