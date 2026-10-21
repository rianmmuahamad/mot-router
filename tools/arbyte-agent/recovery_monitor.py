#!/usr/bin/python3
"""Minute-level monitoring and tested procedures; independent from the LLM."""
import collections
import fcntl
import json
import hashlib
import re
from pathlib import Path
import sqlite3
import subprocess
import time
import urllib.parse

from recovery_core import State, classify
from recovery_broker import ROOT, BACKUPS, WORKSPACES, COMPONENTS, HTTP, healthy, request, resources
from recovery_client import call


def failures(state, now):
    problems = []
    cursor = state.value('request_cursor')
    try:
        with sqlite3.connect('file:/opt/mot-router/data/mot-router.db?mode=ro', uri=True, timeout=5) as db:
            highest = db.execute('SELECT COALESCE(MAX(id),0) FROM requests').fetchone()[0]
            if cursor is None:
                # Start from current traffic, never diagnose stale historical errors on install.
                state.set_value('request_cursor', highest)
                return [], True
            rows = db.execute('SELECT id,status,error,provider,model FROM requests WHERE id>? AND ts>? ORDER BY id LIMIT 100',
                              (cursor, (now - 300) * 1000)).fetchall()
            def route(provider, model):
                return hashlib.sha256((str(provider) + '\0' + str(model)).encode()).hexdigest()[:16]
            groups = collections.Counter(route(provider, model) + ':' + classify(status, detail or '')
                                         for _, status, detail, provider, model in rows if status >= 400)
            state.set_value('request_cursor', highest if len(rows) < 100 else rows[-1][0])
            recent = state.value('failure_window', [])
            recent = [item for item in recent if item['time'] > now - 300]
            if groups:
                recent.append({'time': now, 'counts': dict(groups)})
            state.set_value('failure_window', recent)
            total = collections.Counter()
            for frame in recent:
                total.update(frame['counts'])
            unresolved = state.value('unresolved_inference', {})
            # Idle traffic is not evidence of recovery. Clear only after a later success or live proof.
            unresolved = {k: v for k, v in unresolved.items() if 'route' in v}
            for request_id, status, detail, provider, model in rows:
                scope = route(provider, model)
                if status < 400:
                    unresolved = {k: v for k, v in unresolved.items() if v['route'] != scope}
                else:
                    kind = classify(status, detail or '')
                    group = scope + ':' + kind
                    if total[group] >= 2:
                        unresolved[group] = {'kind': kind, 'route': scope, 'last_error_id': request_id,
                                             'last_error_at': now, 'failures_last_5m': total[group],
                                             'provider': provider if provider in ('opencode-zen', 'codebuddy', 'cline', 'tokenharbor') else 'other',
                                             'model': model if re.fullmatch(r'[a-zA-Z0-9_/.:\-]{1,100}', model or '') else 'redacted',
                                             'status': status}
            proof = state.value('last_live_verification', {})
            unresolved = {kind: e for kind, e in unresolved.items()
                          if not (proof.get('ok') and proof.get('time', 0) > e['last_error_at']
                                  and e['provider'] == 'opencode-zen' and e['model'] == 'muse-spark-1.3-contributor-free')}
            state.set_value('unresolved_inference', unresolved)
            for group, evidence in unresolved.items():
                problems.append({'kind': evidence['kind'], 'component': 'mot-router', 'scope': evidence['route'], 'evidence': evidence})
    except (sqlite3.Error, OSError):
        return [], False
    return problems, True


def collect(state):
    now = int(time.time())
    problems, logs_ok = failures(state, now)
    component_health = {name: healthy(name) for name in COMPONENTS if name != 'nodebank-probe-runner'}
    for name, ok in component_health.items():
        if not ok:
            problems.append({'kind': 'service_unhealthy', 'component': name, 'evidence': {'health': False}})
    # NullClaw itself is monitored without making a model request.
    if subprocess.run(['/bin/systemctl', 'is-active', '--quiet', 'arbyte-agent'], timeout=5).returncode:
        problems.append({'kind': 'agent_unhealthy', 'component': 'arbyte-agent'})
    if not logs_ok and component_health['mot-router']:
        problems.append({'kind': 'telemetry_unavailable', 'component': 'mot-router'})
    if component_health['mot-nodebank-runtime']:
        try:
            _, raw = request('http://127.0.0.1:9093/status')
            d = json.loads(raw)
            # Pending checks/short banks are not sync failure. Only actual pull failure is evidence.
            if d.get('source_error'):
                problems.append({'kind': 'node_sync', 'component': 'mot-nodebank-runtime',
                                 'evidence': {'source_error_present': True, 'total': d.get('total')}})
            state.set_value('nodebank', {k: d.get(k) for k in ('total', 'unique_healthy_exit_ips', 'target_gap', 'counts')})
        except Exception:
            pass
    limits = resources()
    state.set_value('monitor', {'time': now, 'services': component_health, 'resources': limits})
    journal_summary(state)
    return problems


def journal_summary(state):
    """Bounded corroborating evidence. Raw log lines remain on the VPS."""
    try:
        p = subprocess.Popen(['/bin/journalctl', '-u', 'mot-router', '-u', 'motgate', '-u', 'guard',
                              '-u', 'mot-nodebank-runtime', '-n', '30', '--since=-2 minutes',
                              '--no-pager', '-o', 'json'], stdout=subprocess.PIPE, stderr=subprocess.DEVNULL)
        raw = p.stdout.read(32769)
        if p.poll() is None:
            p.terminate()
        p.wait(timeout=5)
        counts = collections.Counter()
        for line in raw[:32768].splitlines():
            try:
                message = str(json.loads(line).get('MESSAGE', ''))
                if any(s in message.lower() for s in ('error', 'failed', '403', '502')):
                    counts[classify(502 if '502' in message else 403 if '403' in message else 500, message)] += 1
            except (ValueError, TypeError):
                continue
        state.set_value('journal_summary', {'time': int(time.time()), 'classification_counts': dict(counts)})
    except Exception:
        state.set_value('journal_summary', {'available': False})


def remediate(state, incidents):
    for incident in incidents:
        kind, component = incident['kind'], incident['component']
        if incident['state'] == 'needs_attention' or incident['hits'] < 2:
            continue
        if kind in ('service_unhealthy', 'agent_unhealthy'):
            if not state.claim_incident(incident['fingerprint']):
                if incident['attempts'] >= 3:
                    state.mark_attention(incident, 'tiga pemulihan sudah dicoba; menunggu bukti baru')
                continue
            try:
                if kind == 'agent_unhealthy':
                    subprocess.run(['/bin/systemctl', 'restart', 'arbyte-agent'], check=True, timeout=30,
                                   stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
                else:
                    call({'action': 'restart', 'component': component})
            except Exception:
                pass
        elif kind == 'rate_limit':
            # No IP rotation, access impersonation, or repeated inference probes.
            state.mark_attention(incident, 'limit upstream; cooldown 15 menit, tidak melakukan rotasi atau spam')
        elif kind in ('access_policy', 'authentication'):
            state.mark_attention(incident, 'periksa izin atau kredensial provider; tidak mengakali pembatasan akses')
        elif kind in ('compatibility', 'transport', 'inference_failure', 'node_sync', 'telemetry_unavailable'):
            if state.claim_incident(incident['fingerprint'], cooldown=3600):
                if kind == 'compatibility' and incident['evidence'].get('model', 'muse-spark-1.3-contributor-free') == 'muse-spark-1.3-contributor-free':
                    try:
                        call({'action': 'verify', 'component': 'mot-router'})
                    except Exception:
                        pass
                # AI worker is deliberately dormant until independent provider/tool evidence exists.
                try:
                    status = call({'action': 'inspect'})
                    if status.get('activation') == 'code_recovery':
                        launch_ai(incident)
                    else:
                        state.mark_attention(incident, 'perbaikan kode menunggu provider AI independen terverifikasi')
                except Exception:
                    state.mark_attention(incident, 'broker tidak tersedia')


def launch_ai(incident):
    # The prompt contains classification and local IDs, never raw upstream/source text.
    unit = 'arbyte-repair-ai.service'
    from recovery_broker import run
    run(['/bin/systemctl', 'start', '--no-block', unit])


def continue_ai(state):
    """Resume after an asynchronous test ends instead of relying on a long chat turn."""
    try:
        status = call({'action': 'inspect'})
        if status.get('activation') != 'code_recovery':
            return
        jobs = status.get('jobs', [])
        if any(j['state'] in ('running', 'queued') for j in jobs):
            return
        candidates = [j for j in jobs if j['action'] == 'test' and j['state'] in ('succeeded', 'failed', 'deferred')
                      and j['created'] > time.time() - 3600]
        if not candidates:
            return
        candidate = max(candidates, key=lambda j: j['updated'])
        if any(j['action'] == 'deploy' and j['data'].get('job_id') == candidate['id'] for j in jobs):
            return
        marker = candidate['id'] + ':' + candidate['state']
        if state.value('ai_continuation') == marker:
            return
        launch_ai({})
        state.set_value('ai_continuation', marker)
    except Exception:
        return


def notifications(state):
    """Use existing paired Telegram owner, keeping bot token out of logs/context."""
    try:
        c = json.loads(Path('/var/lib/arbyte-agent/.nullclaw/config.json').read_text())
        accounts = c.get('channels', {}).get('telegram', {}).get('accounts', {})
        account = next(iter(accounts.values()))
        token = account['bot_token']
        if not isinstance(token, str):
            return
        if token.startswith('enc2:'):
            from cryptography.hazmat.primitives.ciphers.aead import ChaCha20Poly1305
            key = bytes.fromhex(Path('/var/lib/arbyte-agent/.nullclaw/.secret_key').read_text().strip())
            encrypted = bytes.fromhex(token[5:])
            token = ChaCha20Poly1305(key).decrypt(encrypted[:12], encrypted[12:], b'').decode()
        elif token.startswith('enc'):
            return
        owner = next(str(v) for v in account.get('allow_from', []) if str(v).isdigit())
        with state.connect() as db:
            pending = db.execute('SELECT id,text FROM notifications WHERE delivered=0 ORDER BY id LIMIT 4').fetchall()
        for item in pending:
            code, raw = request('https://api.telegram.org/bot' + token + '/sendMessage',
                                {'chat_id': owner, 'text': item['text']}, timeout=10)
            if code != 200 or not json.loads(raw).get('ok'):
                break
            with state.connect() as db:
                db.execute('UPDATE notifications SET delivered=1 WHERE id=?', (item['id'],))
    except Exception:
        return  # The durable outbox retries next minute; never print HTTP URLs containing tokens.


def cleanup(state):
    state.prune()
    cutoff = time.time() - 14 * 86400
    for parent in (ROOT / 'builds', ROOT / 'releases', BACKUPS, WORKSPACES):
        if not parent.exists():
            continue
        for p in parent.iterdir():
            try:
                if not p.is_symlink() and p.is_dir() and p.stat().st_mtime < cutoff:
                    row = state.job(p.name)
                    if row['state'] in ('running', 'queued'):
                        continue
                    import shutil
                    shutil.rmtree(p)
            except ValueError:
                # Jobs already pruned: directory ID still must be a canonical UUID.
                from recovery_core import valid_id
                try:
                    valid_id(p.name)
                    import shutil
                    shutil.rmtree(p)
                except ValueError:
                    pass


def main():
    import os
    os.umask(0o077)
    state = State(ROOT / 'state.db')
    with open(ROOT / 'monitor.lock', 'a') as lock:
        try:
            fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError:
            return
        incidents = state.observe(collect(state))
        remediate(state, incidents)
        continue_ai(state)
        notifications(state)
        cleanup(state)


if __name__ == '__main__':
    main()
