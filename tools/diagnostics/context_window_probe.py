#!/usr/bin/env python3
"""Bounded, sequential probes via local Mot Router. No paid models or retries.
Run on the VPS. A temporary router key is created and deleted in finally.
Only a healthy model with measured usage advances to a ~1M-token request.
Synthetic repetition tests acceptance and three markers, not general recall quality.
"""
import argparse
import json
import math
import os
import secrets
import time
import urllib.error
import urllib.request
from pathlib import Path


def admin(base, path, data=None, method=None):
    raw = None if data is None else json.dumps(data).encode()
    req = urllib.request.Request(base + path, data=raw, method=method,
                                 headers={'Content-Type': 'application/json'})
    with urllib.request.urlopen(req, timeout=15) as response:
        return json.load(response)


def probe(base, key, model, count):
    markers = [secrets.token_hex(4) for _ in range(3)]
    half = count // 2
    prompt = ('This is a context acceptance test. Read the three MARKER values in this data. '
              'Reply only with the three values in order. Do not use tools.\n'
              'MARKER_START=' + markers[0] + '\n' + ' a' * half +
              '\nMARKER_MIDDLE=' + markers[1] + '\n' + ' a' * (count-half) +
              '\nMARKER_END=' + markers[2] + '\nReturn the three marker values, in order.')
    payload = json.dumps({'model': 'opencode-zen/' + model, 'stream': True,
                          'max_tokens': 512, 'reasoning_effort': 'low',
                          'messages': [{'role': 'user', 'content': prompt}]}).encode()
    result = {'model': model, 'repetitions': count, 'request_bytes': len(payload)}
    print(json.dumps({'starting': result}), flush=True)
    request = urllib.request.Request(base + '/v1/chat/completions', data=payload,
                                    headers={'Content-Type': 'application/json',
                                             'Authorization': 'Bearer ' + key})
    started = time.monotonic()
    usage, text, done = None, '', False
    try:
        with urllib.request.urlopen(request, timeout=240) as response:
            result['http_status'] = response.status
            received = 0
            for line in response:
                received += len(line)
                if received > 2 * 1024 * 1024:
                    raise RuntimeError('response exceeded 2 MiB safety cap')
                if time.monotonic() - started > 240:
                    raise TimeoutError('probe exceeded 240 seconds')
                if not line.startswith(b'data:'):
                    continue
                raw = line[5:].strip()
                if raw == b'[DONE]':
                    done = True
                    break
                event = json.loads(raw)
                if event.get('error'):
                    result['error'] = event['error']
                if event.get('usage'):
                    # Each report is cumulative, never sum repeated usage frames.
                    usage = event['usage']
                if event.get('model'):
                    result['response_model'] = event['model']
                for choice in event.get('choices', []):
                    text += choice.get('delta', {}).get('content') or ''
    except urllib.error.HTTPError as exc:
        result['http_status'] = exc.code
        result['error'] = exc.read(8192).decode(errors='replace')
    except Exception as exc:
        result['error'] = str(exc)
    result.update(elapsed_s=round(time.monotonic()-started, 2), usage=usage,
                  stream_done=done, text=text[:2048],
                  markers_found=[marker in text for marker in markers])
    result['accepted'] = bool(done and not result.get('error') and usage and
                              usage.get('prompt_tokens', 0) > 0)
    print(json.dumps(result), flush=True)
    return result


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--base', default='http://127.0.0.1:8787')
    parser.add_argument('--output', default='/tmp/arbyte-context-window-results.json')
    parser.add_argument('--model', action='append', choices=[
        'deepseek-v4.1-flash-free', 'glm-5.3-flash-free'],
        help='Only probe the selected model; repeat to select both')
    args = parser.parse_args()
    os.umask(0o077)
    report = {'measurement': 'Mot Router -> OpenCode Zen free IDs',
              'created_at_utc': time.strftime('%Y-%m-%dT%H:%M:%SZ', time.gmtime()),
              'target_input_tokens': 1005000, 'results': []}
    key = admin(args.base, '/api/keys', {'name': 'context-window-diagnostic'})
    try:
        for model in args.model or ['deepseek-v4.1-flash-free', 'glm-5.3-flash-free']:
            first = probe(args.base, key['key'], model, 1024)
            report['results'].append(first)
            if not first['accepted']:
                continue
            second = probe(args.base, key['key'], model, 2048)
            report['results'].append(second)
            if not second['accepted']:
                continue
            slope = (second['usage']['prompt_tokens'] - first['usage']['prompt_tokens']) / 1024
            if not 0.25 <= slope <= 4:
                second['calibration_error'] = 'unstable or implausible token counts'
                continue
            overhead = first['usage']['prompt_tokens'] - slope*1024
            count = math.ceil((1005000-overhead)/slope)
            if not 0 < count <= 3000000:
                continue
            large = probe(args.base, key['key'], model, count)
            large['estimated_input_tokens'] = 1005000
            large['million_input_confirmed'] = bool(large['accepted'] and
                large['usage']['prompt_tokens'] >= 1000000)
            report['results'].append(large)
    finally:
        admin(args.base, '/api/keys/' + str(key['id']), method='DELETE')
        Path(args.output).write_text(json.dumps(report, indent=2))
        print(json.dumps({'saved': args.output, 'temporary_key_deleted': True}), flush=True)


if __name__ == '__main__':
    main()
