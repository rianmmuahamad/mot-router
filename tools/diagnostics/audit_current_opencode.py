#!/usr/bin/env python3
"""Compare official client versions against a loopback-only HTTP fixture.

No user configuration, real provider credentials, or upstream inference is used.
Captures describe compatibility; they do not establish upstream authorization.
"""
import argparse
import hashlib
import http.server
import json
import os
from pathlib import Path
import subprocess
import tempfile
import threading
import time


def digest(value):
    return hashlib.sha256(json.dumps(value, sort_keys=True, separators=(",", ":")).encode()).hexdigest()


def capture(binary, model, scenario):
    with tempfile.TemporaryDirectory(prefix="mot-current-client-audit-") as directory:
        root = Path(directory)
        captured = []
        network_attempts = []

        class Fixture(http.server.BaseHTTPRequestHandler):
            def log_message(self, *_):
                pass

            def do_GET(self):
                network_attempts.append({"method": "GET", "path": self.path})
                self.send_error(404)

            def do_CONNECT(self):
                network_attempts.append({"method": "CONNECT", "target": self.path})
                self.send_error(502, "External connections disabled by audit fixture")

            def do_POST(self):
                length = int(self.headers.get("Content-Length", 0))
                if not 0 < length <= 2 * 1024 * 1024:
                    self.send_error(413)
                    return
                body = json.loads(self.rfile.read(length))
                headers = dict((k.lower(), v) for k, v in self.headers.items())
                for key in list(headers):
                    if key in {"authorization", "cookie", "proxy-authorization"}:
                        headers[key] = "<redacted>"
                    elif key in {"b3", "traceparent", "host"} or "session" in key or key.startswith("x-opencode-project"):
                        headers[key] = "<present>"
                captured.append({"path": self.path, "headers": headers, "body": body})
                if scenario == "sse_error":
                    payload = {"type": "error", "error": {"type": "FixtureError", "message": "LOCAL_AUDIT_REJECTION"}}
                    wire = "data: " + json.dumps(payload) + "\n\n"
                else:
                    payload = {"id": "local-audit", "object": "chat.completion.chunk", "created": int(time.time()),
                               "model": model, "choices": [{"index": 0, "delta": {"role": "assistant", "content": "LOCAL_AUDIT_OK"}, "finish_reason": "stop"}],
                               "usage": {"prompt_tokens": 21, "completion_tokens": 5, "total_tokens": 26}}
                    wire = "data: " + json.dumps(payload) + "\n\ndata: [DONE]\n\n"
                data = wire.encode()
                self.send_response(200)
                self.send_header("Content-Type", "text/event-stream")
                self.send_header("Content-Length", str(len(data)))
                self.end_headers()
                self.wfile.write(data)

        server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Fixture)
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        local_url = "http://127.0.0.1:%d/v1" % server.server_port
        config = {"update": "disable", "snapshots": False, "warming": False,
                  "model": "opencode/" + model,
                  "providers": {"opencode": {"settings": {"baseURL": local_url,
                                                            "apiKey": "local-audit-fixture"},
                                               "models": {model: {"modelID": model, "capabilities": {"tools": True},
                                                                  "package": "@opencode/ai/providers/openai/chat",
                                                                  "settings": {"baseURL": local_url},
                                                                  "limit": {"context": 1048576, "output": 8192}}}}}}
        config_path = root / "opencode.json"
        config_path.write_text(json.dumps(config))
        env = {key: value for key, value in os.environ.items() if key in {"PATH", "LANG", "LC_ALL", "TERM"}}
        env.update({"XDG_DATA_HOME": str(root / "data"), "XDG_CONFIG_HOME": str(root / "config"),
                    "XDG_CACHE_HOME": str(root / "cache"), "OPENCODE_CONFIG": str(config_path),
                    "OPENCODE_DISABLE_MODELS_FETCH": "true", "OPENCODE_DISABLE_AUTOUPDATE": "true",
                    "HTTP_PROXY": "http://127.0.0.1:%d" % server.server_port,
                    "HTTPS_PROXY": "http://127.0.0.1:%d" % server.server_port,
                    "NO_PROXY": "127.0.0.1,localhost,::1", "no_proxy": "127.0.0.1,localhost,::1"})
        version = subprocess.check_output([binary, "--version"], env=env, cwd=root, text=True).strip()
        result = {"version": version, "model": model, "scenario": scenario,
                  "route_mode": "explicit per-model localhost endpoint and OpenAI Chat runtime"}
        try:
            run = subprocess.run([binary, "run", "--standalone", "--format", "json", "--title", "Local compatibility audit",
                                  "-m", "opencode/" + model, "Reply LOCAL_AUDIT_OK. Do not call tools."],
                                 env=env, cwd=root, capture_output=True, text=True, timeout=40)
            events = []
            for line in run.stdout.splitlines():
                try:
                    event = json.loads(line)
                except ValueError:
                    continue
                if event.get("type") == "error":
                    events.append({"type": "error", "error": event.get("error")})
                elif event.get("type") == "text":
                    events.append({"type": "text", "text": event.get("part", {}).get("text", "")})
            result.update(exit_code=run.returncode, events=events)
            if not captured:
                result["diagnostic"] = run.stderr[-1200:]
        except subprocess.TimeoutExpired:
            result.update(exit_code=None, diagnostic="Timed out after 40 seconds")
        finally:
            server.shutdown()
            server.server_close()
            thread.join(timeout=2)
        summaries = []
        for request in captured:
            body = request["body"]
            tools = [t.get("function", t) for t in body.get("tools", [])]
            summaries.append({"path": request["path"], "headers": request["headers"],
                              "body_keys": sorted(body),
                              "parameters": {k: v for k, v in body.items() if k not in {"messages", "tools"}},
                              "messages": [{"role": m.get("role"), "content_characters": len(str(m.get("content", "")))} for m in body.get("messages", [])],
                              "tools": [{"name": t.get("name"), "definition_sha256": digest(t),
                                         "required": t.get("parameters", {}).get("required", [])} for t in tools]})
        result["requests"] = summaries
        result["blocked_or_auxiliary_requests"] = network_attempts
        return result, captured


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--installed", default="/home/rian/.npm-global/bin/opencode")
    parser.add_argument("--latest", required=True)
    parser.add_argument("--output", required=True)
    parser.add_argument("--raw-output", default="/tmp/mot-current-opencode-audit-raw.json")
    args = parser.parse_args()
    report = {"mode": "isolated state; loopback-only fixture; no upstream inference or real credentials", "clients": []}
    raw = []
    for binary in [args.installed, args.latest]:
        for model in ["deepseek-v4.1-flash-free", "glm-5.3-flash-free"]:
            result, requests = capture(binary, model, "text")
            report["clients"].append(result)
            raw.append({"version": result["version"], "model": model, "requests": requests})
            print(json.dumps({"version": result["version"], "model": model, "exit_code": result["exit_code"], "requests": len(requests)}), flush=True)
    result, requests = capture(args.latest, "deepseek-v4.1-flash-free", "sse_error")
    report["clients"].append(result)
    Path(args.output).write_text(json.dumps(report, indent=2) + "\n")
    Path(args.raw_output).write_text(json.dumps(raw, indent=2) + "\n")
    print(json.dumps({"report": args.output, "sse_error_exit_code": result["exit_code"], "sse_error_events": result.get("events")}))


if __name__ == "__main__":
    main()
