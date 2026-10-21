#!/usr/bin/python3
"""Bounded operations for the Arbyte agent; never exposes proxy credentials."""
import argparse
from datetime import datetime, timezone, timedelta
import fcntl
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import time
import urllib.error
import urllib.parse
import urllib.request

sys.path.insert(0, '/opt/arbyte-agent')

HOME = Path(os.environ.get('ARBYTE_AGENT_STATE', '/var/lib/arbyte-agent/ops'))
SERVICES = ('mot-router', 'motgate', 'guard', 'mot-nodebank',
            'mot-nodebank-runtime', 'mot-dashboard', 'mot-cftunnel', 'caddy', 'arbyte-agent')
BANK = 'http://127.0.0.1:8792'
RUNTIME = 'http://127.0.0.1:9093'
HTTP = urllib.request.build_opener(urllib.request.ProxyHandler({}))


def api(base, path, body=None):
    req = urllib.request.Request(base + path,
        data=None if body is None else json.dumps(body).encode(),
        headers={'Content-Type': 'application/json'})
    try:
        with HTTP.open(req, timeout=8) as r:
            raw = r.read(256 * 1024 + 1)
            if len(raw) > 256 * 1024:
                raise RuntimeError('API response exceeds limit')
            return json.loads(raw)
    except urllib.error.HTTPError as e:
        raise RuntimeError('Local API HTTP %s' % e.code) from None
    except (urllib.error.URLError, TimeoutError):
        raise RuntimeError('Local API unavailable') from None


def save(path, data):
    tmp = path.with_suffix('.partial')
    with open(tmp, 'w', encoding='utf-8') as f:
        os.chmod(tmp, 0o600)
        json.dump(data, f, ensure_ascii=False)
        f.write('\n')
        f.flush()
        os.fsync(f.fileno())
    os.replace(tmp, path)


def load(path, default):
    try:
        return json.loads(path.read_text())
    except FileNotFoundError:
        return default


def event(kind, result):
    p = HOME / 'actions.jsonl'
    if p.exists() and p.stat().st_size > 4 * 1024 * 1024:
        os.replace(p, HOME / 'actions.previous.jsonl')
    with open(p, 'a', encoding='utf-8') as f:
        os.chmod(p, 0o600)
        f.write(json.dumps({'time': int(time.time()), 'action': kind,
                            'result': result}, ensure_ascii=False) + '\n')


def memory():
    values = {}
    for line in Path('/proc/meminfo').read_text().splitlines():
        k, v = line.split(':', 1)
        values[k] = int(v.strip().split()[0]) * 1024
    return {'total_bytes': values['MemTotal'],
            'available_bytes': values['MemAvailable'],
            'swap_used_bytes': values['SwapTotal'] - values['SwapFree']}


def service_states():
    result = {}
    for name in SERVICES:
        p = subprocess.run(['/bin/systemctl', 'is-active', name],
            capture_output=True, text=True, timeout=5)
        result[name] = p.stdout.strip() or 'unknown'
    return result


def cpu():
    # /proc/stat already aggregates all CPUs. Exclude guest fields (double counted).
    with open('/proc/stat') as f:
        values = [int(v) for v in f.readline().split()[1:9]]
    frame = {'total': sum(values), 'idle': values[3] + values[4], 'time': int(time.time())}
    previous = load(HOME / 'cpu-sample.json', {})
    save(HOME / 'cpu-sample.json', frame)
    total = frame['total'] - previous.get('total', frame['total'])
    idle = frame['idle'] - previous.get('idle', frame['idle'])
    return {'usage_percent': round(100 * (1 - idle / total), 1) if total > 0 else None,
            'sample_seconds': max(0, frame['time'] - previous.get('time', frame['time'])),
            'logical_cpus': os.cpu_count()}


def status():
    errors = []
    try:
        stats = api(BANK, '/api/stats')
    except RuntimeError as e:
        stats = {}; errors.append('bank: ' + str(e))
    try:
        runtime = api(RUNTIME, '/status')
    except RuntimeError as e:
        runtime = {}; errors.append('runtime: ' + str(e))
    disk = shutil.disk_usage(HOME)
    services = service_states()
    return {'time': int(time.time()),
        'time_wib': datetime.now(timezone(timedelta(hours=7))).isoformat(),
        'memory': memory(), 'cpu': cpu(),
        'load_average': os.getloadavg(), 'disk_free_bytes': disk.free,
        'services': services,
        'bank': {k: stats.get(k) for k in ('counts', 'paused', 'pause_reason',
            'background', 'combined_rss_bytes', 'checked_since_start',
            'average_nodes_per_second', 'tcp_parallelism', 'protocol_parallelism')},
        'runtime': {k: runtime.get(k) for k in ('counts', 'total', 'current', 'candidate_cap',
            'unique_healthy_exit_ips', 'target_gap',
            'pause_reason', 'source_error', 'active_connections')},
        'errors': errors,
        'warnings': ([name + ' is ' + state for name, state in services.items()
                    if state != 'active'] +
                    (['RAM available below 768 MiB'] if memory()['available_bytes'] < 768 * 1024**2 else []) +
                    (['Disk free below 2 GiB'] if disk.free < 2 * 1024**3 else []))}


def jobs():
    return api(BANK, '/api/jobs').get('jobs', [])


def scan(action):
    policy = load(HOME / 'policy.json', {'auto_scan': True})
    policy['auto_scan'] = action != 'pause'
    save(HOME / 'policy.json', policy)
    if action != 'pause' and memory()['available_bytes'] < 900 * 1024**2:
        return {'deferred': 'available RAM below 900 MiB'}
    current = api(BANK, '/api/stats')
    candidates = [j for j in jobs() if j.get('kind') == 'scan' and
                  j.get('state') in ('running', 'paused')]
    if action == 'pause':
        for j in candidates:
            if j['state'] == 'running':
                api(BANK, '/api/jobs/' + urllib.parse.quote(j['id'], safe='') + '/control', {'action': 'pause'})
        return {'paused': True, 'auto_scan': False}
    if not current.get('paused') and current.get('background'):
        return {'already_running': True}
    # An existing manual job cannot be changed into a background job by resume.
    for j in candidates:
        api(BANK, '/api/jobs/' + urllib.parse.quote(j['id'], safe='') + '/control', {'action': 'cancel'})
    return api(BANK, '/api/scan', {'background': True})


def refresh():
    if memory()['available_bytes'] < 900 * 1024**2:
        return {'deferred': 'available RAM below 900 MiB'}
    if shutil.disk_usage(HOME).free < 2 * 1024**3:
        return {'deferred': 'disk free below 2 GiB'}
    active = [j for j in jobs() if j.get('kind') == 'import' and
              j.get('state') in ('downloading', 'queued', 'running', 'paused')]
    if active:
        return {'deferred': 'an import job is unfinished'}
    state = load(HOME / 'source-attempts.json', {})
    now = int(time.time())
    sources = api(BANK, '/api/sources').get('sources', [])
    due = [s for s in sources if s.get('enabled') and
           now - int(s.get('last_fetch') or 0) >= max(3600, int(s.get('interval_secs') or 86400)) and
           now - state.get(s['id'], 0) >= 3600]
    if not due:
        return {'up_to_date': True}
    source = min(due, key=lambda s: (state.get(s['id'], 0), s.get('last_fetch') or 0, s['id']))
    state[source['id']] = now
    save(HOME / 'source-attempts.json', state)
    result = api(BANK, '/api/sources/' + urllib.parse.quote(source['id'], safe='') + '/import', {})
    return {'source_id': source['id'], **result}


def source_enabled(source_id, enabled):
    sources = api(BANK, '/api/sources').get('sources', [])
    source = next((s for s in sources if s['id'] == source_id), None)
    if source is None:
        raise RuntimeError('Unknown source ID; list sources first')
    result = api(BANK, '/api/sources/' + urllib.parse.quote(source_id, safe='') + '/enabled',
                 {'enabled': bool(enabled)})
    return {'source_id': source_id, 'name': source['name'], 'enabled': result['enabled'],
            'import_started': False,
            'note': 'Only future source imports changed; existing nodes and jobs retained'}


def maintain():
    snapshot = status()
    save(HOME / 'latest-status.json', snapshot)
    result = {'status': snapshot, 'operations': []}
    if snapshot['errors']:
        return result
    policy = load(HOME / 'policy.json', {'auto_scan': True})
    bank = snapshot['bank']
    if not policy.get('auto_scan', True) and not bank.get('paused') and bank.get('background'):
        # A dashboard user explicitly resumed background work after a manual pause.
        policy['auto_scan'] = True
        save(HOME / 'policy.json', policy)
    if policy.get('auto_scan', True) and bank.get('paused'):
        reason = bank.get('pause_reason') or ''
        if reason == 'Paused by user':
            policy['auto_scan'] = False
            save(HOME / 'policy.json', policy)
            result['operations'].append({'scan': 'manual pause respected'})
        elif reason in ('Paused on startup', 'Not started', ''):
            result['operations'].append({'scan': scan('resume')})
        else:
            attempts = load(HOME / 'recovery.json', {})
            if time.time() - attempts.get('scan', 0) >= 900:
                attempts['scan'] = int(time.time())
                save(HOME / 'recovery.json', attempts)
                result['operations'].append({'scan': scan('resume')})
    result['operations'].append({'sources': refresh()})
    return result


def run(args):
    if args.command == 'repair':
        from recovery_client import call
        payload = {'action': args.action, 'component': args.component}
        if args.job_id:
            payload['job_id'] = args.job_id
        if args.reason:
            payload['reason'] = args.reason
        if args.account_id is not None:
            payload['account_id'] = args.account_id
        if args.operation:
            payload['operation'] = args.operation
        if args.changes:
            payload['changes'] = json.loads(args.changes)
        return call(payload)
    if args.command in ('status', 'monitor'):
        data = status()
        if args.command == 'monitor':
            save(HOME / 'latest-status.json', data)
        return data
    if args.command == 'maintain':
        return maintain()
    if args.command == 'scan':
        return scan(args.action)
    if args.command == 'sources':
        if args.action == 'refresh':
            return refresh()
        if args.action in ('enable', 'disable'):
            return source_enabled(args.source_id, args.action == 'enable')
        return {'sources': [{k: s.get(k) for k in ('id', 'name', 'enabled', 'interval_secs', 'last_fetch')}
                           for s in api(BANK, '/api/sources').get('sources', [])]}
    if args.command == 'jobs':
        return {'jobs': [{k: j.get(k) for k in ('id', 'kind', 'state', 'processed', 'total', 'ok', 'failed', 'unsupported')}
                         for j in jobs()][:30]}
    if args.command == 'nodes':
        if args.action == 'sync':
            # Runtime automatically pulls the local bank each minute.
            runtime = status()['runtime']
            return {'automatic_pull_seconds': 60, 'candidate_cap': runtime.get('candidate_cap'),
                    'runtime': runtime}
        return api(BANK, '/api/best')
    if args.command == 'export':
        return api(BANK, '/api/export', {})
    raise RuntimeError('Unknown operation')


def main():
    os.umask(0o077)
    parser = argparse.ArgumentParser(description=__doc__)
    sub = parser.add_subparsers(dest='command', required=True)
    for name in ('status', 'monitor', 'maintain', 'jobs', 'export'):
        sub.add_parser(name)
    sub.add_parser('scan').add_argument('action', choices=('pause', 'resume', 'start'))
    source_parser = sub.add_parser('sources')
    source_parser.add_argument('action', choices=('list', 'refresh', 'enable', 'disable'))
    source_parser.add_argument('source_id', nargs='?')
    sub.add_parser('nodes').add_argument('action', choices=('best', 'sync'))
    repair = sub.add_parser('repair')
    repair.add_argument('action', choices=('inspect', 'prepare', 'test', 'deploy', 'rollback', 'status', 'verify', 'restart', 'config', 'account', 'credential'))
    repair.add_argument('job_id', nargs='?')
    repair.add_argument('--component', choices=('mot-router', 'motgate', 'guard', 'mot-nodebank',
                        'mot-nodebank-runtime', 'nodebank-probe-runner', 'caddy'), default='mot-router')
    repair.add_argument('--reason')
    repair.add_argument('--account-id', type=int)
    repair.add_argument('--operation', choices=('revoke', 'restore'))
    repair.add_argument('--changes', help='JSON of bounded environment settings; never credentials')
    args = parser.parse_args()
    if args.command == 'sources':
        if args.action in ('enable', 'disable') and not args.source_id:
            parser.error('enable/disable requires one existing source ID')
        if args.action in ('list', 'refresh') and args.source_id:
            parser.error('list/refresh does not accept a source ID')
    HOME.mkdir(mode=0o700, parents=True, exist_ok=True)
    with open(HOME / 'operation.lock', 'a') as lock:
        try:
            fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError:
            print(json.dumps({'deferred': 'another operation is active'})); return
        try:
            result = run(args)
            if args.command not in ('status', 'jobs'):
                event(args.command, result)
            print(json.dumps(result, ensure_ascii=False))
        except RuntimeError as e:
            if args.command == 'repair':
                event('repair', {'error': 'recovery operation rejected'})
                print(json.dumps({'error': str(e)})); raise SystemExit(1)
            event(args.command, {'error': 'operation failed; check service journal'})
            print(json.dumps({'error': 'operation failed; check service journal'}))
            raise SystemExit(1)
        except Exception:
            # Upstream error bodies/URLs can contain subscription secrets.
            event(args.command, {'error': 'operation failed; check service journal'})
            print(json.dumps({'error': 'operation failed; check service journal'}))
            raise SystemExit(1)


if __name__ == '__main__':
    main()
