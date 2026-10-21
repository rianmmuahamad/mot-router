#!/usr/bin/env python3
"""Capture OMP tool schemas using an isolated localhost-only provider."""
import http.server
import json
import os
import pathlib
import subprocess
import tempfile
import threading
import time

root = pathlib.Path(tempfile.mkdtemp(prefix="mot-omp-capture-"))
model = "glm-5.3-flash-free"
requests = []
tools = []


class Fixture(http.server.BaseHTTPRequestHandler):
    def log_message(self, *_):
        pass

    def do_POST(self):
        size = int(self.headers.get("Content-Length", 0))
        if not 0 < size <= 2 * 1024 * 1024:
            self.send_error(413)
            return
        body = json.loads(self.rfile.read(size))
        tools[:] = body.get("tools", [])
        requests.append({"path": self.path, "model": body.get("model"),
                         "body_keys": sorted(body), "stream": body.get("stream"),
                         "roles": [m.get("role") for m in body.get("messages", [])],
                         "tool_names": [t.get("function", t).get("name") for t in tools]})
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
config = {"providers": {"diagnostic": {
    "baseUrl": f"http://127.0.0.1:{server.server_port}/v1", "api": "openai-completions",
    "apiKey": "local-fixture", "models": [{"id": model, "name": "GLM fixture",
        "reasoning": False, "input": ["text"], "contextWindow": 131072, "maxTokens": 256,
        "cost": {"input": 0, "output": 0, "cacheRead": 0, "cacheWrite": 0}}]}}}
agent = root / "agent"
agent.mkdir()
(agent / "models.json").write_text(json.dumps(config))
env = {k: v for k, v in os.environ.items() if k in {"PATH", "LANG", "LC_ALL", "TERM", "TMPDIR"}}
env.update({"PI_CODING_AGENT_DIR": str(agent), "XDG_DATA_HOME": str(root / "data"),
            "XDG_CONFIG_HOME": str(root / "config"), "XDG_CACHE_HOME": str(root / "cache")})
binary = "/home/rian/.bun/bin/omp"
try:
    version = subprocess.check_output([binary, "--version"], env=env, cwd=root, text=True, timeout=15).strip()
    run = subprocess.run([binary, "--provider", "diagnostic", "--model", model,
                          "--no-session", "--no-title", "--no-prewalk", "--no-extensions",
                          "--no-skills", "--no-rules", "--no-lsp", "--no-pty", "-p",
                          "Reply LOCAL_CAPTURE_OK. Do not call tools."],
                         env=env, cwd=root, capture_output=True, text=True, timeout=45)
    report = {"version": version, "exit_code": run.returncode, "requests": requests}
    if not requests:
        report["diagnostic"] = run.stderr[-2000:] + run.stdout[-1000:]
except subprocess.TimeoutExpired:
    report = {"error": "local capture timed out", "requests": requests}
finally:
    server.shutdown()
report["mode"] = "isolated state; localhost fixture; no real credentials or upstream inference"
dest = pathlib.Path(__file__).resolve().parent / "results/omp-local-capture-2026-10-17.json"
dest.write_text(json.dumps(report, indent=2) + "\n")
if tools:
    # Only public built-in tool schemas; never retain system prompts or credentials.
    dest.with_name("omp-tool-schemas.json").write_text(json.dumps(tools, indent=2) + "\n")
print(json.dumps(report, indent=2))
