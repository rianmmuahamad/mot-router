#!/usr/bin/python3
"""Private Unix-socket recovery broker. Never executes agent-supplied shell commands."""
import argparse
import concurrent.futures
import contextlib
import fcntl
import json
import http.client
import os
from pathlib import Path
import pwd
import re
import secrets
import shutil
import socket
import socketserver
import sqlite3
import stat
import struct
import ssl
import subprocess
import threading
import time
import urllib.error
import urllib.request

from recovery_core import State, digest_file, manifest, manifest_hash, valid_id

ROOT = Path('/var/lib/arbyte-repair')
BACKUPS = Path('/var/backups/arbyte-repair')
WORKSPACES = Path('/var/lib/arbyte-agent/repair-workspaces')
SOCKET = '/run/arbyte-repair/broker.sock'
POLICY = Path('/etc/arbyte-repair.json')
MAX_MESSAGE = 32768
HTTP = urllib.request.build_opener(urllib.request.ProxyHandler({}))
COMPONENTS = {
    'mot-router': {'binary': '/opt/mot-router/mot-router', 'source': '.', 'kind': 'go',
                   'health': 'http://127.0.0.1:8787/api/stats', 'db': '/opt/mot-router/data/mot-router.db'},
    'motgate': {'binary': '/opt/mot-router/tools/motgate/motgate', 'source': 'tools/motgate', 'kind': 'go',
                'health': 'http://127.0.0.1:8790/healthz', 'db': '/var/lib/motgate/motgate.db'},
    'guard': {'binary': '/opt/mot-router/tools/guard/guard', 'source': 'tools/guard', 'kind': 'cpp',
              'health': 'http://127.0.0.1:8791/healthz'},
    'mot-nodebank': {'binary': '/opt/mot-router/nodebank/mot-nodebank', 'source': 'tools/nodebank', 'kind': 'rust',
                     'health': 'http://127.0.0.1:8792/api/stats', 'db': '/opt/mot-router/data/nodebank/bank.db'},
    'mot-nodebank-runtime': {'binary': '/opt/mot-router/nodebank/nodebank-runtime',
                             'source': 'tools/nodebank/probe', 'kind': 'go-probe',
                             'health': 'http://127.0.0.1:9093/status'},
    'nodebank-probe-runner': {'binary': '/opt/mot-router/nodebank/nodebank-probe-runner',
                              'source': 'tools/nodebank/probe', 'kind': 'go-probe',
                              'service': 'mot-nodebank', 'health': 'http://127.0.0.1:8792/api/stats'},
    'caddy': {'config': '/etc/caddy/Caddyfile', 'kind': 'config', 'health': 'http://127.0.0.1:2019/config/'},
}


def request(url, body=None, headers=None, timeout=8, limit=131072):
    req = urllib.request.Request(url, data=None if body is None else json.dumps(body).encode(),
                                 headers={'Content-Type': 'application/json', **(headers or {})})
    try:
        with HTTP.open(req, timeout=timeout) as r:
            raw = r.read(limit + 1)
            if len(raw) > limit:
                raise ValueError('Response exceeds bounded limit')
            return r.status, raw
    except urllib.error.HTTPError as e:
        return e.code, e.read(limit)


def run(argv, timeout=15):
    # Do not return raw journal/build output to the model; it may contain secrets.
    p = subprocess.run(argv, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=timeout)
    if p.returncode:
        raise ValueError('Trusted operation failed: ' + Path(argv[0]).name)


def service(component):
    return COMPONENTS[component].get('service', component)


def healthy(component):
    try:
        if subprocess.run(['/bin/systemctl', 'is-active', '--quiet', service(component)], timeout=5).returncode:
            return False
        code, raw = request(COMPONENTS[component]['health'])
        data = json.loads(raw)
        basic = code == 200 and isinstance(data, dict) and data.get('ok', True) is not False
        return basic and (site_health()['ok'] if component == 'caddy' else True)
    except Exception:
        return False


def site_health():
    """Check origin TLS/SNI with certificate verification, not the Cloudflare edge cache."""
    ca = '/var/lib/caddy/.local/share/caddy/pki/authorities/local/root.crt'
    sites = [('arbyte.coddx.store', '/', 200), ('portal.coddx.store', '/admin', 401),
             ('mot.coddx.store', '/', 401), ('proxy.coddx.store', '/', 401)]
    checks = {}
    for domain, path, expected in sites:
        try:
            context = ssl.create_default_context()
            context.load_verify_locations(cafile=ca)
            with socket.create_connection(('127.0.0.1', 443), timeout=5) as connection:
                with context.wrap_socket(connection, server_hostname=domain) as tls:
                    tls.sendall(('GET %s HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n\r\n' % (path, domain)).encode())
                    response = http.client.HTTPResponse(tls)
                    response.begin()
                    response.read(16384)
                    checks[domain] = {'ok': response.status == expected, 'status': response.status, 'tls_verified': True}
        except Exception:
            checks[domain] = {'ok': False, 'tls_verified': False}
    return {'ok': all(c['ok'] for c in checks.values()), 'sites': checks}


def resources():
    mem = {line.split(':')[0]: int(line.split()[1]) * 1024 for line in Path('/proc/meminfo').read_text().splitlines()}
    return {'available_bytes': mem['MemAvailable'], 'disk_free_bytes': shutil.disk_usage(ROOT).free}


def database_backup(src, dest):
    # The source stays live; backup() includes WAL and preserves transactions.
    with sqlite3.connect('file:' + str(src) + '?mode=ro', uri=True, timeout=20) as db:
        with sqlite3.connect(dest, timeout=20) as target:
            db.backup(target, pages=128, sleep=0.01)
            if target.execute('PRAGMA quick_check').fetchone()[0] != 'ok':
                raise ValueError('Database backup integrity failed')
    Path(dest).chmod(0o600)


def atomic_copy(src, dest):
    dest = Path(dest)
    original = dest.stat()
    tmp = dest.with_name(dest.name + '.arbyte-partial')
    if tmp.is_symlink():
        raise ValueError('Unsafe installation path')
    shutil.copyfile(src, tmp)
    os.chmod(tmp, stat.S_IMODE(original.st_mode))
    os.chown(tmp, original.st_uid, original.st_gid)
    with open(tmp, 'rb') as f:
        os.fsync(f.fileno())
    os.replace(tmp, dest)
    fd = os.open(dest.parent, os.O_DIRECTORY)
    try:
        os.fsync(fd)
    finally:
        os.close(fd)


def snapshot(src, dest):
    before = manifest(src)
    shutil.copytree(src, dest)
    after = manifest(dest)
    # The agent may still be editing: refuse a torn snapshot.
    if before != after or before != manifest(src):
        shutil.rmtree(dest)
        raise ValueError('Source changed while snapshotting')
    return manifest_hash(after)


def static_go_artifact(path):
    with open(path, 'rb') as f:
        header = f.read(64)
        if len(header) < 64 or header[:6] != b'\x7fELF\x02\x01':
            return False
        offset = struct.unpack_from('<Q', header, 32)[0]
        size, count = struct.unpack_from('<HH', header, 54)
        if count > 128 or size < 4:
            return False
        for i in range(count):
            f.seek(offset + i * size)
            block = f.read(4)
            if len(block) != 4 or struct.unpack('<I', block)[0] == 3:  # PT_INTERP
                return False
    return True


class Broker:
    def __init__(self):
        ROOT.mkdir(parents=True, mode=0o700, exist_ok=True)
        BACKUPS.mkdir(parents=True, mode=0o700, exist_ok=True)
        self.state = State(ROOT / 'state.db')
        self.pool = concurrent.futures.ThreadPoolExecutor(max_workers=1)
        self.submit_lock = threading.Lock()

    def policy(self):
        return json.loads(POLICY.read_text())

    def check_ai(self):
        p = self.policy()
        if not p.get('independent_verified') or not p.get('code_repair_enabled'):
            raise ValueError('Code recovery locked: independent provider tool verification is pending')
        config = Path('/var/lib/arbyte-agent/.nullclaw/config.json')
        if not p.get('provider_config_sha256') or digest_file(config) != p['provider_config_sha256']:
            raise ValueError('Independent provider configuration changed; verification must be repeated')
        c = json.loads(config.read_text())
        model = c.get('agents', {}).get('defaults', {}).get('model', {}).get('primary', '')
        provider = model.split('/', 1)[0]
        if provider == 'arbyte' or provider != p.get('independent_provider'):
            raise ValueError('Active model is not the verified independent provider')

    def inspect(self):
        p = self.policy()
        try:
            self.check_ai(); ready = True
        except ValueError:
            ready = False
        return {'activation': 'code_recovery' if ready else 'tested_procedures',
                'independent_verified': bool(p.get('independent_verified')),
                'resources': resources(), 'components': list(COMPONENTS),
                'monitor': self.state.value('monitor', {}),
                'journal_summary': self.state.value('journal_summary', {}),
                'source_receipt': self.state.value('source_receipt', {}),
                'incidents': self.state.incidents(), 'jobs': self.state.jobs()}

    def dispatch(self, data, uid):
        if not isinstance(data, dict) or set(data) - {'action', 'component', 'job_id', 'reason', 'changes', 'account_id', 'operation', 'validation'}:
            raise ValueError('Invalid broker request fields')
        if data.get('validation') and uid != 0:
            raise ValueError('Only root acceptance fixtures may use validation mode')
        action = data.get('action')
        if action == 'inspect':
            return self.inspect()
        if action == 'status':
            return self.state.job(valid_id(data['job_id'])) if data.get('job_id') else self.inspect()
        if action not in ('prepare', 'test', 'deploy', 'rollback', 'restart', 'verify', 'config', 'account', 'credential'):
            raise ValueError('Unknown recovery action')
        component = data.get('component', 'mot-router')
        if component not in COMPONENTS:
            raise ValueError('Component is outside Arbyte')
        if action in ('test', 'deploy', 'config', 'account', 'credential') and uid != 0:
            self.check_ai()
        if action in ('prepare', 'test', 'deploy', 'config', 'account', 'credential') and not str(data.get('reason', '')).strip():
            raise ValueError('Evidence-based reason is required')
        if not isinstance(data.get('reason', ''), str) or len(data.get('reason', '')) > 500:
            raise ValueError('Reason exceeds limit')
        if action in ('test', 'deploy', 'rollback'):
            valid_id(data.get('job_id'))
        # Never save arbitrary fields or sensitive values in the job database.
        if action in ('config', 'account'):
            self.validate_sensitive(data)
        if action == 'credential' and component not in ('motgate', 'caddy'):
            raise ValueError('Credential recovery supports portal admin or Mot dashboard only')
        with self.submit_lock:
            job = self.state.new_job(action, component, data, patch=action == 'test' and not data.get('validation'))
            self.pool.submit(self.execute, job)
        return {'job_id': job, 'state': 'queued'}

    def recover_interrupted(self):
        # Called only after acquiring the broker's process lock.
        with self.state.connect() as db:
            ids = [r[0] for r in db.execute("SELECT id FROM jobs WHERE state IN ('running','queued')")]
        for job in ids:
            row = self.state.job(job)
            if row['action'] == 'test':
                for stage in ('tests', 'build', 'migration', 'linkage'):
                    subprocess.run(['/bin/systemctl', 'stop', 'arbyte-build-' + job + '-' + stage],
                                   stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=15)
            result = row['result']
            if result.get('backup') and row['action'] in ('deploy', 'config', 'credential'):
                try:
                    self.restore(row['component'], Path(result['backup']), config_only=row['action'] == 'config' or (row['action'] == 'credential' and row['component'] == 'motgate'))
                    self.state.update_job(job, 'rolled_back', {'reason': 'Broker restarted during deployment', 'backup': result['backup']})
                except Exception:
                    self.state.update_job(job, 'interrupted', {'reason': 'Interrupted deployment needs attention', 'backup': result['backup']})
            else:
                self.state.update_job(job, 'interrupted', {'reason': 'Broker restarted; candidate was not deployed'})
            self.state.notify('interrupted:' + job, 'Arbyte: recovery terputus; periksa job ' + job + '.')

    def execute(self, job):
        row = self.state.job(job)
        self.state.update_job(job, 'running', {'stage': row['action']})
        try:
            action = getattr(self, 'do_' + row['action'])
            result = action(row)
            state = result.pop('_state', 'succeeded')
            self.state.update_job(job, state, result)
            if row['action'] in ('deploy', 'rollback', 'restart', 'config', 'account', 'credential') or state != 'succeeded':
                self.state.notify('job:' + job, 'Arbyte: %s %s — %s; job %s.' % (row['action'], row['component'], state, job))
        except Exception as e:
            # Only locally generated safe error strings are released.
            reason = str(e) if isinstance(e, ValueError) else type(e).__name__ + '; lihat log recovery privat'
            self.state.update_job(job, 'failed', {'reason': reason[:500]})
            self.state.notify('job:' + job, 'Arbyte: %s %s gagal; job %s.' % (row['action'], row['component'], job))

    def do_prepare(self, row):
        receipt = self.state.value('source_receipt', {})
        if not receipt.get('verified'):
            raise ValueError('Matching production source receipt is required')
        dest = WORKSPACES / row['id']
        WORKSPACES.mkdir(mode=0o700, parents=True, exist_ok=True)
        snapshot(ROOT / 'source', dest)
        if row['component'] == 'caddy':
            config = Path('/etc/caddy/Caddyfile').read_text()
            config = re.sub(r'\$2[aby]\$[^\s]+', '__KEEP_AUTH_HASH__', config)
            (dest / 'Caddyfile').write_text(config)
        user = pwd.getpwnam('arbyte-agent')
        for path in [dest, *dest.rglob('*')]:
            os.chown(path, user.pw_uid, user.pw_gid)
            path.chmod(0o700 if path.is_dir() else 0o600)
        return {'workspace': str(dest), 'baseline': receipt['manifest_sha256'], 'stage': 'ready_for_patch'}

    def build_run(self, row, cwd, argv, label, timeout=900):
        argv = list(argv)
        if argv[0] == 'go':
            argv[0] = '/opt/arbyte-toolchains/go/bin/go'
        elif argv[0] == 'cargo':
            argv[0] = '/opt/arbyte-toolchains/rust/bin/cargo'
        unit = 'arbyte-build-' + row['id'] + '-' + label
        log = ROOT / 'builds' / row['id'] / (label + '.log')
        # Output is a private root-owned file, not a world-readable journal.
        cmd = ['/bin/systemd-run', '--quiet', '--wait', '--pipe', '--collect', '--unit=' + unit,
               '--property=User=arbyte-build', '--property=Group=arbyte-build',
               '--property=MemoryMax=512M', '--property=MemoryHigh=448M', '--property=MemorySwapMax=0',
               '--property=CPUQuota=50%', '--property=TasksMax=64', '--property=Nice=15',
               '--property=LimitFSIZE=256M',
               '--property=ProtectSystem=strict', '--property=ProtectHome=true',
               '--property=PrivateTmp=true', '--property=PrivateDevices=true', '--property=NoNewPrivileges=true',
               '--property=ProtectKernelTunables=true', '--property=ProtectKernelModules=true',
               '--property=ProtectControlGroups=true', '--property=RestrictSUIDSGID=true',
               '--property=PrivateNetwork=true',
               '--property=ReadWritePaths=' + str(ROOT / 'builds' / row['id']) + ' /var/cache/arbyte-build',
               '--property=InaccessiblePaths=/var/lib/arbyte-agent /var/lib/motgate /opt/mot-router/data /etc/mot-router /etc/caddy /root',
               '--property=RuntimeMaxSec=' + str(timeout), '--working-directory=' + str(cwd),
               '--setenv=HOME=/var/cache/arbyte-build', '--setenv=PATH=/opt/arbyte-toolchains/go/bin:/opt/arbyte-toolchains/rust/bin:/usr/bin:/bin',
               '--setenv=CGO_ENABLED=0', '--setenv=GOMAXPROCS=1', '--setenv=GOMEMLIMIT=384MiB',
               '--setenv=GOTOOLCHAIN=local', '--setenv=GOFLAGS=-mod=readonly',
               '--setenv=GOPROXY=off',
               '--setenv=GOCACHE=/var/cache/arbyte-build/go', '--setenv=GOMODCACHE=/var/cache/arbyte-build/modules',
               '--setenv=CARGO_HOME=/var/cache/arbyte-build/cargo', '--setenv=CARGO_BUILD_JOBS=1',
               '--setenv=CARGO_INCREMENTAL=0', '--', *argv]
        with open(log, 'wb') as out:
            try:
                p = subprocess.run(cmd, stdout=out, stderr=out, timeout=timeout + 30)
            except subprocess.TimeoutExpired:
                subprocess.run(['/bin/systemctl', 'stop', unit], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=15)
                raise ValueError('Build deadline exceeded') from None
        if p.returncode:
            raise ValueError('Build/test failed or exceeded resource limit; private log: ' + label)

    def do_test(self, row):
        r = resources()
        if r['available_bytes'] < 900 * 1024**2 or r['disk_free_bytes'] < 2 * 1024**3:
            return {'_state': 'deferred', 'reason': 'Insufficient RAM or disk for bounded build'}
        original = self.state.job(row['data']['job_id'])
        if original['action'] != 'prepare' or original['state'] != 'succeeded' or original['component'] != row['component']:
            raise ValueError('Successful matching prepare job required')
        if original['result']['baseline'] != self.state.value('source_receipt', {})['manifest_sha256']:
            raise ValueError('Prepared source is stale; prepare again from deployed baseline')
        src = WORKSPACES / original['id']
        base = ROOT / 'builds' / row['id']
        base.mkdir(mode=0o700, parents=True)
        dest = base / 'source'
        source_hash = snapshot(src, dest)
        golden = manifest(ROOT / 'source')
        current = manifest(dest)
        if row['data'].get('validation') and current != golden:
            raise ValueError('Validation fixture must use unchanged source')
        allowed = COMPONENTS[row['component']].get('source', '.')
        for name in set(golden) | set(current):
            if golden.get(name) == current.get(name):
                continue
            within = (name.startswith(('internal/', 'web/')) or name == 'main.go') if row['component'] == 'mot-router' else (
                name == 'Caddyfile' if row['component'] == 'caddy' else name.startswith(allowed + '/'))
            if not within:
                raise ValueError('Patch changes files outside selected component: ' + name)
        # Existing acceptance tests, dependency locks, and migrations cannot be silently removed.
        for name, sha in golden.items():
            if name.endswith(('_test.go', 'Cargo.lock', 'Cargo.toml', 'go.mod', 'go.sum')) or '/scripts/' in name:
                if current.get(name) != sha:
                    raise ValueError('Trusted tests or pinned dependencies changed: ' + name)
        spec = COMPONENTS[row['component']]
        cwd = dest / spec.get('source', '.')
        artifact = base / 'candidate'
        builder = pwd.getpwnam('arbyte-build')
        os.chmod(ROOT, 0o711)
        (ROOT / 'builds').chmod(0o711)
        for p in [base, *base.rglob('*')]:
            os.chown(p, builder.pw_uid, builder.pw_gid)
            p.chmod(0o700 if p.is_dir() else 0o600)
        kind = spec['kind']
        if kind in ('go', 'go-probe'):
            tags = ['-tags=with_utls,with_grpc'] if kind == 'go-probe' else []
            self.build_run(row, cwd, ['go', 'test', '-short', '-p', '1', *tags, './...'], 'tests')
            self.build_run(row, cwd, ['go', 'build', '-p', '1', '-buildvcs=false', *tags,
                                      '-ldflags=-s -w', '-o', str(artifact), '.'], 'build')
            # Static Go binaries have no ELF interpreter, unlike the failed CGO release.
            if not static_go_artifact(artifact):
                raise ValueError('Go candidate unexpectedly requires a dynamic loader')
        elif kind == 'rust':
            self.build_run(row, cwd, ['cargo', 'test', '--locked', '--offline', '-j', '1'], 'tests', 1800)
            self.build_run(row, cwd, ['cargo', 'build', '--locked', '--offline', '--release', '-j', '1'], 'build', 1800)
            shutil.copyfile(cwd / 'target/release/mot-nodebank', artifact)
        elif kind == 'cpp':
            # Do not execute a patched Makefile as root or as the builder.
            self.build_run(row, cwd, ['/usr/bin/g++', '-std=c++17', '-O2', '-pthread',
                                      *[str(p) for p in sorted((cwd / 'src').glob('*.cpp'))], '-lcurl', '-o', str(artifact)], 'build')
        elif kind == 'config':
            candidate = src / 'Caddyfile'
            if not candidate.is_file():
                raise ValueError('Place proposed Caddyfile in workspace root')
            config = candidate.read_text()
            hashes = re.findall(r'\$2[aby]\$[^\s]+', Path('/etc/caddy/Caddyfile').read_text())
            if config.count('__KEEP_AUTH_HASH__') != len(hashes):
                raise ValueError('Caddy authentication placeholders must be preserved')
            for value in hashes:
                config = config.replace('__KEEP_AUTH_HASH__', value, 1)
            artifact.write_text(config)
            run(['/usr/bin/caddy', 'validate', '--config', str(artifact), '--adapter', 'caddyfile'])
        else:
            raise ValueError('Unknown build recipe')
        excluded = ('tools/nodebank/target',) if kind == 'rust' else ()
        if manifest(dest, exclude=excluded) != current:
            raise ValueError('Build or tests changed source; immutable source proof failed')
        # Copy out with no links; the compiler process has already exited.
        if not artifact.is_file() or artifact.is_symlink():
            raise ValueError('Candidate artifact missing or unsafe')
        releases = ROOT / 'releases' / row['id']
        releases.mkdir(parents=True, mode=0o700)
        shutil.copyfile(artifact, releases / 'artifact')
        (releases / 'source-manifest.json').write_text(json.dumps(current, sort_keys=True))
        (releases / 'artifact').chmod(0o600)
        # Database-changing candidates must demonstrate compatibility before deployment.
        if spec.get('db') and kind in ('go', 'rust'):
            self.fixture_migration(row, dest, cwd, base)
        return {'source_sha256': source_hash, 'artifact_sha256': digest_file(releases / 'artifact'),
                'baseline_sha256': original['result']['baseline'],
                'stage': 'tested', 'prepare_job': original['id'], 'tests': 'trusted suite passed',
                'migration': 'fixture preserved existing rows' if spec.get('db') and kind in ('go', 'rust') else 'not_applicable'}

    def fixture_migration(self, row, dest, cwd, base):
        spec = COMPONENTS[row['component']]
        fixture = base / 'fixture'
        fixture.mkdir(mode=0o700)
        dbname = Path(spec['db']).name
        database_backup(spec['db'], fixture / dbname)
        before = database_fingerprint(fixture / dbname)
        if spec['kind'] == 'rust':
            builder = pwd.getpwnam('arbyte-build')
            for p in (fixture, fixture / dbname):
                os.chown(p, builder.pw_uid, builder.pw_gid)
            loader = '/opt/arbyte-toolchains/runtime/ld-linux-x86-64.so.2'
            self.build_run(row, cwd, [loader, '--library-path', '/opt/arbyte-toolchains/runtime',
                                      str(base / 'candidate'), '--data', str(fixture), 'stats'], 'migration', 1800)
            verify_database_fingerprint(fixture / dbname, before)
            return
        # Trusted helper invokes only store.Open on the copied database; no HTTP or upstream calls.
        module = 'github.com/rian/mot-router' if row['component'] == 'mot-router' else 'github.com/rian/motgate'
        helper = cwd / 'recovery_fixture'
        helper.mkdir()
        (helper / 'main.go').write_text('package main\nimport ("os"; "' + module + '/internal/store")\nfunc main(){s,e:=store.Open(os.Args[1]);if e!=nil{panic("fixture migration failed")};s.Close()}\n')
        builder = pwd.getpwnam('arbyte-build')
        for p in [fixture, fixture / dbname, helper, helper / 'main.go']:
            os.chown(p, builder.pw_uid, builder.pw_gid)
        self.build_run(row, cwd, ['go', 'run', '-p', '1', './recovery_fixture', str(fixture)], 'migration')
        verify_database_fingerprint(fixture / dbname, before)

    def backup(self, row):
        spec = COMPONENTS[row['component']]
        path = BACKUPS / row['id']
        path.mkdir(mode=0o700)
        target = spec.get('binary', spec.get('config'))
        shutil.copy2(target, path / 'previous')
        if spec.get('db'):
            database_backup(spec['db'], path / Path(spec['db']).name)
        if (ROOT / 'source').exists():
            shutil.copytree(ROOT / 'source', path / 'source')
        for config in ['/etc/caddy/Caddyfile', '/etc/mot-router/motgate.env', '/etc/mot-router/guard.env']:
            if Path(config).exists():
                shutil.copy2(config, path / Path(config).name)
        metadata = {'component': row['component'], 'target_sha256': digest_file(target),
                    'time': int(time.time()), 'source_receipt': self.state.value('source_receipt', {})}
        (path / 'receipt.json').write_text(json.dumps(metadata))
        self.state.update_job(row['id'], 'running', {'stage': 'backed_up', 'backup': str(path)})
        return path

    def restore(self, component, backup, config_only=False):
        spec = COMPONENTS[component]
        target = '/etc/mot-router/' + component + '.env' if config_only else spec.get('binary', spec.get('config'))
        atomic_copy(backup / 'previous', target)
        self.activate(component)
        if not config_only and (backup / 'source').exists():
            restored = ROOT / ('restore-source-' + backup.name)
            snapshot(backup / 'source', restored)
            old = ROOT / ('old-source-' + backup.name)
            os.replace(ROOT / 'source', old)
            os.replace(restored, ROOT / 'source')
            shutil.rmtree(old)
            metadata = json.loads((backup / 'receipt.json').read_text())
            self.state.set_value('source_receipt', metadata['source_receipt'])
        # Do not overwrite live databases: customer usage since the backup must survive.

    def activate(self, component):
        if component in ('mot-nodebank', 'nodebank-probe-runner'):
            self.preserve_bank_pause()
        if component == 'caddy':
            run(['/usr/bin/caddy', 'validate', '--config', '/etc/caddy/Caddyfile'], 20)
            run(['/bin/systemctl', 'reload', 'caddy'], 20)
        else:
            run(['/bin/systemctl', 'restart', service(component)], 30)

    def preserve_bank_pause(self):
        # A dashboard pause is durable in jobs even when the bank HTTP server is unavailable.
        try:
            with sqlite3.connect('file:' + COMPONENTS['mot-nodebank']['db'] + '?mode=ro', uri=True, timeout=5) as db:
                row = db.execute("SELECT state,error FROM jobs WHERE kind='scan' ORDER BY created_at DESC LIMIT 1").fetchone()
            if row and row[0] in ('paused', 'cancelled') and not row[1]:
                # Drop privilege. arbytectl records auto_scan=false before contacting the bank.
                subprocess.run(['/usr/sbin/runuser', '-u', 'arbyte-agent', '--', '/usr/local/bin/arbytectl', 'scan', 'pause'],
                               stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=20)
                policy = json.loads(Path('/var/lib/arbyte-agent/ops/policy.json').read_text())
                if policy.get('auto_scan') is not False:
                    raise ValueError('Manual bank pause could not be preserved; restart deferred')
        except (OSError, sqlite3.Error, subprocess.TimeoutExpired):
            raise ValueError('Unable to preserve bank scan policy before restart') from None

    def wait_health(self, component):
        for _ in range(15):
            if healthy(component):
                return True
            time.sleep(2)
        return False

    def do_deploy(self, row):
        tested = self.state.job(row['data']['job_id'])
        if tested['action'] != 'test' or tested['state'] != 'succeeded' or tested['component'] != row['component']:
            raise ValueError('Successful matching immutable test artifact required')
        receipt = self.state.value('source_receipt', {})
        if receipt and tested['result'].get('baseline_sha256') != receipt.get('manifest_sha256'):
            raise ValueError('Production source changed since candidate tests')
        artifact = ROOT / 'releases' / tested['id'] / 'artifact'
        if digest_file(artifact) != tested['result']['artifact_sha256']:
            raise ValueError('Artifact hash changed after tests')
        backup = self.backup(row)
        spec = COMPONENTS[row['component']]
        expected = receipt.get('components', {}).get(row['component'])
        if expected and digest_file(spec.get('binary', spec.get('config'))) != expected:
            raise ValueError('Production artifact drifted; reconcile source receipt first')
        try:
            atomic_copy(artifact, spec.get('binary', spec.get('config')))
            self.activate(row['component'])
            if not self.wait_health(row['component']):
                raise ValueError('Post-deploy health failed')
            proof = self.live_verify() if row['component'] in ('mot-router', 'motgate', 'guard') else {'ok': True, 'health': True}
            if not proof['ok']:
                raise ValueError('Post-deploy inference proof failed')
            self.promote_source(tested, row['component'])
            self.state.set_value('last_good:' + row['component'], row['id'])
            return {'backup': str(backup), 'artifact_sha256': tested['result']['artifact_sha256'], 'validation': proof}
        except Exception:
            self.restore(row['component'], backup)
            return {'_state': 'rolled_back', 'backup': str(backup), 'healthy_after_rollback': self.wait_health(row['component']),
                    'reason': 'Candidate failed post-deploy validation; database retained'}

    def do_rollback(self, row):
        previous = self.state.job(row['data']['job_id'])
        if previous['action'] not in ('deploy', 'config', 'credential') or previous['component'] != row['component']:
            raise ValueError('Rollback requires matching deployment job')
        backup = BACKUPS / previous['id']
        self.restore(row['component'], backup, config_only=previous['action'] == 'config' or (previous['action'] == 'credential' and previous['component'] == 'motgate'))
        return {'healthy': self.wait_health(row['component']), 'database_retained': True}

    def promote_source(self, tested, component):
        # Freeze the new baseline so future patches start from the deployed source.
        candidate = ROOT / 'builds' / tested['id'] / 'source'
        receipt = self.state.value('source_receipt', {})
        allowed = COMPONENTS[component].get('source', '.')
        golden = ROOT / 'source'
        source = json.loads((ROOT / 'releases' / tested['id'] / 'source-manifest.json').read_text())
        names = set(manifest(golden)) | set(source)
        for name in names:
            within = (name.startswith(('internal/', 'web/')) or name == 'main.go') if component == 'mot-router' else (
                False if component == 'caddy' else name.startswith(allowed + '/'))
            if not within or '/recovery_fixture/' in name:
                continue
            src, target = candidate / name, golden / name
            if src.exists():
                if digest_file(src) != source.get(name):
                    raise ValueError('Tested source changed before baseline promotion')
                target.parent.mkdir(parents=True, exist_ok=True)
                shutil.copyfile(src, target)
                target.chmod(0o600)
            elif target.exists():
                target.unlink()
        receipt['manifest_sha256'] = manifest_hash(manifest(golden))
        receipt.setdefault('components', {})[component] = digest_file(COMPONENTS[component].get('binary', COMPONENTS[component].get('config')))
        self.state.set_value('source_receipt', receipt)

    def do_restart(self, row):
        # Restart only after a failed service plus endpoint health check, not arbitrary 403s.
        if healthy(row['component']):
            return {'already_healthy': True}
        self.activate(row['component'])
        if not self.wait_health(row['component']):
            raise ValueError('Service did not recover after restart')
        return {'healthy': True}

    def do_verify(self, row):
        last = self.state.value('last_live_verification', {})
        if time.time() - last.get('time', 0) < 3600:
            return {'_state': 'deferred', 'reason': 'Live verification cooldown: one per hour', 'previous': last}
        proof = self.live_verify()
        if not proof['ok']:
            proof['_state'] = 'failed'
        return proof

    def live_verify(self):
        from recovery_verify import verify
        self.state.set_value('last_live_verification', {'time': int(time.time()), 'ok': False, 'running': True})
        proof = verify()
        proof['time'] = int(time.time())
        self.state.set_value('last_live_verification', proof)
        return proof

    def validate_sensitive(self, data):
        if data['action'] == 'config':
            if data.get('component') not in ('motgate', 'guard'):
                raise ValueError('Environment configuration supported for motgate/guard only')
            allowed = {'motgate': {'MOTGATE_DEFAULT_HOURS'}, 'guard': {'GUARD_LISTEN'}}
            changes = data.get('changes')
            if not isinstance(changes, dict) or not changes or set(changes) - allowed[data['component']]:
                raise ValueError('Unsupported environment fields')
            if data['component'] != 'motgate' or any(type(v) is not int or v < 1 or v > 8760 for v in changes.values()):
                raise ValueError('Only effective motgate duration settings can be merged; guard flags need a tested code release')
        else:
            if data.get('component') != 'motgate' or type(data.get('account_id')) is not int or data['account_id'] < 1:
                raise ValueError('One existing motgate account ID required')
            if data.get('operation') not in ('revoke', 'restore'):
                raise ValueError('Only revoke/restore recovery operations allowed; usage resolution is not guessed')

    def do_config(self, row):
        target = Path('/etc/mot-router/' + row['component'] + '.env')
        # Environment files can contain secrets: merge known fields without releasing contents.
        backup = BACKUPS / row['id']
        backup.mkdir(mode=0o700)
        shutil.copy2(target, backup / 'previous')
        self.state.update_job(row['id'], 'running', {'stage': 'backed_up', 'backup': str(backup), 'config_only': True})
        lines = target.read_text().splitlines()
        changes = dict(row['data']['changes'])
        for i, line in enumerate(lines):
            key = line.split('=', 1)[0]
            if key in changes:
                lines[i] = key + '=' + str(changes.pop(key))
        lines.extend(k + '=' + str(v) for k, v in changes.items())
        candidate = backup / 'candidate'
        candidate.write_text('\n'.join(lines) + '\n')
        try:
            atomic_copy(candidate, target)
            self.activate(row['component'])
            if not self.wait_health(row['component']):
                raise ValueError('Configuration health failed')
            return {'backup': str(backup), 'changed_fields': list(row['data']['changes']), 'healthy': True, 'config_only': True}
        except Exception:
            atomic_copy(backup / 'previous', target)
            self.activate(row['component'])
            return {'_state': 'rolled_back', 'backup': str(backup), 'config_only': True}

    def do_account(self, row):
        # Auth stays inside broker; no token top-ups/waivers are invented to clear an outage.
        env = Path('/etc/mot-router/motgate.env').read_text().splitlines()
        password = next(x.split('=', 1)[1].strip().strip('"').strip("'") for x in env if x.startswith('MOTGATE_ADMIN_PASSWORD='))
        path = BACKUPS / row['id']
        path.mkdir(mode=0o700)
        database_backup(COMPONENTS['motgate']['db'], path / 'motgate.db')
        import base64
        auth = base64.b64encode(('admin:' + password).encode()).decode()
        url = 'http://127.0.0.1:8790/api/admin/users/%d/%s' % (row['data']['account_id'], row['data']['operation'])
        code, raw = request(url, {}, {'Authorization': 'Basic ' + auth})
        if code != 200:
            raise ValueError('Account recovery API rejected operation')
        return {'account_id': row['data']['account_id'], 'operation': row['data']['operation'], 'backup': str(path)}

    def do_credential(self, row):
        target = Path('/etc/mot-router/motgate.env' if row['component'] == 'motgate' else '/etc/caddy/Caddyfile')
        password = secrets.token_urlsafe(32)
        text = target.read_text()
        if row['component'] == 'motgate':
            text, count = re.subn(r'(?m)^MOTGATE_ADMIN_PASSWORD=.*$', 'MOTGATE_ADMIN_PASSWORD=' + password, text)
        else:
            hashed = subprocess.run(['/usr/bin/caddy', 'hash-password'], input=(password + '\n').encode(),
                                    capture_output=True, check=True, timeout=10).stdout.decode().strip()
            text, count = re.subn(r'(?s)(mot\.coddx\.store\s*\{.*?basic_auth\s*\{\s*rian\s+)(\$2[aby]\$[^\s]+)',
                                  lambda m: m[1] + hashed, text, count=1)
        if count != 1:
            raise ValueError('Expected credential field could not be identified safely')
        backup = BACKUPS / row['id']
        backup.mkdir(mode=0o700)
        shutil.copy2(target, backup / 'previous')
        candidate = backup / 'candidate'
        candidate.write_text(text)
        if row['component'] == 'caddy':
            run(['/usr/bin/caddy', 'validate', '--config', str(candidate), '--adapter', 'caddyfile'], 20)
        self.state.update_job(row['id'], 'running', {'stage': 'backed_up', 'backup': str(backup)})
        try:
            atomic_copy(candidate, target)
            self.activate(row['component'])
            if not self.wait_health(row['component']):
                raise ValueError('Credential recovery health failed')
            private = ROOT / 'credentials'
            private.mkdir(mode=0o700, exist_ok=True)
            path = private / (row['id'] + '.json')
            path.write_text(json.dumps({'username': 'admin' if row['component'] == 'motgate' else 'rian', 'password': password}))
            path.chmod(0o600)
            receipt = self.state.value('source_receipt', {})
            if row['component'] == 'caddy':
                receipt.setdefault('components', {})['caddy'] = digest_file(target)
                self.state.set_value('source_receipt', receipt)
            return {'backup': str(backup), 'healthy': True, 'credential_file': str(path),
                    'note': 'New password saved privately on VPS; not exposed to model or Telegram'}
        except Exception:
            atomic_copy(backup / 'previous', target)
            self.activate(row['component'])
            return {'_state': 'rolled_back', 'backup': str(backup)}


def database_fingerprint(path):
    # Stream old columns in primary-key order without exposing customer values.
    result = {}
    with sqlite3.connect(path) as db:
        db.execute('PRAGMA cache_size=-16384')
        db.execute('PRAGMA temp_store=FILE')
        for (name,) in db.execute("SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name"):
            quoted = '"' + name.replace('"', '""') + '"'
            cols = [r[1] for r in db.execute('PRAGMA table_info(' + quoted + ')')]
            result[name] = {'columns': cols, 'sha256': table_digest(db, quoted, cols)}
    return result


def table_digest(db, table, columns):
    h = __import__('hashlib').sha256()
    cols = ','.join('"' + x.replace('"', '""') + '"' for x in columns)
    for row in db.execute('SELECT ' + cols + ' FROM ' + table + ' ORDER BY ' + cols):
        h.update(json.dumps(row, default=str, separators=(',', ':')).encode())
    return h.hexdigest()


def verify_database_fingerprint(path, before):
    with sqlite3.connect(path) as db:
        if db.execute('PRAGMA quick_check').fetchone()[0] != 'ok':
            raise ValueError('Fixture database integrity failed')
        for table, baseline in before.items():
            quoted = '"' + table.replace('"', '""') + '"'
            try:
                if table_digest(db, quoted, baseline['columns']) != baseline['sha256']:
                    raise ValueError('Migration changed existing customer/accounting data')
            except sqlite3.DatabaseError:
                raise ValueError('Migration removed an existing table/column') from None


class Handler(socketserver.StreamRequestHandler):
    def handle(self):
        self.request.settimeout(10)
        uid = struct.unpack('3i', self.request.getsockopt(socket.SOL_SOCKET, socket.SO_PEERCRED, 12))[1]
        if uid not in (0, pwd.getpwnam('arbyte-agent').pw_uid):
            return
        try:
            raw = self.rfile.readline(MAX_MESSAGE + 1)
            if len(raw) > MAX_MESSAGE or not raw.endswith(b'\n'):
                raise ValueError('Broker message exceeds limit')
            response = self.server.broker.dispatch(json.loads(raw), uid)
        except Exception as e:
            response = {'error': str(e) if isinstance(e, ValueError) else 'Broker request failed'}
        encoded = json.dumps(response).encode()
        if len(encoded) > 131072:
            encoded = b'{"error":"Bounded broker response exceeded"}'
        self.wfile.write(encoded + b'\n')


def main():
    os.umask(0o077)
    lockpath = ROOT / 'broker.lock'
    ROOT.mkdir(parents=True, mode=0o700, exist_ok=True)
    with open(lockpath, 'a') as lock:
        fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        broker = Broker()
        broker.recover_interrupted()
        Path(SOCKET).parent.mkdir(mode=0o750, parents=True, exist_ok=True)
        with contextlib.suppress(FileNotFoundError):
            os.unlink(SOCKET)
        class Server(socketserver.ThreadingMixIn, socketserver.UnixStreamServer):
            daemon_threads = True
        with Server(SOCKET, Handler) as server:
            server.broker = broker
            user = pwd.getpwnam('arbyte-agent')
            os.chown(Path(SOCKET).parent, 0, user.pw_gid)
            os.chown(SOCKET, 0, user.pw_gid)
            os.chmod(SOCKET, 0o660)
            server.serve_forever()


if __name__ == '__main__':
    main()
