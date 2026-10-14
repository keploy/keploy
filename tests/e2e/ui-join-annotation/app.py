"""Smallest app that produces a recordable HTTP exchange."""
import http.server
import json
import socketserver


class Handler(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        self.send_response(200)
        self.send_header("content-type", "application/json")
        self.end_headers()
        self.wfile.write(json.dumps({"ok": True}).encode())

    def log_message(self, *args):
        pass


# SO_REUSEADDR, so a previous run's TIME_WAIT socket does not make this
# bind fail and get mistaken for "the feature did not record anything".
socketserver.TCPServer.allow_reuse_address = True
with socketserver.TCPServer(("127.0.0.1", 8099), Handler) as httpd:
    httpd.serve_forever()
