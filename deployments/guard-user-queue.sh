#!/usr/bin/env bash
# Run as root on VPS after native build and fixture tests in the staging directory.
set -euo pipefail
stage=/tmp/arbyte-guard-queue
backup=/var/backups/mot-router/guard-queue-$(date -u +%Y%m%dT%H%M%SZ)
install -d -m 700 "$backup"
cp -a /opt/mot-router/tools/guard/guard "$backup/guard"
cp -a /etc/systemd/system/guard.service "$backup/guard.service"
cp -a /etc/mot-router/guard.env "$backup/guard.env"
cp -a "$stage/src" "$backup/src"
cp -a "$stage/tests" "$backup/tests"
cp -a "$stage/README.md" "$stage/Makefile" "$backup/"
sha256sum "$stage/guard" > "$backup/artifact.sha256"
# Do not terminate an inference in progress. Leave the prepared release staged
# if the caller needs to pause submissions to obtain a quiet switchover window.
python3 - "$backup" <<'PY'
import json,pathlib,sys,time,urllib.request
out=pathlib.Path(sys.argv[1])
for attempt in range(90):
 with urllib.request.urlopen('http://127.0.0.1:8791/healthz',timeout=2) as r: state=json.load(r)
 if attempt==0:(out/'before.json').write_text(json.dumps(state))
 if state['inflight']==0:
  print('guard idle; proceeding',flush=True);break
 time.sleep(.5)
else:
 print('DEPLOY_DEFERRED: active requests did not drain; production binary unchanged',flush=True)
 raise SystemExit(75)
PY
rollback() {
 systemctl stop guard || true
 install -o root -g motgate -m 750 "$backup/guard" /opt/mot-router/tools/guard/guard
 systemctl start guard
 echo "Guard rollback: $backup" >&2
}
trap rollback ERR
systemctl stop guard
install -o root -g motgate -m 750 "$stage/guard" /opt/mot-router/tools/guard/guard.new
mv /opt/mot-router/tools/guard/guard.new /opt/mot-router/tools/guard/guard
systemctl start guard
python3 - "$backup" <<'PY'
import json,pathlib,sys,time,urllib.request
for attempt in range(15):
 try:
  with urllib.request.urlopen('http://127.0.0.1:8791/healthz',timeout=2) as r: state=json.load(r)
  assert state['per_user_inflight']==4 and state['queue_wait_ms']==60000,state
  assert 'waited' in state and 'queue_timeouts' in state,state
  (pathlib.Path(sys.argv[1])/'after.json').write_text(json.dumps(state))
  print(json.dumps(state),flush=True);break
 except Exception:time.sleep(1)
else:raise RuntimeError('new guard health/config check failed')
PY
trap - ERR
printf 'Backup: %s\n' "$backup"
