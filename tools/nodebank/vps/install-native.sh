#!/usr/bin/env bash
# Run on the VPS with a staging directory containing the verified binaries.
set -euo pipefail
stage=${1:?Usage: install-native.sh STAGING_DIRECTORY}
backup="/opt/mot-router/backups/nodebank-native-$(date -u +%Y%m%dT%H%M%SZ)"
sudo install -d -m 700 -o ubuntu -g ubuntu "$backup"
cp /opt/mot-router/mot-router "$backup/mot-router"
cp /opt/mot-router/nodebank/mot-nodebank "$backup/mot-nodebank"
sudo cp /etc/systemd/system/mot-nodebank.service "$backup/mot-nodebank.service"
python3 - "$backup/routing-settings.json" <<'PY'
import json, sqlite3, sys, os
with sqlite3.connect('/opt/mot-router/data/mot-router.db') as db:
    values = dict(db.execute("SELECT key,value FROM settings WHERE key IN ('mihomo_enabled','mihomo_rotate_on_limit','mihomo_group')"))
with open(sys.argv[1], 'w') as f:
    json.dump({'enabled': values.get('mihomo_enabled') == '1', 'rotate_on_limit': values.get('mihomo_rotate_on_limit') != '0', 'group': values.get('mihomo_group', '')}, f)
os.chmod(sys.argv[1], 0o600)
PY
if sudo test -f /etc/systemd/system/mot-router.service.d/nodebank-native.conf; then
 sudo cp /etc/systemd/system/mot-router.service.d/nodebank-native.conf "$backup/nodebank-native.conf"
fi
rollback() {
 code=$?
 trap - ERR
 set +e
 sudo systemctl stop mot-nodebank-runtime
 cp "$backup/mot-router" /opt/mot-router/mot-router.rollback
 mv /opt/mot-router/mot-router.rollback /opt/mot-router/mot-router
 cp "$backup/mot-nodebank" /opt/mot-router/nodebank/mot-nodebank.rollback
 mv /opt/mot-router/nodebank/mot-nodebank.rollback /opt/mot-router/nodebank/mot-nodebank
 sudo cp "$backup/mot-nodebank.service" /etc/systemd/system/mot-nodebank.service
 if test -f "$backup/nodebank-native.conf"; then
  sudo cp "$backup/nodebank-native.conf" /etc/systemd/system/mot-router.service.d/nodebank-native.conf
 else
  sudo rm -f /etc/systemd/system/mot-router.service.d/nodebank-native.conf
 fi
 sudo systemctl daemon-reload
 sudo systemctl restart mot-nodebank mot-router
 curl -fsS -X PUT http://127.0.0.1:8787/api/mihomo -H 'Content-Type: application/json' --data-binary "@$backup/routing-settings.json" >/dev/null
 printf 'Deployment rolled back. Backup: %s\n' "$backup" >&2
 exit "$code"
}
trap rollback ERR
# Atomic replacement keeps the old executing inode valid until restart.
install -m 700 "$stage/mot-nodebank" /opt/mot-router/nodebank/mot-nodebank.new
mv /opt/mot-router/nodebank/mot-nodebank.new /opt/mot-router/nodebank/mot-nodebank
install -m 700 "$stage/nodebank-runtime" /opt/mot-router/nodebank/nodebank-runtime.new
mv /opt/mot-router/nodebank/nodebank-runtime.new /opt/mot-router/nodebank/nodebank-runtime
install -m 700 "$stage/mot-router-native" /opt/mot-router/mot-router.new
mv /opt/mot-router/mot-router.new /opt/mot-router/mot-router
sudo install -m 644 "$stage/mot-nodebank-vps.service" /etc/systemd/system/mot-nodebank.service
sudo install -m 644 "$stage/mot-nodebank-runtime.service" /etc/systemd/system/mot-nodebank-runtime.service
sudo install -m 644 "$stage/mot-nodebank.slice" /etc/systemd/system/mot-nodebank.slice
sudo install -d -m 755 /etc/systemd/system/mot-router.service.d
cat <<'CONFIG' | sudo tee /etc/systemd/system/mot-router.service.d/nodebank-native.conf >/dev/null
[Service]
Environment=MOT_NODEBANK_RUNTIME=1
CONFIG
sudo systemctl daemon-reload
sudo systemctl restart mot-nodebank
sudo systemctl enable --now mot-nodebank-runtime
for i in $(seq 1 30); do
 if curl -fsS http://127.0.0.1:9093/version >/dev/null; then break; fi
 sleep 1
done
curl -fsS http://127.0.0.1:9093/version >/dev/null
sudo systemctl restart mot-router
# Preserve credentials/provider settings; only enable the requested MOT routing.
for i in $(seq 1 30); do
 if curl -fsS http://127.0.0.1:8787/api/mihomo >/dev/null; then break; fi
 sleep 1
done
curl -fsS -X PUT http://127.0.0.1:8787/api/mihomo \
 -H 'Content-Type: application/json' \
 --data '{"enabled":true,"rotate_on_limit":true,"group":"MOT"}' >/dev/null
printf 'Backup: %s\n' "$backup"
sudo systemctl is-active mot-nodebank mot-nodebank-runtime mot-router
