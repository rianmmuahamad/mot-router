"""Bounded inference proof; no model-generated tool calls are executed."""
import json
import sqlite3
import time

from recovery_core import classify


def tools():
    return [
        {'name': 'Bash', 'description': 'Client shell helper; do not invoke in this verification.',
         'input_schema': {'type': 'object', 'properties': {'command': {'type': 'string'}}, 'required': ['command']}},
        {'name': 'Read', 'description': 'Client file reader; do not invoke in this verification.',
         'input_schema': {'type': 'object', 'properties': {'file_path': {'type': 'string'}}, 'required': ['file_path']}},
        {'name': 'Edit', 'description': 'Client editor; do not invoke in this verification.',
         'input_schema': {'type': 'object', 'properties': {k: {'type': 'string'} for k in ('file_path', 'old_string', 'new_string')},
                          'required': ['file_path', 'old_string', 'new_string']}},
        {'name': 'mcp_test_ping', 'description': 'Verification declaration only; do not invoke.',
         'input_schema': {'type': 'object', 'properties': {}}}]


def anthropic_stream(raw):
    text, started, stopped, error = '', False, False, False
    usage = {'input_tokens': None, 'output_tokens': None}
    for line in raw.decode(errors='replace').splitlines():
        if not line.startswith('data: '):
            continue
        try:
            event = json.loads(line[6:])
        except json.JSONDecodeError:
            continue
        kind = event.get('type')
        if kind == 'message_start':
            started = True
            for key, value in event.get('message', {}).get('usage', {}).items():
                if key in usage and type(value) is int:
                    usage[key] = max(value, usage[key] or 0)
        elif kind == 'content_block_delta' and event.get('delta', {}).get('type') == 'text_delta':
            text += event['delta'].get('text', '')
        elif kind == 'message_delta':
            for key, value in event.get('usage', {}).items():
                if key in usage and type(value) is int:
                    usage[key] = max(value, usage[key] or 0)
        elif kind == 'message_stop':
            stopped = True
        elif kind == 'error':
            error = True
    return {'ok': started and stopped and not error and text.strip() == 'AGENT_OK',
            'complete_stream': started and stopped and not error, 'usage': usage,
            'usage_reported': all(v is not None for v in usage.values()) and (usage['input_tokens'] or 0) > 0}


def verify_nonstream(key):
    """NullClaw uses OpenAI JSON mode. The Anthropic compatibility surface is SSE."""
    from recovery_broker import request
    definitions = [
        {'name': 'shell', 'description': 'Run a bounded operator command; do not call in this probe.',
         'parameters': {'type': 'object', 'properties': {'command': {'type': 'string'}}, 'required': ['command']}},
        {'name': 'file_read', 'description': 'Read a workspace file; do not call in this probe.',
         'parameters': {'type': 'object', 'properties': {'path': {'type': 'string'}}, 'required': ['path']}},
        {'name': 'file_write', 'description': 'Write a workspace file; do not call in this probe.',
         'parameters': {'type': 'object', 'properties': {'path': {'type': 'string'}, 'content': {'type': 'string'}}, 'required': ['path', 'content']}},
        {'name': 'memory_recall', 'description': 'Recall agent memory; do not call in this probe.',
         'parameters': {'type': 'object', 'properties': {'query': {'type': 'string'}}, 'required': ['query']}}]
    body = {'model': 'muse-spark-1.3-contributor-free', 'stream': False, 'max_tokens': 1024,
            'messages': [{'role': 'user', 'content': 'Reply exactly AGENT_OK. Do not call tools.'}],
            'tools': [{'type': 'function', 'function': item} for item in definitions]}
    code, raw = request('http://127.0.0.1:8787/v1/chat/completions', body,
                        {'Authorization': 'Bearer ' + key}, timeout=100)
    try:
        data = json.loads(raw)
    except ValueError:
        return {'ok': False, 'status': code, 'surface': 'openai', 'reason': 'Non-stream JSON response missing'}
    choices = data.get('choices', [])
    message = choices[0].get('message', {}) if choices else {}
    usage = data.get('usage', {})
    return {'status': code, 'surface': 'openai', 'declared_tools': 4,
            'ok': code == 200 and str(message.get('content', '')).strip() == 'AGENT_OK'
                  and type(usage.get('prompt_tokens')) is int and usage['prompt_tokens'] > 0
                  and type(usage.get('completion_tokens')) is int,
            'usage': usage}


def verify():
    from recovery_broker import request
    result = {'ok': False, 'checks': {}, 'checked_at': int(time.time()), 'measurement': 'vps'}
    checks = result['checks']
    try:
        code, raw = request('http://127.0.0.1:8787/api/gate')
        profile = json.loads(raw)
        result['profile'] = {k: profile.get(k) for k in ('source', 'checked_at', 'upstream_url', 'client_format', 'verified_version', 'refreshing')}
        code, raw = request('http://127.0.0.1:8787/api/gate/test', {}, timeout=70)
        data = json.loads(raw)
        checks['baseline'] = {'ok': code == 200 and data.get('ok') is True and data.get('status') == 200
                                  and data.get('text', '').strip() == 'GATE_OK', 'status': data.get('status', code)}
        if not checks['baseline']['ok']:
            checks['baseline']['classification'] = classify(data.get('status', code), str(data.get('error', '')))
            return result
        code, raw = request('http://127.0.0.1:8787/api/playground',
                            {'model': 'opencode-zen/muse-spark-1.3-contributor-free',
                             'prompt': 'Reply exactly PLAYGROUND_OK. Do not call tools.'}, timeout=100)
        data = json.loads(raw)
        checks['playground'] = {'ok': code == 200 and not data.get('error') and data.get('text', '').strip() == 'PLAYGROUND_OK',
                                'usage': data.get('usage'), 'status': code}
        if not checks['playground']['ok']:
            checks['playground']['classification'] = classify(502, str(data.get('error', '')))
            return result
        with sqlite3.connect('file:/opt/mot-router/data/mot-router.db?mode=ro', uri=True, timeout=10) as db:
            row = db.execute("SELECT key FROM api_keys WHERE name='Arbyte NullClaw Ops' AND revoked=0 LIMIT 1").fetchone()
        if not row:
            result['reason'] = 'Dedicated operations key unavailable'
            return result
        headers = {'x-api-key': row[0], 'anthropic-version': '2023-06-01'}
        body = {'model': 'muse-spark-1.3-contributor-free', 'max_tokens': 1024, 'stream': True,
                'messages': [{'role': 'user', 'content': 'Reply exactly AGENT_OK. Do not call tools.'}], 'tools': tools()}
        code, raw = request('http://127.0.0.1:8787/v1/messages', body, headers, timeout=100)
        checks['agent_stream'] = {'status': code, **anthropic_stream(raw)}
        if code != 200 or not checks['agent_stream']['ok'] or not checks['agent_stream']['usage_reported']:
            checks['agent_stream']['ok'] = False
            checks['agent_stream']['classification'] = classify(code, raw.decode(errors='replace'))
            return result
        checks['agent_nonstream'] = verify_nonstream(row[0])
        result['ok'] = all(c.get('ok') for c in checks.values())
    except Exception as e:
        result['reason'] = type(e).__name__ + '; verification did not complete'
    return result
