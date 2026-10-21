#!/usr/bin/env bash
set -euo pipefail
stage=/tmp/mot-manual-ten-release
backup=/var/backups/mot-router/manual-ten-$(date -u +%Y%m%dT%H%M%SZ)
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
cp -a /opt/mot-router/nodebank/nodebank-runtime "$backup/nodebank-runtime"
cp -a /opt/mot-router/releases/zen-pool/source.tar.gz "$backup/source.tar.gz"
cp -a /etc/mot-router "$backup/config"
python3 - "$backup" <<'PY'
import sqlite3,sys,urllib.request,json
with sqlite3.connect('file:/opt/mot-router/data/mot-router.db?mode=ro',uri=True,timeout=30) as src,sqlite3.connect(sys.argv[1]+'/router.db') as dest:src.backup(dest,pages=256,sleep=.02)
with urllib.request.urlopen('http://127.0.0.1:8787/api/zen-pool',timeout=5) as r:state=json.load(r)
with open(sys.argv[1]+'/pool-config.json','w') as f:json.dump(state['config'],f)
PY
rollback(){
 trap - ERR
 systemctl stop mot-router mot-nodebank-runtime || true
 install -o ubuntu -g ubuntu -m 755 "$backup/mot-router" /opt/mot-router/mot-router
 install -o ubuntu -g ubuntu -m 755 "$backup/nodebank-runtime" /opt/mot-router/nodebank/nodebank-runtime
 install -o ubuntu -g ubuntu -m 644 "$backup/source.tar.gz" /opt/mot-router/releases/zen-pool/source.tar.gz
 systemctl start mot-nodebank-runtime mot-router
 echo "Rollback binary; database live dipertahankan. Backup: $backup" >&2
}
trap rollback ERR
systemctl stop mot-router
python3 - "$backup" <<'PY'
import urllib.request,sys,json,os
with urllib.request.urlopen('http://127.0.0.1:9093/proxies',timeout=5) as r:raw=r.read(8*1024*1024+1)
assert len(raw)<=8*1024*1024
with open(sys.argv[1]+'/runtime-health.json','wb') as f:f.write(raw)
PY
# Carry fresh measurements tied to configuration hashes across this upgrade.
install -o ubuntu -g ubuntu -m 600 "$backup/runtime-health.json" /opt/mot-router/data/nodebank/healthy-v1.json.restart-health.json
systemctl stop mot-nodebank-runtime
install -o ubuntu -g ubuntu -m 755 "$stage/mot-router" /opt/mot-router/mot-router.new
install -o ubuntu -g ubuntu -m 755 "$stage/nodebank-runtime" /opt/mot-router/nodebank/nodebank-runtime.new
mv /opt/mot-router/mot-router.new /opt/mot-router/mot-router
mv /opt/mot-router/nodebank/nodebank-runtime.new /opt/mot-router/nodebank/nodebank-runtime
install -o ubuntu -g ubuntu -m 644 "$stage/source.tar.gz" /opt/mot-router/releases/zen-pool/source.tar.gz
systemctl start mot-nodebank-runtime mot-router
python3 - "$backup" <<'PY'
import json,urllib.request,time,sys
expected=json.load(open(sys.argv[1]+'/pool-config.json'))
saved=json.load(open(sys.argv[1]+'/runtime-health.json'))['proxies'];selected=saved['MOT']['now'];n=saved.get(selected,{})
for _ in range(30):
 try:
  def get(port,path):
   with urllib.request.urlopen(f'http://127.0.0.1:{port}{path}',timeout=2) as r:return json.load(r)
  pool=get(8787,'/api/zen-pool');runtime=get(9093,'/status')
  assert pool['config']==expected and runtime['selection_cas'] is True
  if selected=='DIRECT' or (n.get('alive') and 0<=time.time()-n.get('checked_at',0)<3600):assert runtime['current']==selected
  with urllib.request.urlopen('http://127.0.0.1:8787/',timeout=2) as r:html=r.read()
  assert b'mihomoProbeModel' in html and b'runMihomoBatch' in html
  break
 except Exception:time.sleep(1)
else:raise RuntimeError('Validasi batch manual gagal')
print(json.dumps({'pool_config':pool['config'],'selection_before':selected,'selection_after':runtime['current'],'healthy_ips':runtime['unique_healthy_exit_ips']}))
PY
rm /opt/mot-router/data/nodebank/healthy-v1.json.restart-health.json
sha256sum /opt/mot-router/mot-router /opt/mot-router/nodebank/nodebank-runtime /opt/mot-router/releases/zen-pool/source.tar.gz > "$backup/deployed.sha256"
trap - ERR
printf 'Backup: %s\n' "$backup"
