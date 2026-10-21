"""Bounded JSON-line client for the private recovery broker."""
import json
import socket


def call(data):
    raw = json.dumps(data).encode() + b'\n'
    if len(raw) > 32768:
        raise RuntimeError('Recovery request exceeds limit')
    with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as sock:
        sock.settimeout(15)
        sock.connect('/run/arbyte-repair/broker.sock')
        sock.sendall(raw)
        with sock.makefile('rb') as reader:
            result = reader.readline(131073)
    if len(result) > 131072 or not result.endswith(b'\n'):
        raise RuntimeError('Invalid recovery broker response')
    data = json.loads(result)
    if data.get('error'):
        raise RuntimeError(data['error'])
    return data
