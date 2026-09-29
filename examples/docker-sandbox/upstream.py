import json
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from threading import Lock


counts = {"reads": 0, "writes": 0, "requests": []}
counts_lock = Lock()


class Handler(BaseHTTPRequestHandler):
    def send_body(self, status, body, content_type="text/plain"):
        self.send_response(status)
        self.send_header("Content-Type", content_type)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        if self.path == "/summary":
            with counts_lock:
                body = json.dumps(counts).encode()
            self.send_body(200, body, "application/json")
            return
        with counts_lock:
            counts["reads"] += 1
        self.send_body(200, b"container-upstream-read")

    def do_POST(self):
        length = int(self.headers.get("Content-Length", "0"))
        body = self.rfile.read(length).decode("utf-8", errors="replace")
        with counts_lock:
            counts["writes"] += 1
            counts["requests"].append({
                "method": "POST",
                "path": self.path,
                "body": body,
                "branch": self.headers.get("X-Demo-Branch", ""),
            })
        self.send_body(201, b"container-upstream-write")

    def log_message(self, *_):
        return


ThreadingHTTPServer(("0.0.0.0", 8080), Handler).serve_forever()
