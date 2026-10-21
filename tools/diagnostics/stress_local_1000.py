#!/usr/bin/env python3
"""Exercise the router binary with 1,000 requests against loopback fixtures only."""
import argparse
import collections
import concurrent.futures
import datetime
import hashlib
import http.server
import json
import math
import os
import pathlib
import queue
import re
import shutil
import socket
import sqlite3
import statistics
import subprocess
import tempfile
import threading
import time
import urllib.error
import urllib.request

parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument("--binary", default="/tmp/mot-router-native-contract")
parser.add_argument("--workers", type=int, default=4, choices=range(1, 9))
parser.add_argument("--output", help="Write a separate result file without overwriting earlier runs")
args = parser.parse_args()
repo = pathlib.Path(__file__).resolve().parents[2]
results_dir = repo / "tools/diagnostics/results"
tools = json.loads((results_dir / "omp-tool-schemas.json").read_text())
native = json.loads((results_dir / "opencode-v2-tool-schemas.json").read_text())
names = ["read"]  # Current router probe policy; independent of the caller tool roster.
models = ["glm-5.3-flash-free", "deepseek-v4.1-flash-free"]
root = pathlib.Path(tempfile.mkdtemp(prefix="mot-local-1000-"))
mutex = threading.Lock()
hits = collections.Counter()
seen = collections.Counter()
header_errors = []
long_text = " synthetic fixture context" * 10923


def scenario(index):
    if index % 50 == 0:
        return "sse_error"
    return "http403" if index % 10 == 8 else "http429" if index % 10 == 9 else "tool" if index % 10 in (6, 7) else "text"


class Fixture(http.server.BaseHTTPRequestHandler):
    def log_message(self, *_):
        pass

    def do_GET(self):
        data = json.dumps({"object": "list", "data": [{"id": m, "object": "model"} for m in models]}).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def do_POST(self):
        size = int(self.headers.get("Content-Length", 0))
        if not 0 < size <= 2 * 1024 * 1024:
            self.send_error(413)
            return
        body = json.loads(self.rfile.read(size))
        content = " ".join(str(m.get("content", "")) for m in body.get("messages", []) if m.get("role") == "user")
        marker = re.search(r"case:(\d+)", content)
        if marker is None:
            with mutex:
                hits["adaptation_probes"] += 1
            self.send_response(200)
            self.send_header("Content-Type", "text/event-stream")
            self.end_headers()
            self.wfile.write(b'data: {"choices":[{"delta":{"content":"OK"},"finish_reason":"stop"}]}\n\ndata: [DONE]\n\n')
            return
        index = int(marker.group(1))
        kind = scenario(index)
        with mutex:
            hits[kind] += 1
            seen[index] += 1
        errors = []
        session = self.headers.get("x-opencode-session")
        if self.headers.get("User-Agent") != "opencode/latest/2.0.29/cli":
            errors.append("wrong User-Agent")
        if not session or len(session) != 30 or any(self.headers.get(k) != session for k in ["x-opencode-session-id", "x-session-affinity", "x-session-id"]):
            errors.append("inconsistent session aliases")
        if self.headers.get("x-opencode-request"):
            errors.append("V1 request header leaked")
        if not re.fullmatch(r"[0-9a-f]{40}", self.headers.get("x-opencode-project", "")):
            errors.append("wrong project shape")
        if not self.headers.get("b3") or not self.headers.get("traceparent"):
            errors.append("missing tracing")
        if body.get("model") != models[(index // 10) % 2]:
            errors.append("model substitution")
        if body.get("max_completion_tokens") != 512 or "max_tokens" in body:
            errors.append("wrong output limit parameter")
        actual_names = {t.get("function", {}).get("name") for t in body.get("tools", [])}
        if not set(names).issubset(actual_names):
            errors.append("missing native roster")
        if not any(m.get("role") == "tool" for m in body.get("messages", [])):
            errors.append("tool history lost")
        if errors:
            with mutex:
                if len(header_errors) < 20:
                    header_errors.append({"index": index, "errors": errors})
        error_type = "FreeTierError" if kind == "http403" or (kind == "sse_error" and index % 100 == 0) else "FreeUsageLimitError"
        if kind in ("http403", "http429", "sse_error"):
            self.send_response(403 if kind == "http403" else 429 if kind == "http429" else 200)
            self.send_header("Content-Type", "text/event-stream" if kind == "sse_error" else "application/json")
            self.end_headers()
            data = json.dumps({"type": "error", "error": {"type": error_type, "message": "Synthetic local fixture rejection"}}).encode()
            self.wfile.write(b"data: " + data + b"\n\n" if kind == "sse_error" else data)
            return
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.end_headers()

        def emit(delta, finish=None, usage=None):
            chunk = {"id": "fixture-" + str(index), "object": "chat.completion.chunk", "model": body["model"],
                     "choices": [{"index": 0, "delta": delta, "finish_reason": finish}]}
            if usage:
                chunk["usage"] = usage
            self.wfile.write(("data: " + json.dumps(chunk) + "\n\n").encode())
            self.wfile.flush()

        emit({"role": "assistant"}, usage={"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2})
        if kind == "tool":
            wire_tool = "shell" if "shell" in actual_names else "bash"
            emit({"tool_calls": [{"index": 0, "id": "call-" + str(index), "type": "function", "function": {"name": wire_tool, "arguments": '{"command":'}}]})
            emit({"tool_calls": [{"index": 0, "function": {"arguments": '"printf fixture"}'}}]}, "tool_calls")
        else:
            emit({"content": "FIXTURE_"})
            emit({"content": "OK_" + str(index)}, "stop")
        emit({}, usage={"prompt_tokens": 21, "completion_tokens": 5, "total_tokens": 26})
        self.wfile.write(b"data: [DONE]\n\n")


fixture = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Fixture)
threading.Thread(target=fixture.serve_forever, daemon=True).start()
profile = {"client_format": "v2", "verified_version": "2.0.29", "probe_model": models[1],
           "user_agent": "opencode/latest/2.0.29/cli", "identity": {"mode": "timestamp", "counter_bits": 12,
           "timestamp_bytes": 6, "suffix_length": 14, "session_descending": True}, "session_prefix": "ses_",
           "session_hex": 12, "session_alnum": 14, "requires_stream": True, "required_tools": names,
           "accepts_tool_superset": False, "tool_descriptions": {t["function"]["name"]: t["function"]["description"] for t in native if t["function"]["name"] in names},
           "source": "probe", "verdict": "ok", "checked_at": datetime.datetime.now(datetime.timezone.utc).isoformat()}
with sqlite3.connect(root / "mot-router.db") as db:
    db.execute("CREATE TABLE settings(key TEXT PRIMARY KEY,value TEXT NOT NULL)")
    db.execute("INSERT INTO settings VALUES(?,?)", ("gate.verified_profile.v1", json.dumps(profile)))
with socket.socket() as reserved:
    reserved.bind(("127.0.0.1", 0))
    port = reserved.getsockname()[1]
env = {k: v for k, v in os.environ.items() if k in {"PATH", "LANG", "TMPDIR"}}
env.update({"ZEN_BASE_URL": f"http://127.0.0.1:{fixture.server_port}/v1", "MOT_MODEL": models[1]})
log = open(root / "router.log", "wb")
process = subprocess.Popen([args.binary, "-host", "127.0.0.1", "-port", str(port), "-data", str(root)], env=env, stdout=log, stderr=log)
opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
base = f"http://127.0.0.1:{port}"
report = {"mode": "1,000 client requests; loopback-only synthetic upstream; no VPS or real provider calls",
          "binary_sha256": hashlib.sha256(pathlib.Path(args.binary).read_bytes()).hexdigest(), "concurrency": args.workers, "reference_profile": "current read-only probe policy; tool fixtures invoke only advertised tools"}


def perform(index):
    model, kind = models[(index // 10) % 2], scenario(index)
    anthropic = index % 3 == 0
    marker = f"case:{index} Reply only to the fixture."
    if index < 100 and index % 10 < 8:
        marker += long_text
    if anthropic:
        payload = {"model": "opencode-zen/" + model, "max_tokens": 512, "system": "Synthetic fixture only.",
                   "messages": [{"role": "user", "content": "Read fixture.txt"},
                                {"role": "assistant", "content": [{"type": "tool_use", "id": "history", "name": "read", "input": {"path": "fixture.txt"}}]},
                                {"role": "user", "content": [{"type": "tool_result", "tool_use_id": "history", "content": "Synthetic history result"}, {"type": "text", "text": marker}]}],
                   "tools": [{"name": t["function"]["name"], "description": t["function"].get("description", ""), "input_schema": t["function"]["parameters"]} for t in tools]}
    else:
        payload = {"model": "opencode-zen/" + model, "max_completion_tokens": 512, "stream": index % 2 == 0,
                   "messages": [{"role": "system", "content": "Synthetic fixture only."}, {"role": "user", "content": "Read fixture.txt"},
                                {"role": "assistant", "tool_calls": [{"id": "history", "type": "function", "function": {"name": "read", "arguments": '{"path":"fixture.txt"}'}}]},
                                {"role": "tool", "tool_call_id": "history", "content": "Synthetic history result"}, {"role": "user", "content": marker}], "tools": tools}
    req = urllib.request.Request(base + ("/v1/messages" if anthropic else "/v1/chat/completions"), data=json.dumps(payload).encode(),
                                 headers={"Content-Type": "application/json", "Authorization": "Bearer " + key, "User-Agent": "local-stress-fixture/1"})
    started = time.monotonic()
    try:
        try:
            with opener.open(req, timeout=20) as response:
                status, raw = response.status, response.read(256 * 1024).decode()
        except urllib.error.HTTPError as response:
            status, raw = response.code, response.read(256 * 1024).decode()
        error_kind = "FreeTierError" if kind == "http403" or (kind == "sse_error" and index % 100 == 0) else "FreeUsageLimitError"
        if kind.startswith("http") or kind == "sse_error":
            correct = error_kind in raw and (status >= 400 or '"error"' in raw)
            problem = "upstream SSE error became empty success" if kind == "sse_error" and not correct else "wrong upstream error" if not correct else None
        else:
            frames = [json.loads(line[6:]) for line in raw.splitlines() if line.startswith("data: ") and line[6:].strip() != "[DONE]"] if raw.startswith(("data:", "event:")) else [json.loads(raw)]
            usage = next((f["usage"] for f in reversed(frames) if f.get("usage")), {})
            correct_usage = (usage.get("prompt_tokens", usage.get("input_tokens")), usage.get("completion_tokens", usage.get("output_tokens"))) == (21, 5)
            text = "".join((f.get("delta", {}).get("text") or "") + "".join(c.get("delta", {}).get("content") or c.get("message", {}).get("content") or "" for c in f.get("choices", [])) for f in frames)
            correct_content = text == f"FIXTURE_OK_{index}" if kind == "text" else any('"name": "bash"' in json.dumps(f) and "printf fixture" in json.dumps(f) for f in frames) or ('"name":"bash"' in raw and "printf fixture" in raw)
            correct = status == 200 and correct_usage and correct_content
            problem = "response content/tool mapping/usage mismatch" if not correct else None
        return {"index": index, "scenario": kind, "model": model, "surface": "anthropic" if anthropic else "openai",
                "status": status, "correct": correct, "problem": problem, "elapsed_ms": round((time.monotonic() - started) * 1000, 2)}
    except Exception as error:
        return {"index": index, "scenario": kind, "model": model, "status": 0, "correct": False,
                "problem": type(error).__name__ + ": " + str(error), "elapsed_ms": round((time.monotonic() - started) * 1000, 2)}


try:
    for _ in range(100):
        if process.poll() is not None:
            raise RuntimeError("Fixture router failed to start")
        try:
            with opener.open(base + "/api/gate", timeout=1) as response:
                assert json.load(response)["user_agent"] == profile["user_agent"]
            break
        except (OSError, AssertionError):
            time.sleep(.05)
    else:
        raise RuntimeError("Fixture router did not become ready")
    with sqlite3.connect(root / "mot-router.db") as db:
        key = db.execute("SELECT key FROM api_keys WHERE revoked=0 LIMIT 1").fetchone()[0]
    start = time.monotonic()
    with concurrent.futures.ThreadPoolExecutor(max_workers=args.workers) as workers:
        records = []
        for record in workers.map(perform, range(1000)):
            records.append(record)
            if len(records) % 100 == 0:
                print(json.dumps({"completed": len(records), "validation_failures": sum(not r["correct"] for r in records)}), flush=True)
    elapsed = time.monotonic() - start
    with sqlite3.connect(root / "mot-router.db") as db:
        logged = db.execute("SELECT status,count(*) FROM requests GROUP BY status").fetchall()
    memory = pathlib.Path(f"/proc/{process.pid}/status").read_text()
    rss = {name: int(re.search(rf"^{name}:\s+(\d+)", memory, re.M).group(1)) for name in ["VmRSS", "VmHWM"]}
    times = sorted(r["elapsed_ms"] for r in records)
    report.update({"completed": len(records), "correct": sum(r["correct"] for r in records), "validation_failures": sum(not r["correct"] for r in records),
                   "elapsed_seconds": round(elapsed, 3), "requests_per_second": round(1000 / elapsed, 2),
                   "latency_ms": {"p50": times[499], "p95": times[949], "p99": times[989]},
                   "router_rss_mib": round(rss["VmRSS"] / 1024, 2), "router_peak_rss_mib": round(rss["VmHWM"] / 1024, 2),
                   "status_counts": dict(collections.Counter(r["status"] for r in records)), "scenario_counts": dict(hits),
                   "sqlite_logged_status_counts": dict(logged), "unique_inference_requests": len(seen),
                   "duplicate_inference_requests": sum(max(0, n - 1) for n in seen.values()), "header_or_body_contract_errors": header_errors,
                   "large_input_requests": 80, "large_input_characters": len(long_text),
                   "failure_samples": [r for r in records if not r["correct"]][:25],
                   "production_services_changed": False, "real_provider_requests": 0,
                   "interpretation": "Local compatibility results do not prove OpenCode upstream will accept real inference."})
    path = pathlib.Path(args.output) if args.output else results_dir / ("local-1000-requests-" + datetime.datetime.now().strftime("%Y-%m-%d") + ".json")
    path.write_text(json.dumps(report, indent=2) + "\n")
    print(json.dumps(report, indent=2))
finally:
    process.terminate()
    try:
        process.wait(timeout=5)
    except subprocess.TimeoutExpired:
        process.kill()
        process.wait()
    log.close()
    fixture.shutdown()
    shutil.rmtree(root)
