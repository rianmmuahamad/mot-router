#!/usr/bin/env bash
set -euo pipefail
umask 077
stage=/tmp/mot-compatibility-20261019
exec 9>/run/mot-router-deploy.lock
flock -n 9
cd "$stage"
sha256sum -c SHA256SUMS
expected=14cfcccb1edffdabfefa7aa60256e67de20282e5a4c2d964f750a4a59d737716
actual=$(sha256sum /opt/mot-router/mot-router)
test "${actual%% *}" = "$expected" || { echo 'Binary produksi berubah; deployment dihentikan.' >&2; exit 75; }
python3 - <<'PY'
import urllib.request,json,time
for attempt in range(45):
 try:
  with urllib.request.urlopen('http://127.0.0.1:8791/healthz',timeout=2) as r:g=json.load(r)
  with urllib.request.urlopen('http://127.0.0.1:9093/status',timeout=2) as r:n=json.load(r)
  if g.get('inflight',1)==0 and n.get('active_connections',1)==0:break
 except Exception:pass
 time.sleep(1)
else:raise SystemExit('Deployment ditunda: request masih aktif')
PY
backup=/var/backups/mot-router/compatibility-$(date -u +%Y%m%dT%H%M%SZ)
install -d -m 700 "$backup"
cp -a /opt/mot-router/mot-router "$backup/mot-router"
cp -a /etc/mot-router "$backup/config"
cp -a /opt/mot-router/releases/zen-pool/source.tar.gz "$backup/previous-archive.tar.gz"
cp "$stage/previous-source.tar.gz" "$backup/source.tar.gz"
python3 - "$backup" <<'PY'
import sys,json,sqlite3,urllib.request,subprocess
root=sys.argv[1]
with sqlite3.connect('file:/opt/mot-router/data/mot-router.db?mode=ro',uri=True,timeout=30) as src,sqlite3.connect(root+'/router.db') as dst:src.backup(dst,pages=256,sleep=.02)
snapshot={}
for name,port,path in [('runtime',9093,'/status'),('bank',8792,'/api/stats')]:
 with urllib.request.urlopen(f'http://127.0.0.1:{port}{path}',timeout=5) as r:d=json.load(r)
 snapshot[name]={k:d.get(k) for k in (['current'] if name=='runtime' else ['paused','background'])}
snapshot['runtime_pid']=subprocess.check_output(['systemctl','show','mot-nodebank-runtime','-p','MainPID','--value'],text=True).strip()
with open(root+'/before.json','w') as f:json.dump(snapshot,f)
PY
rollback() {
 trap - ERR
 systemctl stop mot-router || true
 install -o ubuntu -g ubuntu -m 755 "$backup/mot-router" /opt/mot-router/mot-router.new
 mv /opt/mot-router/mot-router.new /opt/mot-router/mot-router
 install -o ubuntu -g ubuntu -m 644 "$backup/source.tar.gz" /opt/mot-router/releases/zen-pool/source.tar.gz
 systemctl start mot-router
 echo "Binary lama dipulihkan. Database live dipertahankan. Backup: $backup" >&2
}
trap rollback ERR
systemctl stop mot-router
install -o ubuntu -g ubuntu -m 755 "$stage/mot-router" /opt/mot-router/mot-router.new
mv /opt/mot-router/mot-router.new /opt/mot-router/mot-router
install -o ubuntu -g ubuntu -m 644 "$stage/source.tar.gz" /opt/mot-router/releases/zen-pool/source.tar.gz
systemctl start mot-router
python3 - "$backup" <<'PY'
import urllib.request,json,subprocess,sys,time
before=json.load(open(sys.argv[1]+'/before.json'))
for attempt in range(20):
 try:
  with urllib.request.urlopen('http://127.0.0.1:8787/api/models/status',timeout=2) as r:cache=json.load(r)
  assert isinstance(cache.get('cached'),bool)
  for name,port,path in [('runtime',9093,'/status'),('bank',8792,'/api/stats')]:
   with urllib.request.urlopen(f'http://127.0.0.1:{port}{path}',timeout=2) as r:d=json.load(r)
   for key,value in before[name].items():assert d.get(key)==value,(name,key)
  pid=subprocess.check_output(['systemctl','show','mot-nodebank-runtime','-p','MainPID','--value'],text=True).strip()
  assert pid==before['runtime_pid']
  for service in ['mot-router','motgate','guard','mot-nodebank','mot-nodebank-runtime']:
   assert subprocess.run(['systemctl','is-active','--quiet',service]).returncode==0,service
  break
 except Exception:time.sleep(1)
else:raise RuntimeError('Validasi deployment gagal')
print(json.dumps({'validation':'passed','nodebank_pause_preserved':True,'runtime_restarted':False,'manual_production_inference_tests':0,'backup':sys.argv[1]}))
PY
sha256sum /opt/mot-router/mot-router /opt/mot-router/releases/zen-pool/source.tar.gz > "$backup/deployed.sha256"
trap - ERR
