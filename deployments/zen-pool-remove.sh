#!/usr/bin/env bash
set -euo pipefail
stage=/tmp/mot-pool-remove-release
backup=/var/backups/mot-router/pool-remove-$(date -u +%Y%m%dT%H%M%SZ)
python3 - <<'PY'
import json,time,urllib.request
for _ in range(45):
 try:
  def get(port,path):
   with urllib.request.urlopen(f'http://127.0.0.1:{port}{path}',timeout=2) as r:return json.load(r)
  if get(8791,'/healthz').get('inflight',1)==0 and get(9093,'/status').get('active_connections',1)==0:break
 except Exception:pass
 time.sleep(1)
else:raise SystemExit('Deployment ditunda: request masih aktif')
PY
install -d -m 700 "$backup"
cp -a /opt/mot-router/mot-router "$backup/mot-router"
cp -a /etc/mot-router "$backup/config"
cp -a /opt/mot-router/releases/zen-pool/source.tar.gz "$backup/source.tar.gz"
if [ -f /tmp/mot-pool-removal-before.json ]; then install -m 600 /tmp/mot-pool-removal-before.json "$backup/pool-before-removal.json"; fi
python3 - "$backup" <<'PY'
import sqlite3,sys,json,urllib.request,subprocess
with sqlite3.connect('file:/opt/mot-router/data/mot-router.db?mode=ro',uri=True,timeout=30) as src,sqlite3.connect(sys.argv[1]+'/router.db') as dest:src.backup(dest,pages=256,sleep=.02)
with urllib.request.urlopen('http://127.0.0.1:9093/status',timeout=5) as r:runtime=json.load(r)
state={'current':runtime['current'],'runtime_pid':subprocess.check_output(['systemctl','show','mot-nodebank-runtime','-p','MainPID','--value'],text=True).strip()}
with open(sys.argv[1]+'/runtime-before.json','w') as f:json.dump(state,f)
PY
rollback() {
 trap - ERR
 systemctl stop mot-router || true
 install -o ubuntu -g ubuntu -m 755 "$backup/mot-router" /opt/mot-router/mot-router
 install -o ubuntu -g ubuntu -m 644 "$backup/source.tar.gz" /opt/mot-router/releases/zen-pool/source.tar.gz
 systemctl start mot-router
 echo "Binary lama dipulihkan; database live dipertahankan. Backup: $backup" >&2
}
trap rollback ERR
systemctl stop mot-router
install -o ubuntu -g ubuntu -m 755 "$stage/mot-router" /opt/mot-router/mot-router.new
mv /opt/mot-router/mot-router.new /opt/mot-router/mot-router
install -o ubuntu -g ubuntu -m 644 "$stage/source.tar.gz" /opt/mot-router/releases/zen-pool/source.tar.gz
systemctl start mot-router
python3 - "$backup" <<'PY'
import json,sys,time,urllib.request,urllib.error,subprocess
before=json.load(open(sys.argv[1]+'/runtime-before.json'))
for _ in range(20):
 try:
  with urllib.request.urlopen('http://127.0.0.1:8787/',timeout=2) as r:html=r.read()
  assert b'Satu node, tanpa rotasi' in html
  assert all(x not in html for x in [b'zenPoolCard',b'runMihomoBatch',b'mihomoProbeModel',b'10 IP'])
  try:
   urllib.request.urlopen('http://127.0.0.1:8787/api/zen-pool',timeout=2)
   raise RuntimeError('API pool masih aktif')
  except urllib.error.HTTPError as e:assert e.code==404
  with urllib.request.urlopen('http://127.0.0.1:9093/status',timeout=2) as r:runtime=json.load(r)
  pid=subprocess.check_output(['systemctl','show','mot-nodebank-runtime','-p','MainPID','--value'],text=True).strip()
  assert pid==before['runtime_pid']
  assert runtime['current']==before['current']
  break
 except Exception:time.sleep(1)
else:raise RuntimeError('Validasi pengembalian rotasi gagal')
print(json.dumps({'pool_api_status':404,'runtime_restarted':False,'selected_node':runtime['current'],'healthy_ips':runtime['unique_healthy_exit_ips']}))
PY
sha256sum /opt/mot-router/mot-router /opt/mot-router/releases/zen-pool/source.tar.gz > "$backup/deployed.sha256"
trap - ERR
printf 'Backup: %s\n' "$backup"
