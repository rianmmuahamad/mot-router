#!/usr/bin/env bash
set -euo pipefail
stage=/tmp/mot-manual-retry-release
backup=/var/backups/mot-router/manual-retry-$(date -u +%Y%m%dT%H%M%SZ)
python3 - <<'PY'
import json,time,urllib.request
for _ in range(45):
 try:
  def get(port,path):
   with urllib.request.urlopen(f'http://127.0.0.1:{port}{path}',timeout=2) as r:return json.load(r)
  if get(8791,'/healthz').get('inflight',1)==0 and get(9093,'/status').get('active_connections',1)==0:break
 except Exception:pass
 time.sleep(1)
else:raise SystemExit('Deployment ditunda: request masih aktif; produksi belum diubah')
PY
install -d -m 700 "$backup"
cp -a /opt/mot-router/mot-router "$backup/mot-router"
cp -a /etc/mot-router "$backup/config"
cp -a /opt/mot-router/releases/zen-pool/source.tar.gz "$backup/source.tar.gz"
python3 - "$backup" <<'PY'
import sqlite3,sys,json
with sqlite3.connect('file:/opt/mot-router/data/mot-router.db?mode=ro',uri=True,timeout=30) as src,sqlite3.connect(sys.argv[1]+'/router.db') as dest:src.backup(dest,pages=256,sleep=.02)
PY
rollback() {
 trap - ERR
 systemctl stop mot-router || true
 install -o ubuntu -g ubuntu -m 755 "$backup/mot-router" /opt/mot-router/mot-router
 install -o ubuntu -g ubuntu -m 644 "$backup/source.tar.gz" /opt/mot-router/releases/zen-pool/source.tar.gz
 systemctl start mot-router
 echo "Binary lama dipulihkan; database live tetap. Backup: $backup" >&2
}
trap rollback ERR
python3 - "$backup" <<'PY'
import json,sys,urllib.request
with urllib.request.urlopen('http://127.0.0.1:8787/api/zen-pool',timeout=5) as r:p=json.load(r)
with open(sys.argv[1]+'/pool-config.json','w') as f:json.dump(p['config'],f)
PY
systemctl stop mot-router
install -o ubuntu -g ubuntu -m 755 "$stage/mot-router" /opt/mot-router/mot-router.new
mv /opt/mot-router/mot-router.new /opt/mot-router/mot-router
install -o ubuntu -g ubuntu -m 644 "$stage/source.tar.gz" /opt/mot-router/releases/zen-pool/source.tar.gz
systemctl start mot-router
python3 - "$backup" <<'PY'
import json,sys,time,urllib.request
expected=json.load(open(sys.argv[1]+'/pool-config.json'))
for _ in range(20):
 try:
  with urllib.request.urlopen('http://127.0.0.1:8787/api/zen-pool',timeout=2) as r:p=json.load(r)
  with urllib.request.urlopen('http://127.0.0.1:8787/',timeout=2) as r:html=r.read()
  assert p['config']==expected
  assert b'Tes manual tersedia' in html and b'function zenProbeDate' in html and b'${esc(until)}' not in html
  assert all(m.get('reason')!='model_unavailable' for m in p.get('models',{}).values())
  break
 except Exception:time.sleep(1)
else:raise RuntimeError('Validasi patch status gagal')
PY
sha256sum /opt/mot-router/mot-router /opt/mot-router/releases/zen-pool/source.tar.gz > "$backup/deployed.sha256"
trap - ERR
printf 'Backup: %s\n' "$backup"
