#!/usr/bin/python3
"""Install the approved recovery package as root without replacing Telegram/provider settings."""
import argparse
import datetime
import json
import os
from pathlib import Path
import pwd
import shutil
import subprocess
import sys
import tarfile

from recovery_core import State, digest_file, manifest, manifest_hash
from recovery_broker import COMPONENTS


def extract(archive, dest):
    dest.mkdir(parents=True, exist_ok=True)
    with tarfile.open(archive, 'r:gz') as tar:
        for member in tar.getmembers():
            path = Path(member.name)
            if path.is_absolute() or '..' in path.parts or not (member.isdir() or member.isfile()):
                raise ValueError('Unsafe recovery package member')
        tar.extractall(dest)


def main():
    os.umask(0o077)
    p = argparse.ArgumentParser()
    p.add_argument('package', type=Path)
    p.add_argument('--operator-only', action='store_true', help='Update broker/operator while retaining verified toolchains and warm caches')
    args = p.parse_args()
    if os.geteuid() != 0:
        raise SystemExit('Root installation required')
    package = args.package.resolve()
    receipt = json.loads((package / 'receipt.json').read_text())
    for name, sha in receipt['archives'].items():
        if digest_file(package / name) != sha:
            raise ValueError('Package checksum mismatch')
    if digest_file('/opt/mot-router/mot-router') != receipt['production_router_sha256']:
        raise ValueError('Production router changed since source verification')
    stamp = datetime.datetime.now(datetime.timezone.utc).strftime('%Y%m%dT%H%M%SZ')
    backup = Path('/var/backups/arbyte-agent') / (stamp + '-recovery-install')
    backup.mkdir(parents=True, mode=0o700)
    shutil.copytree('/opt/arbyte-agent', backup / 'operator')
    shutil.copytree('/var/lib/arbyte-agent/.nullclaw', backup / 'nullclaw')
    for name in ('arbytectl', 'arbyte-agent'):
        shutil.copy2('/usr/local/bin/' + name, backup / name)
    for unit in ('arbyte-repair-monitor.timer', 'arbyte-repair-monitor.service', 'arbyte-repair-broker.service'):
        subprocess.run(['/bin/systemctl', 'stop', unit], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    root = Path('/var/lib/arbyte-repair')
    root.mkdir(mode=0o711, exist_ok=True)
    for path in (Path('/var/backups/arbyte-repair'), root / 'releases', root / 'builds'):
        path.mkdir(parents=True, mode=0o700, exist_ok=True)
    if (root / 'source').exists():
        shutil.copytree(root / 'source', backup / 'source')
        shutil.rmtree(root / 'source')
    extract(package / 'source.tar.gz', root / 'source')
    source = manifest(root / 'source')
    if manifest_hash(source) != receipt['manifest_sha256']:
        raise ValueError('Source manifest differs from approved laptop snapshot')
    try:
        builder = pwd.getpwnam('arbyte-build')
    except KeyError:
        subprocess.run(['/usr/sbin/useradd', '--system', '--home-dir', '/var/cache/arbyte-build',
                        '--shell', '/usr/sbin/nologin', 'arbyte-build'], check=True)
        builder = pwd.getpwnam('arbyte-build')
    cache = Path('/var/cache/arbyte-build')
    cache.mkdir(parents=True, mode=0o700, exist_ok=True)
    toolchains = Path('/opt/arbyte-toolchains')
    if not args.operator_only:
        extract(package / 'dependencies.tar.gz', cache)
        for path in [cache, *cache.rglob('*')]:
            os.chown(path, builder.pw_uid, builder.pw_gid)
        extract(package / 'toolchains.tar.gz', toolchains)
        if (package / 'rust-runtime.tar.gz').exists():
            extract(package / 'rust-runtime.tar.gz', toolchains / 'runtime')
        for path in [toolchains, *toolchains.rglob('*')]:
            if path.is_dir():
                path.chmod(0o755)
            elif not path.is_symlink():
                path.chmod(path.stat().st_mode | 0o444)
        # Runtime wrappers allow the laptop's pinned Rust toolchain to run on Ubuntu 20.04.
        rust = toolchains / 'rust/bin'
        for name in ('cargo', 'rustc', 'rustdoc'):
            executable = rust / name
            if executable.exists():
                executable.rename(rust / (name + '-real'))
                executable.write_text('#!/bin/sh\nexec /opt/arbyte-toolchains/runtime/ld-linux-x86-64.so.2 '
                    '--library-path /opt/arbyte-toolchains/runtime:/opt/arbyte-toolchains/rust/lib '
                    '/opt/arbyte-toolchains/rust/bin/' + name + '-real "$@"\n')
                executable.chmod(0o755)
    else:
        subprocess.run([str(toolchains / 'go/bin/go'), 'version'], check=True)
        subprocess.run([str(toolchains / 'rust/bin/rustc'), '--version'], check=True)
    # Source archive also contains root-owned broker implementation and acceptance tests.
    operator = root / 'source/tools/arbyte-agent'
    for path in operator.glob('*.py'):
        shutil.copy2(path, Path('/opt/arbyte-agent') / path.name)
        (Path('/opt/arbyte-agent') / path.name).chmod(0o644)
    for directory in ('systemd', 'workspace'):
        for path in (operator / directory).iterdir():
            dest = Path('/opt/arbyte-agent') / directory / path.name
            dest.parent.mkdir(exist_ok=True)
            shutil.copy2(path, dest)
    for name, src in [('arbytectl', 'arbytectl.py'), ('arbyte-agent', 'agent_client.py')]:
        dest = Path('/usr/local/bin') / name
        shutil.copy2(operator / src, dest)
        dest.chmod(0o755)
    agent = pwd.getpwnam('arbyte-agent')
    workspaces = Path('/var/lib/arbyte-agent/repair-workspaces')
    workspaces.mkdir(mode=0o700, exist_ok=True)
    os.chown(workspaces, agent.pw_uid, agent.pw_gid)
    config = Path('/var/lib/arbyte-agent/.nullclaw/config.json')
    c = json.loads(config.read_text())
    allowed = c['autonomy'].setdefault('allowed_paths', [])
    if str(workspaces) not in allowed:
        allowed.append(str(workspaces))
    c['autonomy']['block_medium_risk_commands'] = False
    c['autonomy']['require_approval_for_medium_risk'] = False
    for tool in c['tools']['tool_customizations']:
        if tool['name'] in ('file_edit', 'file_append', 'file_read_hashed', 'file_edit_hashed'):
            tool['enabled'] = True
    # Elevated actions are constrained by the broker, not a generic sudo tool.
    config.write_text(json.dumps(c, indent=2) + '\n')
    config.chmod(0o600)
    os.chown(config, agent.pw_uid, agent.pw_gid)
    for path in (operator / 'workspace').glob('*.md'):
        dest = Path('/var/lib/arbyte-agent/.nullclaw/workspace') / path.name
        shutil.copy2(path, dest); os.chown(dest, agent.pw_uid, agent.pw_gid); dest.chmod(0o600)
    policy = Path('/etc/arbyte-repair.json')
    if policy.exists():
        shutil.copy2(policy, backup / policy.name)
    # Never accidentally enable AI code repair during an upgrade.
    policy.write_text(json.dumps({'independent_verified': False, 'code_repair_enabled': False,
                                 'toolchain': receipt['toolchain'], 'installed_at': stamp}, indent=2) + '\n')
    policy.chmod(0o600)
    state = State(root / 'state.db')
    state.set_value('source_receipt', {'verified': True, 'manifest_sha256': receipt['manifest_sha256'],
                    'production_router_sha256': receipt['production_router_sha256'],
                    'components': {name: digest_file(spec.get('binary', spec.get('config'))) for name, spec in COMPONENTS.items()},
                    'verification': 'router reproduced exactly on laptop; other components recorded with migration acceptance gate'})
    for path in (operator / 'systemd').glob('arbyte-repair-*'):
        shutil.copy2(path, Path('/etc/systemd/system') / path.name)
    subprocess.run(['/usr/sbin/runuser', '-u', 'arbyte-agent', '--', 'env', 'HOME=/var/lib/arbyte-agent',
                    'NULLCLAW_HOME=/var/lib/arbyte-agent/.nullclaw', '/usr/local/bin/nullclaw', 'config', 'validate'], check=True)
    subprocess.run(['/bin/systemctl', 'daemon-reload'], check=True)
    subprocess.run(['/bin/systemctl', 'enable', '--now', 'arbyte-repair-broker.service'], check=True)
    subprocess.run(['/bin/systemctl', 'restart', 'arbyte-agent'], check=True)
    subprocess.run(['/bin/systemctl', 'enable', '--now', 'arbyte-repair-monitor.timer'], check=True)
    print(json.dumps({'installed': True, 'backup': str(backup), 'activation': 'tested_procedures',
                      'source_sha256': receipt['manifest_sha256']}))


if __name__ == '__main__':
    main()
