#!/usr/bin/env python3
"""Run on the VPS: bounded GLM-only acceptance checks, stopping on rejection."""
import json
import pathlib
import sqlite3
import sys
import time
import urllib.error
import urllib.request

model = "glm-5.3-flash-free"
report = {"model": model, "checks": {}, "model_substitution": False}
dest = pathlib.Path("/tmp/mot-glm-consistency-result.json")


def finish(reason=None):
    if reason:
        report["stopped"] = reason
    dest.write_text(json.dumps(report, indent=2) + "\n")
    dest.chmod(0o600)
    print(json.dumps(report, indent=2))
    sys.exit(1 if reason else 0)


def post(path, payload):
    req = urllib.request.Request("http://127.0.0.1:8787" + path,
        data=json.dumps(payload).encode(), headers={"Content-Type": "application/json"})
    with urllib.request.urlopen(req, timeout=100) as r:
        return json.load(r)


gate = post("/api/gate/refresh?model=" + model, {})
report["checks"]["baseline"] = {k: gate.get(k) for k in
    ["probe_model", "source", "verdict", "client_format", "verified_version", "required_tools"]}
report["checks"]["baseline"]["conditions"] = [
    {k: v.get(k) for k in ["name", "ok", "status", "inconclusive"]}
    for v in gate.get("conditions", [])]
if gate.get("probe_model") != model or gate.get("source") != "probe" or gate.get("verdict") != "ok":
    finish("GLM baseline did not verify; no extra inference or IP rotation requested")

test = post("/api/gate/test", {})
report["checks"]["gate_end_to_end"] = test
if not test.get("ok") or test.get("text", "").strip() != "GATE_OK" or test.get("model") != model:
    finish("GLM end-to-end verification failed")

play = post("/api/playground", {"model": "opencode-zen/" + model,
    "prompt": "Reply exactly GLM_PLAY_OK. Do not call tools."})
report["checks"]["playground"] = play
if play.get("text", "").strip() != "GLM_PLAY_OK" or play.get("error"):
    finish("GLM Playground failed")

schemas = json.loads(pathlib.Path(sys.argv[1]).read_text())
with sqlite3.connect("file:/opt/mot-router/data/mot-router.db?mode=ro", uri=True) as db:
    row = db.execute("SELECT key FROM api_keys WHERE revoked=0 ORDER BY (lower(name) LIKE ?) DESC,id LIMIT 1", ("%motgate%",)).fetchone()
if not row:
    finish("No existing integration key available")
payload = {"model": "opencode-zen/" + model, "stream": True, "max_completion_tokens": 512,
    "messages": [{"role": "system", "content": "Follow the user's instruction. Do not call tools."},
                 {"role": "user", "content": "Reply exactly GLM_AGENT_OK."}], "tools": schemas}
req = urllib.request.Request("http://127.0.0.1:8787/v1/chat/completions", data=json.dumps(payload).encode(),
    headers={"Content-Type": "application/json", "Authorization": "Bearer " + row[0],
             "User-Agent": "arbyte-glm-compat-check/1"})
started = time.monotonic()
check = {"text": "", "done": False, "finished": False, "actual_models": [],
         "omp_tool_count": len(schemas), "usage": None}
try:
    with urllib.request.urlopen(req, timeout=100) as r:
        check["status"] = r.status
        read = 0
        for line in r:
            read += len(line)
            if read > 128 * 1024:
                raise RuntimeError("Unexpectedly large check response")
            if not line.startswith(b"data: "):
                continue
            frame = line[6:].strip()
            if frame == b"[DONE]":
                check["done"] = True
                continue
            v = json.loads(frame)
            if v.get("model") and v["model"] not in check["actual_models"]:
                check["actual_models"].append(v["model"])
            if v.get("usage"):
                check["usage"] = v["usage"]
            if v.get("error"):
                check["error"] = v["error"]
            for choice in v.get("choices", []):
                check["text"] += choice.get("delta", {}).get("content") or ""
                if choice.get("finish_reason"):
                    check["finished"] = True
except urllib.error.HTTPError as e:
    check.update({"status": e.code, "error": e.read(2048).decode(errors="replace")})
check["elapsed_ms"] = round((time.monotonic() - started) * 1000)
check["ok"] = (check["text"].strip() == "GLM_AGENT_OK" and check["done"] and check["finished"]
               and check["actual_models"] == [model] and bool(check["usage"]) and not check.get("error"))
report["checks"]["omp_request"] = check
finish(None if check["ok"] else "GLM request with captured OMP schemas failed")
