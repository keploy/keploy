# app.py TAG PORT UPSTREAM_HOST UPSTREAM_PORT: on every GET /<path> it
# resolves a DNS name of its run's own (<TAG>dns<path>.keploy-ci.example.com)
# and calls the upstream on a path of its run's own (/<TAG>up/<path>), then
# answers with the upstream's answer. Each recording's mocks must then hold
# its own run's names and paths, and none of the other run's.
import http.server
import socket
import sys
import urllib.request

tag, port, up_host, up_port = sys.argv[1], int(sys.argv[2]), sys.argv[3], sys.argv[4]


class H(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        n = "".join(c if c.isalnum() else "x" for c in self.path) or "root"
        try:
            socket.getaddrinfo(f"{tag}dns{n}.keploy-ci.example.com", 80, proto=socket.IPPROTO_TCP)
        except OSError:
            pass  # the name does not resolve; the query is what is checked
        try:
            body = urllib.request.urlopen(f"http://{up_host}:{up_port}/{tag}up/{n}", timeout=10).read()
        except Exception as e:  # noqa: BLE001 -- the answer says what failed
            body = ("err:" + repr(e)).encode()
        self.send_response(200)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *a):
        pass


http.server.HTTPServer(("0.0.0.0", port), H).serve_forever()
