#!/usr/bin/env python3
"""Capture installed OpenCode against a localhost fixture, with isolated state."""
import http.server
import json
import os
import pathlib
import sys
import subprocess
import tempfile
import threading
import time

root = pathlib.Path(tempfile.mkdtemp(prefix="mot-opencode-v2-"))
requests = []
model = sys.argv[1] if len(sys.argv) > 1 else "deepseek-v4.1-flash-free"
tools = []


class Fixture(http.server.BaseHTTPRequestHandler):
    def log_message(self, *_):
        pass

    def do_GET(self):
        body = json.dumps({"object": "list", "data": [{"id": model, "object": "model"}]}).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_POST(self):
        size = int(self.headers.get("Content-Length", 0))
        if size > 2 * 1024 * 1024:
            self.send_error(413)
            return
        body = json.loads(self.rfile.read(size))
        headers = {k.lower(): v for k, v in self.headers.items()}
        for k in list(headers):
            if k in {"authorization", "cookie", "proxy-authorization"}:
                headers[k] = "<redacted>"
            elif "session" in k or k == "x-opencode-request":
                headers[k] = "<present; length=%d>" % len(headers[k])
            elif k == "host":
                headers[k] = "<localhost fixture>"
        tools[:] = body.get("tools", [])
        requests.append({"path": self.path, "headers": headers, "model": body.get("model"),
                         "body_keys": sorted(body), "stream": body.get("stream"),
                         "max_tokens": body.get("max_tokens"),
                         "roles": [m.get("role") for m in body.get("messages", [])],
                         "tools": [{"name": t.get("function", t).get("name"),
                                    "required": t.get("function", t).get("parameters", {}).get("required", [])}
                                   for t in body.get("tools", [])]})
        chunk = {"id": "local-fixture", "object": "chat.completion.chunk", "model": model,
                 "created": int(time.time()), "choices": [{"index": 0,
                 "delta": {"role": "assistant", "content": "LOCAL_CAPTURE_OK"}, "finish_reason": "stop"}],
                 "usage": {"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2}}
        payload = ("data: " + json.dumps(chunk) + "\n\ndata: [DONE]\n\n").encode()
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.send_header("Content-Length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)


server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Fixture)
threading.Thread(target=server.serve_forever, daemon=True).start()
config = {"update": "disable", "snapshots": False, "warming": False,
          "model": "opencode/" + model,
          "providers": {"opencode": {"settings": {
              "baseURL": "http://127.0.0.1:%d/v1" % server.server_port, "apiKey": "local-fixture"},
              "models": {model: {"modelID": model, "capabilities": {"tools": True},
                                  "limit": {"context": 1048576, "output": 8192}}}}}}
(root / "opencode.json").write_text(json.dumps(config))
env = {k: v for k, v in os.environ.items() if k in {"PATH", "LANG", "LC_ALL", "TERM", "TMPDIR"}}
env.update({"XDG_DATA_HOME": str(root / "data"), "XDG_CONFIG_HOME": str(root / "config"),
            "XDG_CACHE_HOME": str(root / "cache"), "OPENCODE_CONFIG": str(root / "opencode.json"),
            "OPENCODE_DISABLE_MODELS_FETCH": "true", "OPENCODE_DISABLE_AUTOUPDATE": "true"})
binary = "/home/rian/.npm-global/bin/opencode"
version = subprocess.check_output([binary, "--version"], env=env, cwd=root, text=True).strip()
try:
    run = subprocess.run([binary, "run", "--standalone", "--format", "json", "--title", "Local protocol capture",
                          "-m", "opencode/" + model, "Reply LOCAL_CAPTURE_OK. Do not call tools."],
                         env=env, cwd=root, capture_output=True, text=True, timeout=45)
    code = run.returncode
    diagnostic = run.stderr[-1200:] if not requests else ""
except subprocess.TimeoutExpired:
    code, diagnostic = None, "Local capture timed out"
finally:
    server.shutdown()
report = {"version": version, "mode": "isolated state; localhost fixture; no real credentials or upstream inference",
          "exit_code": code, "requests": requests}
if diagnostic:
    report["diagnostic"] = diagnostic
suffix = "-glm" if model == "glm-5.3-flash-free" else ""
dest = pathlib.Path(__file__).resolve().parent / ("results/opencode-v2-local-capture" + suffix + "-2026-10-17.json")
dest.write_text(json.dumps(report, indent=2) + "\n")
if tools:
    dest.with_name("opencode-v2-tool-schemas.json").write_text(json.dumps(tools, indent=2) + "\n")
print(json.dumps(report, indent=2))
