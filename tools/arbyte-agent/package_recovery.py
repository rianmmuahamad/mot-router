#!/usr/bin/env python3
"""Create a secret-free source snapshot and pinned offline toolchains/dependencies."""
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import tarfile

from recovery_core import digest_file, manifest_hash


def add_file(tar, src, name):
    info = tar.gettarinfo(str(src), name)
    if info.issym():
        # Toolchain executables may be symlinks; package their file contents, never links.
        data = Path(src).resolve()
        info = tar.gettarinfo(str(data), name)
        src = data
    info.uid = info.gid = 0
    info.uname = info.gname = ''
    if info.isfile():
        with open(src, 'rb') as f:
            tar.addfile(info, f)


def main():
    repo = Path(__file__).resolve().parents[2]
    out = repo / '.cache/recovery-package'
    out.mkdir(parents=True, exist_ok=True)
    files = sorted([*repo.glob('*.go'), repo / 'go.mod', repo / 'go.sum',
                    *[p for root in ['internal', 'web', 'tools/motgate', 'tools/guard', 'tools/nodebank', 'tools/arbyte-agent']
                      for p in (repo / root).rglob('*') if p.is_file() and not p.is_symlink()
                      and not set(p.relative_to(repo).parts).intersection({'target', 'dist', 'data', '__pycache__', '.git', '.cache'})
                      and p.suffix in {'.go', '.mod', '.sum', '.rs', '.toml', '.lock', '.html', '.css', '.js', '.json', '.svg',
                                       '.py', '.md', '.service', '.timer', '.cpp', '.h', '.sh', '.slice',
                                       '.woff', '.woff2', '.ttf', '.png', '.jpg', '.jpeg', '.webp', '.ico'}]])
    # JSON only when it is a shipped asset or diagnostic fixture, never runtime state/credentials.
    files = [p for p in files if p.suffix != '.json' or ('assets' in p.parts or p.name in ('native_descriptions_v1.json', 'config.template.json'))]
    entries = {str(p.relative_to(repo)): digest_file(p) for p in files}
    with tarfile.open(out / 'source.tar.gz', 'w:gz') as tar:
        for p in files: add_file(tar, p, str(p.relative_to(repo)))
    if '--source-only' in sys.argv:
        receipt = json.loads((out / 'receipt.json').read_text())
        receipt['manifest_sha256'] = manifest_hash(entries)
        receipt['source_files'] = len(entries)
        receipt['archives']['source.tar.gz'] = digest_file(out / 'source.tar.gz')
        if (out / 'rust-runtime.tar.gz').exists():
            receipt['archives']['rust-runtime.tar.gz'] = digest_file(out / 'rust-runtime.tar.gz')
        (out / 'receipt.json').write_text(json.dumps(receipt, indent=2) + '\n')
        print(json.dumps({'package': str(out), 'source_files': len(entries), 'manifest_sha256': receipt['manifest_sha256']}))
        return
    go = Path('/home/rian/go/pkg/mod/golang.org/toolchain@v0.0.1-go1.25.12.linux-amd64')
    rust = Path('/home/rian/.rustup/toolchains/stable-x86_64-unknown-linux-gnu')
    if '--reuse-toolchains' not in sys.argv:
        with tarfile.open(out / 'rust-runtime.tar.gz', 'w:gz') as tar:
            for name in ('ld-linux-x86-64.so.2', 'libc.so.6', 'libpthread.so.0', 'libdl.so.2', 'librt.so.1',
                         'libm.so.6', 'libgcc_s.so.1', 'libz.so.1', 'libresolv.so.2', 'libutil.so.1'):
                add_file(tar, Path('/lib/x86_64-linux-gnu') / name, name)
    if '--reuse-toolchains' not in sys.argv:
        with tarfile.open(out / 'toolchains.tar.gz', 'w:gz') as tar:
            for root, prefix, keep in [(go, 'go', ('bin', 'src', 'pkg', 'lib')), (rust, 'rust', ('bin', 'lib'))]:
                for folder in keep:
                    for p in (root / folder).rglob('*'):
                        if p.is_file() and '/rustlib/src/' not in str(p):
                            add_file(tar, p, prefix + '/' + str(p.relative_to(root)))
    module_files = set()
    modulecache = Path('/home/rian/go/pkg/mod/cache/download')
    for cwd in (repo, repo / 'tools/motgate', repo / 'tools/nodebank/probe'):
        env = {**os.environ, 'GOCACHE': '/tmp/mot-router-go-cache', 'GOTOOLCHAIN': 'local'}
        env['GOPROXY'] = 'off'
        tags = ['-tags=with_utls,with_grpc'] if cwd.name == 'probe' else []
        p = subprocess.run([str(go / 'bin/go'), 'list', '-buildvcs=false', '-deps', '-test', *tags,
                            '-f', '{{if .Module}}{{.Module.Path}} {{.Module.Version}}{{end}}', './...'],
                           cwd=cwd, env=env, capture_output=True, text=True)
        if p.returncode: raise RuntimeError(p.stderr[:2000])
        for entry in sorted(set(p.stdout.splitlines())):
            values = entry.split()
            if len(values) != 2: continue
            module_path, version = values
            escaped = re.sub(r'[A-Z]', lambda m: '!' + m[0].lower(), module_path)
            folder = modulecache / escaped / '@v'
            for ext in ('.zip', '.ziphash', '.mod', '.info'):
                file = folder / (version + ext)
                if not file.exists():
                    if ext == '.info': continue
                    raise RuntimeError('Pinned module missing from cache: ' + module_path)
                module_files.add(file)
    cargo = Path('/home/rian/.cargo/registry')
    lock = (repo / 'tools/nodebank/Cargo.lock').read_text()
    crates = re.findall(r'\[\[package\]\]\nname = "([^"]+)"\nversion = "([^"]+)"\nsource = "registry', lock)
    cargo_files = set()
    for name, version in crates:
        candidates = list((cargo / 'cache').glob('*/' + name + '-' + version + '.crate'))
        cargo_files.update(candidates)
        indexpath = ('1/' + name if len(name) == 1 else '2/' + name if len(name) == 2 else
                     '3/' + name[0] + '/' + name if len(name) == 3 else name[:2] + '/' + name[2:4] + '/' + name)
        cargo_files.update((cargo / 'index').glob('*/.cache/' + indexpath))
    cargo_files.update((cargo / 'index').glob('*/config.json'))
    with tarfile.open(out / 'dependencies.tar.gz', 'w:gz') as tar:
        for p in sorted(module_files): add_file(tar, p, 'modules/cache/download/' + str(p.relative_to(modulecache)))
        for p in sorted(cargo_files): add_file(tar, p, 'cargo/registry/' + str(p.relative_to(cargo)))
    receipt = {'production_router_sha256': digest_file('/tmp/arbyte-recovery-baseline'),
               'manifest_sha256': manifest_hash(entries), 'source_files': len(entries),
               'toolchain': {'go': '1.25.12', 'rust': subprocess.check_output([str(rust / 'bin/rustc'), '--version'], text=True).strip()},
               'archives': {p.name: digest_file(p) for p in out.glob('*.tar.gz')}}
    (out / 'receipt.json').write_text(json.dumps(receipt, indent=2) + '\n')
    print(json.dumps({'package': str(out), 'receipt': receipt, 'archive_bytes': {p.name: p.stat().st_size for p in out.glob('*.tar.gz')}}))


if __name__ == '__main__': main()
