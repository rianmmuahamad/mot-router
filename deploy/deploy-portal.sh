#!/usr/bin/env bash
# Deploy motgate + guard on the same VPS as mot-router.
#
# Run ON the VPS as root, from a checkout at /opt/mot-router that already has
# mot-router running (see docs/VPS-DEPLOY.md).
set -euo pipefail

REPO=/opt/mot-router
DOMAIN="${DOMAIN:-portal.coddx.store}"
ADMIN_PASSWORD="${ADMIN_PASSWORD:-}"
GUARD_MASTER_KEY="${GUARD_MASTER_KEY:-}"

if [[ $EUID -ne 0 ]]; then
  echo "run as root" >&2
  exit 1
fi

if [[ -z "$GUARD_MASTER_KEY" ]]; then
  echo "==> creating a mot-router master key for the guard"
  GUARD_MASTER_KEY=$(curl -fsS -X POST http://127.0.0.1:8787/api/keys \
    -H 'Content-Type: application/json' -d '{"name":"motgate"}' |
    sed -n 's/.*"key":"\([^"]*\)".*/\1/p')
  if [[ -z "$GUARD_MASTER_KEY" ]]; then
    echo "could not create master key; is mot-router running on :8787?" >&2
    exit 1
  fi
  echo "    master key: $GUARD_MASTER_KEY"
  echo "    (save this; it is written into guard.service)"
fi

if [[ -z "$ADMIN_PASSWORD" ]]; then
  ADMIN_PASSWORD=$(openssl rand -hex 16)
  echo "==> generated admin password: $ADMIN_PASSWORD"
fi

echo "==> building guard"
make -C "$REPO/tools/guard"

echo "==> building motgate"
( cd "$REPO/tools/motgate" && /usr/local/go/bin/go build -o motgate . )

echo "==> installing systemd units"
install -d /var/lib/motgate
chown mot:mot /var/lib/motgate

sed -e "s|CHANGE_ME|$GUARD_MASTER_KEY|" "$REPO/deploy/systemd/guard.service" \
  > /etc/systemd/system/guard.service
sed -e "s|CHANGE_ME|$ADMIN_PASSWORD|" -e "s|portal.coddx.store|$DOMAIN|g" \
  "$REPO/deploy/systemd/motgate.service" > /etc/systemd/system/motgate.service

systemctl daemon-reload
systemctl enable --now guard motgate
sleep 2
systemctl --no-pager --lines=5 status guard motgate || true

echo "==> Caddy"
if ! grep -q "$DOMAIN" /etc/caddy/Caddyfile 2>/dev/null; then
  cat "$REPO/deploy/Caddyfile.portal" >> /etc/caddy/Caddyfile
  systemctl reload caddy
  echo "    appended $DOMAIN block to /etc/caddy/Caddyfile"
else
  echo "    $DOMAIN already present in /etc/caddy/Caddyfile"
fi

echo
echo "==> checks"
curl -fsS http://127.0.0.1:8791/healthz && echo
curl -fsS http://127.0.0.1:8790/healthz && echo
echo
echo "Portal: https://$DOMAIN  (admin password: $ADMIN_PASSWORD)"
echo "DNS: create an A record for ${DOMAIN%%.*} -> this VPS, proxy OFF (grey) first,"
echo "     so Let's Encrypt can issue the certificate."