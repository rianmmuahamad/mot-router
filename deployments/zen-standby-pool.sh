#!/usr/bin/env bash
set -euo pipefail
stage=/tmp/mot-zen-pool-release
runtime_changed=1
if cmp -s "$stage/nodebank-runtime" /opt/mot-router/nodebank/nodebank-runtime; then runtime_changed=0; fi
backup=/var/backups/mot-router/zen-pool-$(date -u +%Y%m%dT%H%M%SZ)
# No restart while users have active requests/tunnels. Deferred safely if busy.
python3 - <<'PY'
import json,time,urllib.request
for attempt in range(45):
 try:
  def get(port,path):
   with urllib.request.urlopen(f'http://127.0.0.1:{port}{path}',timeout=2) as r:return json.load(r)
  guard=get(8791,'/healthz');runtime=get(9093,'/status')
  if guard.get('inflight',1)==0 and runtime.get('active_connections',1)==0:break
 except Exception:pass
 time.sleep(1)
else:raise SystemExit('Deployment ditunda: request/tunnel masih aktif; binary produksi belum diubah')
PY
install -d -m 700 "$backup"
cp -a /opt/mot-router/mot-router "$backup/mot-router"
cp -a /opt/mot-router/nodebank/nodebank-runtime "$backup/nodebank-runtime"
cp -a /etc/mot-router "$backup/config"
systemctl cat mot-router mot-nodebank-runtime > "$backup/services.txt"
python3 - "$backup" <<'PY'
import sqlite3,sys,pathlib
backup=pathlib.Path(sys.argv[1])
for name,path in [('router','/opt/mot-router/data/mot-router.db'),('motgate','/opt/mot-router/data/motgate.db')]:
 if not pathlib.Path(path).is_file():continue
 with sqlite3.connect(f'file:{path}?mode=ro',uri=True,timeout=30) as src,sqlite3.connect(str(backup/f'{name}.db')) as dest:src.backup(dest,pages=256,sleep=0.02)
PY
sha256sum "$stage/mot-router" "$stage/nodebank-runtime" "$stage/source.tar.gz" > "$backup/artifacts.sha256"
rollback() {
 trap - ERR
 systemctl stop mot-router || true
 if [ "$runtime_changed" = 1 ]; then systemctl stop mot-nodebank-runtime || true; fi
 install -o ubuntu -g ubuntu -m 755 "$backup/mot-router" /opt/mot-router/mot-router
 if [ "$runtime_changed" = 1 ]; then install -o ubuntu -g ubuntu -m 755 "$backup/nodebank-runtime" /opt/mot-router/nodebank/nodebank-runtime; fi
 if [ "$runtime_changed" = 1 ]; then systemctl start mot-nodebank-runtime; fi
 systemctl start mot-router
 echo "Rollback binary selesai. Database live dipertahankan. Backup: $backup" >&2
}
trap rollback ERR
systemctl stop mot-router
if [ "$runtime_changed" = 1 ]; then systemctl stop mot-nodebank-runtime; fi
# Initial rollout is disabled. No bank, account, quota or request row is changed.
python3 - <<'PY'
import sqlite3,json
with sqlite3.connect('/opt/mot-router/data/mot-router.db',timeout=30) as db:
 db.execute('INSERT INTO settings(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value',('zen_pool.config.v1',json.dumps({'enabled':False,'parallelism':1})))
PY
install -o ubuntu -g ubuntu -m 755 "$stage/mot-router" /opt/mot-router/mot-router.new
if [ "$runtime_changed" = 1 ]; then install -o ubuntu -g ubuntu -m 755 "$stage/nodebank-runtime" /opt/mot-router/nodebank/nodebank-runtime.new; fi
mv /opt/mot-router/mot-router.new /opt/mot-router/mot-router
if [ "$runtime_changed" = 1 ]; then mv /opt/mot-router/nodebank/nodebank-runtime.new /opt/mot-router/nodebank/nodebank-runtime; fi
install -d -o ubuntu -g ubuntu -m 755 /opt/mot-router/releases/zen-pool
install -o ubuntu -g ubuntu -m 644 "$stage/source.tar.gz" /opt/mot-router/releases/zen-pool/source.tar.gz
if [ "$runtime_changed" = 1 ]; then systemctl start mot-nodebank-runtime; fi
systemctl start mot-router
python3 - <<'PY'
import urllib.request,json,time
for attempt in range(20):
 try:
  with urllib.request.urlopen('http://127.0.0.1:8787/api/zen-pool',timeout=2) as r:pool=json.load(r)
  with urllib.request.urlopen('http://127.0.0.1:9093/status',timeout=2) as r:runtime=json.load(r)
  assert pool['config']=={'enabled':False,'parallelism':1}
  assert runtime['tunnel_capacity']==64 and 'active_tunnels' in runtime
  with urllib.request.urlopen('http://127.0.0.1:8787/',timeout=2) as r:assert b'zenPoolCard' in r.read()
  break
 except Exception:time.sleep(1)
else:raise RuntimeError('Validasi deployment gagal')
PY
trap - ERR
printf 'Backup: %s\n' "$backup"
