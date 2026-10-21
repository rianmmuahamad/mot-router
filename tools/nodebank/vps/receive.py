#!/usr/bin/env python3
"""Validate one laptop NodeBank export from stdin and atomically store it."""

import hashlib
import ipaddress
import json
import os
import pathlib
import sys
import tempfile
import time

MAX_BYTES = 32 * 1024 * 1024
MAX_NODES = 5000
PROTOCOLS = {"vless", "vmess", "trojan", "shadowsocks", "http", "socks"}


def fail(message):
    print(json.dumps({"ok": False, "error": message}), file=sys.stderr)
    raise SystemExit(2)


def main():
    if len(sys.argv) != 3 or sys.argv[1] != "--output":
        fail("usage: receive.py --output PATH")
    raw = sys.stdin.buffer.read(MAX_BYTES + 1)
    if len(raw) > MAX_BYTES:
        fail("snapshot exceeds 32 MiB")
    try:
        value = json.loads(raw)
    except (UnicodeDecodeError, json.JSONDecodeError):
        fail("invalid JSON snapshot")
    if not isinstance(value, dict) or value.get("schema_version") != 1:
        fail("unsupported schema version")
    if value.get("vantage") != "laptop" or value.get("zen_verified") is not False:
        fail("snapshot provenance is invalid")
    now = int(time.time())
    generated = value.get("generated_at")
    nodes = value.get("nodes")
    if not isinstance(generated, int) or generated > now + 300 or generated < now - 3600:
        fail("snapshot is stale or has an invalid timestamp")
    if not isinstance(nodes, list) or not 1 <= len(nodes) <= MAX_NODES:
        fail(f"snapshot must contain between 1 and {MAX_NODES} candidates")

    ids, exits = set(), set()
    for node in nodes:
        if not isinstance(node, dict):
            fail("candidate must be an object")
        node_id, config = node.get("id"), node.get("config")
        if not isinstance(node_id, str) or not 1 <= len(node_id) <= 128 or node_id in ids:
            fail("candidate id is missing or duplicated")
        ids.add(node_id)
        if not isinstance(config, dict) or config.get("type") not in PROTOCOLS:
            fail("candidate protocol is unsupported")
        server, port = config.get("server"), config.get("server_port")
        if not isinstance(server, str) or not server or len(server) > 253:
            fail("candidate server is invalid")
        if not isinstance(port, int) or isinstance(port, bool) or not 1 <= port <= 65535:
            fail("candidate port is invalid")
        latency = node.get("latency_ms")
        checked = node.get("checked_at")
        if not isinstance(latency, (int, float)) or isinstance(latency, bool) or not 0 <= latency <= 3000:
            fail("candidate latency is invalid")
        if not isinstance(checked, int) or checked < now - 3600 or checked > now + 300:
            fail("candidate health result is stale")
        try:
            exit_ip = str(ipaddress.ip_address(node.get("exit_ip", "")))
        except ValueError:
            fail("candidate exit IP is invalid")
        if exit_ip in exits:
            fail("snapshot contains duplicate exit IPs")
        exits.add(exit_ip)

    output = pathlib.Path(sys.argv[2])
    output.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
    os.chmod(output.parent, 0o700)
    fd, temp_name = tempfile.mkstemp(prefix=".nodebank-", suffix=".partial", dir=output.parent)
    try:
        with os.fdopen(fd, "wb") as file:
            os.fchmod(file.fileno(), 0o600)
            file.write(raw)
            file.flush()
            os.fsync(file.fileno())
        os.replace(temp_name, output)
        os.chmod(output, 0o600)
        dir_fd = os.open(output.parent, os.O_RDONLY | getattr(os, "O_DIRECTORY", 0))
        try:
            os.fsync(dir_fd)
        finally:
            os.close(dir_fd)
    finally:
        try:
            os.unlink(temp_name)
        except FileNotFoundError:
            pass
    print(json.dumps({"ok": True, "count": len(nodes), "generated_at": generated,
                      "received_at": now, "sha256": hashlib.sha256(raw).hexdigest()}))


if __name__ == "__main__":
    main()
