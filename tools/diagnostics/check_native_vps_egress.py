#!/usr/bin/env python3
"""Run the installed official client via a temporary SSH-forwarded VPS proxy."""
import json
import os
import pathlib
import subprocess
import sys
import tempfile

root = pathlib.Path(tempfile.mkdtemp(prefix="mot-native-vps-egress-"))
model = sys.argv[1] if len(sys.argv) > 1 else "glm-5.3-flash-free"
direct = "--direct" in sys.argv[2:]
expected = "DEEPSEEK_NATIVE_VPS_OK" if model == "deepseek-v4.1-flash-free" else "GLM_NATIVE_VPS_OK"
config = {"update": "disable", "snapshots": False, "warming": False,
          "model": "opencode/" + model,
          "providers": {"opencode": {"settings": {"apiKey": "public"},
              "models": {model: {"modelID": model, "limit": {"context": 1048576, "output": 256}}}}}}
(root / "opencode.json").write_text(json.dumps(config))
env = {k: v for k, v in os.environ.items() if k in {"PATH", "LANG", "LC_ALL", "TERM", "TMPDIR"}}
env.update({"XDG_DATA_HOME": str(root / "data"), "XDG_CONFIG_HOME": str(root / "config"),
            "XDG_CACHE_HOME": str(root / "cache"), "OPENCODE_CONFIG": str(root / "opencode.json"),
            "OPENCODE_DISABLE_MODELS_FETCH": "true", "OPENCODE_DISABLE_AUTOUPDATE": "true",
            "NO_PROXY": "127.0.0.1,localhost,::1", "no_proxy": "127.0.0.1,localhost,::1"})
if not direct:
    env.update({"HTTPS_PROXY": "http://127.0.0.1:18893", "HTTP_PROXY": "http://127.0.0.1:18893"})
binary = "/home/rian/.npm-global/bin/opencode"
report = {"model": model, "egress": "laptop direct" if direct else "VPS current NodeBank node via SSH forward",
          "version": subprocess.check_output([binary, "--version"], env=env, cwd=root, text=True).strip()}
try:
    run = subprocess.run([binary, "run", "--standalone", "--format", "json", "--title", "GLM compatibility audit",
                          "-m", "opencode/" + model, "Reply exactly " + expected + ". Do not call tools."],
                         env=env, cwd=root, capture_output=True, text=True, timeout=45)
    report.update({"exit_code": run.returncode, "text": "", "errors": []})
    for line in run.stdout.splitlines():
        try:
            event = json.loads(line)
        except ValueError:
            continue
        if event.get("type") == "text":
            report["text"] += event.get("part", {}).get("text", "")
        elif event.get("type") == "error":
            report["errors"].append(event.get("error"))
    report["stderr"] = run.stderr[-1200:]
    report["ok"] = run.returncode == 0 and report["text"].strip() == expected
except subprocess.TimeoutExpired:
    report.update({"ok": False, "error": "Official client did not complete within 45 seconds"})
suffix = "-deepseek" if model == "deepseek-v4.1-flash-free" else ""
if direct:
    suffix += "-laptop-direct"
dest = pathlib.Path(__file__).resolve().parent / ("results/opencode-native-vps-egress" + suffix + "-2026-10-17.json")
dest.write_text(json.dumps(report, indent=2) + "\n")
print(json.dumps(report, indent=2))
