#!/usr/bin/env bash
set -euo pipefail
stage=/tmp/mot-router-glm-route
backup=/var/backups/mot-router/glm-route-$(date -u +%Y%m%dT%H%M%SZ)
install -d -m 700 "$backup"
cp -a /opt/mot-router/mot-router "$backup/mot-router"
python3 - "$backup/router.db" <<'PY'
import sqlite3,sys
with sqlite3.connect('file:/opt/mot-router/data/mot-router.db?mode=ro',uri=True,timeout=30) as src, sqlite3.connect(sys.argv[1]) as dest:
 src.backup(dest,pages=256,sleep=0.02)
PY
sha256sum "$stage" > "$backup/artifact.sha256"
rollback() {
 systemctl stop mot-router || true
 install -o ubuntu -g ubuntu -m 755 "$backup/mot-router" /opt/mot-router/mot-router
 systemctl start mot-router
 echo "Binary lama dipulihkan: $backup" >&2
}
trap rollback ERR
systemctl stop mot-router
install -o ubuntu -g ubuntu -m 755 "$stage" /opt/mot-router/mot-router.new
mv /opt/mot-router/mot-router.new /opt/mot-router/mot-router
systemctl start mot-router
python3 - <<'PY'
import urllib.request,time
for attempt in range(20):
 try:
  with urllib.request.urlopen('http://127.0.0.1:8787/api/config',timeout=3) as r: assert r.status==200
  break
 except Exception: time.sleep(1)
else: raise RuntimeError('router health check failed')
PY
trap - ERR
printf 'Backup: %s\n' "$backup"
