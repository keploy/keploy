# Stub package index and file servers for capture.sh: each fails pip one way.
# Binds free ports on 127.0.0.1 and writes them to <portfile> as shell
# assignments once all are listening.
#
#   usage: python3 pip-stubs.py <wheel> <portfile>
#
#   IDX_HANG  accepts a connection and never answers it (a read timeout).
#   IDX       an index that answers. Its pages, each linking to the wheel:
#               /simple/flask      at 192.0.2.1 (TEST-NET-1: a connect timeout)
#               /hang/flask        on IDX_HANG
#               /stall/flask       on F_STALL
#               /cut/flask         on F_CUT
#               /flaky/flask       on F_OK
#               /reset/flask       on F_OK
#             and with no links (a package that does not exist):
#               /simple/nosuchpkg, /flaky/nosuchpkg, /reset/nosuchpkg
#             The first FLAKY_FAILS (default 1) requests under /flaky/ are
#             never answered, and the first request under /reset/ is reset.
#   F_STALL   sends the wheel's headers and half of it, then nothing.
#   F_CUT     sends the wheel's headers and half of it, then closes.
#   F_OK      sends the whole wheel.
import http.server
import os
import socket
import socketserver
import struct
import sys
import threading
import time

WHEEL = open(sys.argv[1], 'rb').read()
NAME = os.path.basename(sys.argv[1])
PORTFILE = sys.argv[2]
HEAD = b'HTTP/1.1 200 OK\r\nContent-Type: application/octet-stream\r\nContent-Length: %d\r\nConnection: close\r\n\r\n' % len(WHEEL)


def listener():
    s = socket.socket()
    s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    s.bind(('127.0.0.1', 0))
    s.listen(64)
    return s


def never_answer(s):
    held = []
    while True:
        c, _ = s.accept()
        held.append(c)


def half(s, then_close):
    held = []
    while True:
        c, _ = s.accept()
        c.recv(65536)
        c.sendall(HEAD)
        c.sendall(WHEEL[: len(WHEEL) // 2])
        if then_close:
            c.close()
        else:
            held.append(c)


def whole(s):
    while True:
        c, _ = s.accept()
        c.recv(65536)
        c.sendall(HEAD)
        c.sendall(WHEEL)
        c.close()


hang, stall, cut, ok = listener(), listener(), listener(), listener()
P = {
    'IDX_HANG': hang.getsockname()[1],
    'F_STALL': stall.getsockname()[1],
    'F_CUT': cut.getsockname()[1],
    'F_OK': ok.getsockname()[1],
}
seen = {'flaky': 0, 'reset': 0}
FLAKY_FAILS = int(os.environ.get('FLAKY_FAILS', '1'))
lock = threading.Lock()


def link(port, host='127.0.0.1'):
    where = host if port is None else '%s:%d' % (host, port)
    return '<a href="http://%s/packages/%s">%s</a>' % (where, NAME, NAME)


class Index(http.server.BaseHTTPRequestHandler):
    def log_message(self, *a):
        pass

    def finish(self):
        try:
            super().finish()
        except OSError:
            pass  # the socket a /reset/ request closed

    def do_GET(self):
        p = self.path.rstrip('/')
        pages = {
            '/simple/flask': link(None, '192.0.2.1'),
            '/hang/flask': link(P['IDX_HANG']),
            '/stall/flask': link(P['F_STALL']),
            '/cut/flask': link(P['F_CUT']),
            '/flaky/flask': link(P['F_OK']),
            '/reset/flask': link(P['F_OK']),
            '/simple/nosuchpkg': '',
            '/flaky/nosuchpkg': '',
            '/reset/nosuchpkg': '',
        }
        for prefix, fails in (('flaky', FLAKY_FAILS), ('reset', 1)):
            if p.startswith('/%s/' % prefix):
                with lock:
                    seen[prefix] += 1
                    failing = seen[prefix] <= fails
                if failing and prefix == 'flaky':
                    time.sleep(3600)
                if failing and prefix == 'reset':
                    # SO_LINGER 0: close() sends a RST, not a FIN.
                    self.connection.setsockopt(socket.SOL_SOCKET, socket.SO_LINGER, struct.pack('ii', 1, 0))
                    self.connection.close()
                    self.close_connection = True
                    return
        if p not in pages:
            self.send_response(404)
            self.end_headers()
            return
        body = ('<html><body>%s</body></html>\n' % pages[p]).encode()
        self.send_response(200)
        self.send_header('Content-Type', 'text/html')
        self.send_header('Content-Length', str(len(body)))
        self.end_headers()
        self.wfile.write(body)


socketserver.TCPServer.allow_reuse_address = True
srv = socketserver.ThreadingTCPServer(('127.0.0.1', 0), Index)
srv.daemon_threads = True
P['IDX'] = srv.server_address[1]
for target, args in ((never_answer, (hang,)), (half, (stall, False)), (half, (cut, True)), (whole, (ok,)), (srv.serve_forever, ())):
    threading.Thread(target=target, args=args, daemon=True).start()
with open(PORTFILE + '.tmp', 'w') as f:
    f.write(''.join('%s=%d\n' % kv for kv in P.items()))
os.rename(PORTFILE + '.tmp', PORTFILE)
while True:
    time.sleep(3600)
