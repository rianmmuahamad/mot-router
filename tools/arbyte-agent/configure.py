#!/usr/bin/python3
"""Run as root on the VPS after copying this directory to /opt/arbyte-agent."""
import datetime
import json
import os
from pathlib import Path
import pwd
import hashlib
import secrets
import shutil
import sqlite3
import subprocess
import urllib.request
from cryptography.hazmat.primitives.ciphers.aead import ChaCha20Poly1305

os.umask(0o077)
root = Path('/opt/arbyte-agent')
home = Path('/var/lib/arbyte-agent')
account = pwd.getpwnam('arbyte-agent')
stamp = datetime.datetime.now(datetime.timezone.utc).strftime('%Y%m%dT%H%M%SZ')
backup = Path('/var/backups/arbyte-agent') / stamp
backup.mkdir(parents=True, mode=0o700)
with sqlite3.connect('file:/opt/mot-router/data/mot-router.db?mode=ro', uri=True) as db:
    with sqlite3.connect(backup / 'mot-router.db') as dest:
        db.backup(dest)
if (home / '.nullclaw/config.json').exists():
    shutil.copytree(home / '.nullclaw', backup / 'nullclaw')

http = urllib.request.build_opener(urllib.request.ProxyHandler({}))
def request(path, data=None):
    req = urllib.request.Request('http://127.0.0.1:8787' + path,
        data=None if data is None else json.dumps(data).encode(),
        headers={'Content-Type': 'application/json'})
    with http.open(req, timeout=10) as r:
        return json.load(r)

keys = request('/api/keys')
key = next((k for k in keys if k.get('name') == 'Arbyte NullClaw Ops' and not k.get('revoked')), None)
if key is None:
    key = request('/api/keys', {'name': 'Arbyte NullClaw Ops'})
config = json.loads((root / 'config.template.json').read_text())
secret_path = home / '.nullclaw/.secret_key'
if secret_path.exists():
    secret_key = bytes.fromhex(secret_path.read_text().strip())
else:
    secret_key = os.urandom(32)
    secret_path.write_text(secret_key.hex())
    os.chmod(secret_path, 0o600)
    os.chown(secret_path, account.pw_uid, account.pw_gid)
nonce = os.urandom(12)
encrypted = ChaCha20Poly1305(secret_key).encrypt(nonce, key['key'].encode(), b'')
config['models']['providers']['arbyte']['api_key'] = 'enc2:' + (nonce + encrypted).hex()
operator_path = home / '.nullclaw/operator-token'
if not operator_path.exists():
    operator_path.write_text(secrets.token_urlsafe(32))
    os.chmod(operator_path, 0o600)
    os.chown(operator_path, account.pw_uid, account.pw_gid)
config['gateway']['paired_tokens'] = [hashlib.sha256(operator_path.read_text().strip().encode()).hexdigest()]
config['gateway']['webhook_sync_for_workers'] = True
config['session'] = {'max_concurrent_tasks': 1}
config_path = home / '.nullclaw/config.json'
config_path.write_text(json.dumps(config, indent=2) + '\n')
os.chmod(config_path, 0o600)
os.chown(config_path, account.pw_uid, account.pw_gid)

for p in (home / 'ops', home / '.nullclaw/workspace'):
    p.mkdir(parents=True, exist_ok=True, mode=0o700)
    os.chown(p, account.pw_uid, account.pw_gid)
for src in (root / 'workspace').glob('*.md'):
    dest = home / '.nullclaw/workspace' / src.name
    if not dest.exists():
        shutil.copyfile(src, dest)
        os.chmod(dest, 0o600)
        os.chown(dest, account.pw_uid, account.pw_gid)
policy = home / 'ops/policy.json'
if not policy.exists():
    policy.write_text(json.dumps({'auto_scan': True}) + '\n')
    os.chown(policy, account.pw_uid, account.pw_gid)

subprocess.run(['install', '-m', '0755', str(root / 'arbytectl.py'), '/usr/local/bin/arbytectl'], check=True)
subprocess.run(['install', '-m', '0755', str(root / 'agent_client.py'), '/usr/local/bin/arbyte-agent'], check=True)
for unit in (root / 'systemd').glob('*'):
    shutil.copyfile(unit, Path('/etc/systemd/system') / unit.name)

env = ['sudo', '-u', 'arbyte-agent', 'env', 'HOME=' + str(home),
       'NULLCLAW_HOME=' + str(home / '.nullclaw'),
       'NULLCLAW_WORKSPACE=' + str(home / '.nullclaw/workspace'), 'nullclaw']
subprocess.run(env + ['config', 'validate'], check=True)
saved = json.loads(config_path.read_text())
assert saved['models']['providers']['arbyte']['api_key'].startswith('enc')
print('Configured encrypted provider key ID', key['id'])
print('Pre-install backup:', backup)
