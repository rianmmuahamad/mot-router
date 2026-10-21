#!/usr/bin/env bash
# Run with sudo on the VPS after copying artifacts into /tmp/arbyte-nodecap-5000.
set -euo pipefail
stage=/tmp/arbyte-nodecap-5000
backup=/var/backups/mot-router/nodecap-5000-$(date -u +%Y%m%dT%H%M%SZ)
install -d -m 700 "$backup"
cp -a /opt/mot-router/mot-router "$backup/mot-router"
cp -a /opt/mot-router/nodebank/mot-nodebank "$backup/mot-nodebank"
cp -a /opt/mot-router/nodebank/nodebank-runtime "$backup/nodebank-runtime"
cp -a /home/ubuntu/.local/libexec/mot-nodebank/receive.py "$backup/receive.py"
cp -a /etc/systemd/system/mot-router.service /etc/systemd/system/mot-nodebank.service /etc/systemd/system/mot-nodebank-runtime.service "$backup/"
python3 - "$backup" <<'PY'
import json,pathlib,sqlite3,sys,urllib.request
out=pathlib.Path(sys.argv[1])
for label,path in [('router','/opt/mot-router/data/mot-router.db'),('bank','/opt/mot-router/data/nodebank/bank.db')]:
 with sqlite3.connect('file:'+path+'?mode=ro',uri=True,timeout=30) as src, sqlite3.connect(str(out/(label+'.db'))) as dest:
  src.backup(dest,pages=256,sleep=0.02)
with urllib.request.urlopen('http://127.0.0.1:8792/api/stats',timeout=15) as r: stats=json.load(r)
(out/'before.json').write_text(json.dumps({k:stats.get(k) for k in ['counts','paused','pause_reason','background','combined_rss_bytes']}))
PY
sha256sum "$stage"/mot-router "$stage"/mot-nodebank "$stage"/nodebank-runtime > "$backup/artifacts.sha256"
rollback() {
 echo "Deployment gagal; memulihkan binary dari $backup" >&2
 systemctl stop mot-router mot-nodebank-runtime mot-nodebank || true
 install -o ubuntu -g ubuntu -m 755 "$backup/mot-router" /opt/mot-router/mot-router
 install -o ubuntu -g ubuntu -m 755 "$backup/mot-nodebank" /opt/mot-router/nodebank/mot-nodebank
 install -o ubuntu -g ubuntu -m 755 "$backup/nodebank-runtime" /opt/mot-router/nodebank/nodebank-runtime
 install -o ubuntu -g ubuntu -m 755 "$backup/receive.py" /home/ubuntu/.local/libexec/mot-nodebank/receive.py
 systemctl start mot-nodebank mot-nodebank-runtime mot-router
}
trap rollback ERR
systemctl stop mot-router mot-nodebank-runtime mot-nodebank
install -o ubuntu -g ubuntu -m 755 "$stage/mot-router" /opt/mot-router/mot-router.new
install -o ubuntu -g ubuntu -m 755 "$stage/mot-nodebank" /opt/mot-router/nodebank/mot-nodebank.new
install -o ubuntu -g ubuntu -m 755 "$stage/nodebank-runtime" /opt/mot-router/nodebank/nodebank-runtime.new
install -o ubuntu -g ubuntu -m 755 "$stage/receive.py" /home/ubuntu/.local/libexec/mot-nodebank/receive.py.new
mv /opt/mot-router/mot-router.new /opt/mot-router/mot-router
mv /opt/mot-router/nodebank/mot-nodebank.new /opt/mot-router/nodebank/mot-nodebank
mv /opt/mot-router/nodebank/nodebank-runtime.new /opt/mot-router/nodebank/nodebank-runtime
mv /home/ubuntu/.local/libexec/mot-nodebank/receive.py.new /home/ubuntu/.local/libexec/mot-nodebank/receive.py
systemctl start mot-nodebank
python3 - <<'READY'
import time,urllib.request
for attempt in range(20):
 try:
  with urllib.request.urlopen('http://127.0.0.1:8792/api/stats',timeout=3) as r: assert r.status==200
  break
 except Exception: time.sleep(1)
else: raise RuntimeError('bank API not ready')
READY
systemctl start mot-nodebank-runtime mot-router
python3 - "$backup" <<'PY'
import json,pathlib,sys,time,urllib.request
last=None
for attempt in range(20):
 try:
  with urllib.request.urlopen('http://127.0.0.1:9093/status',timeout=3) as r: status=json.load(r)
  with urllib.request.urlopen('http://127.0.0.1:8792/api/stats',timeout=3) as r: stats=json.load(r)
  with urllib.request.urlopen('http://127.0.0.1:8787/api/config',timeout=3) as r: assert r.status==200
  assert status['candidate_cap']==5000, status.get('candidate_cap')
  assert not status['source_error'], status['source_error']
  before=json.loads((pathlib.Path(sys.argv[1])/'before.json').read_text())
  if before['paused']: assert stats['paused'], 'paused scan resumed unexpectedly'
  assert stats['counts']['total'] >= before['counts']['total'], 'bank inventory unexpectedly reduced'
  report={'runtime':{k:status.get(k) for k in ['candidate_cap','total','unique_healthy_exit_ips','target_gap','source_error','pause_reason']},'bank':{k:stats.get(k) for k in ['counts','paused','pause_reason','combined_rss_bytes']}}
  (pathlib.Path(sys.argv[1])/'after.json').write_text(json.dumps(report))
  print(json.dumps(report));break
 except Exception as exc:
  last=exc;time.sleep(1)
else: raise RuntimeError('deployment health validation failed: '+str(last))
PY
trap - ERR
printf 'Backup: %s\n' "$backup"
