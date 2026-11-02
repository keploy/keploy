# The upstream both recorded apps call, outside keploy: it answers each GET
# with "up:<path>", so an app's answer says which upstream path it reached.
import http.server
import sys


class H(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        b = ("up:" + self.path).encode()
        self.send_response(200)
        self.send_header("Content-Length", str(len(b)))
        self.end_headers()
        self.wfile.write(b)

    def log_message(self, *a):
        pass


http.server.ThreadingHTTPServer(("0.0.0.0", int(sys.argv[1])), H).serve_forever()
