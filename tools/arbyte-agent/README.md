# Arbyte NullClaw operator

Installed on the Mot VPS as `arbyte-agent.service`, running under the dedicated
unprivileged `arbyte-agent` account. Native model tool calls use the authenticated
loopback gateway at `127.0.0.1:8798`, via the internal Arbyte router at port 8787.
No browser, local model, WARP, or Mihomo is required.

Default agent model: `arbyte/deepseek-v4.1-flash-free` through the existing
local Arbyte provider. Model changes preserve Telegram pairing and encrypted keys.

## Use from the laptop

```bash
# Interactive conversation; exit with /exit.
ssh -t mot 'sudo -u arbyte-agent arbyte-agent chat'

# A single task, using the same agent service and memory.
ssh mot 'sudo -u arbyte-agent arbyte-agent ask "Cek NodeBank dan jelaskan kendalanya"'

# Deterministic status without calling a model.
ssh mot 'sudo -u arbyte-agent arbyte-agent status'
ssh mot 'sudo -u arbyte-agent arbytectl jobs'

# Toggle one existing subscription source; does not expose its private URL.
ssh mot 'sudo -u arbyte-agent arbytectl sources enable MORPHEUS-best'
ssh mot 'sudo -u arbyte-agent arbytectl sources disable MORPHEUS-best'

# Explicit scan control.
ssh mot 'sudo -u arbyte-agent arbytectl scan pause'
ssh mot 'sudo -u arbyte-agent arbytectl scan resume'
```

All model requests, including chat, execute inside the memory-limited service.
The Python client serializes operator/audit requests and never prints its bearer
token. The native gateway uses non-streaming tool calls; CLI streaming did not
execute tools reliably with Muse during the initial setup test.

## Scheduled work

- `arbyte-maintenance.timer`: every five minutes after the previous run. Records
  VPS metrics, resumes background scan after startup, and imports at most one
  enabled due source. Imports are serialized; source failures back off one hour.
- `arbyte-audit.timer`: every six hours after the previous audit. Runs the AI via
  the authenticated gateway, checks tool execution evidence, and saves its report.
- NodeBank's background mode handles continuous bounded checks and healthy-node
  rechecks. Runtime pulls candidates every minute and checks them from the VPS.

These are VPS systemd timers, independent of Codex. Native NullClaw cron is
disabled because its agent jobs invoke the CLI streaming path. New schedules
can be installed by the VPS administrator using the same service templates.

Maintenance respects dashboard/manual pauses; `scan resume` explicitly enables
automatic work again. Work is deferred below 900 MiB available RAM or 2 GiB free
disk. Resource/network pauses use a 15-minute retry cooldown. It never enables
disabled sources automatically or sends AI prompts through each proxy. The AI
operator can enable/disable an existing source when authorized, including for
the user's delegated 1,000-IP stock goal. Changes affect future imports, preserve
source credentials and existing bank nodes, and are recorded in the action log.
Healthy proxy counts depend on actual source quality and tests; the runtime cap
is 1,000 candidates. Status distinguishes that cap from unique healthy exit IPs.

## Limits and access

NullClaw service: MemoryHigh=96 MiB, MemoryMax=128 MiB including children,
CPUQuota=25%, no swap. Maintenance: 64 MiB; audit client: 32 MiB. Existing
NodeBank bank/worker/runtime remain under their combined 384 MiB slice limit.
One operator/audit runs at a time. Model context is capped at 24k tokens and
tool iterations at 12. The LLM is served remotely by Arbyte, not on VPS RAM.

The agent has no generic sudo privileges. Recovery now provides bounded service,
build and deployment operations through the private broker described below.
No public gateway or new DNS record is installed.
Model key is dedicated (`Arbyte NullClaw Ops`) and stored in native `enc2:`
ChaCha20-Poly1305 format. Config, key, and state directories are private.

## Files on VPS

- Source and templates: `/opt/arbyte-agent`
- Agent state/config/workspace: `/var/lib/arbyte-agent/.nullclaw`
- Latest metrics: `/var/lib/arbyte-agent/ops/latest-status.json`
- Latest AI report: `/var/lib/arbyte-agent/ops/latest-audit.json`
- Bounded action log: `/var/lib/arbyte-agent/ops/actions.jsonl` (4 MiB + one rotation)
- Database/config backup before key/config changes: `/var/backups/arbyte-agent`

Logs and schedules:

```bash
ssh mot 'sudo journalctl -u arbyte-agent -u arbyte-audit -u arbyte-maintenance -n 50 --no-pager'
ssh mot 'systemctl list-timers arbyte-* --all --no-pager'
```

## Reproduce installation

NullClaw release `v2026.5.29`, official `nullclaw-linux-x86_64.bin`:
SHA-256 `966c524d3816531371e8657ffc84c40f47d94db89f6c70e816adce983e6d1f0a`.
Download only from the upstream release and verify its published digest first.
The setup needs existing Python 3 + cryptography, curl, and the running local
Arbyte router/NodeBank APIs. No new Python packages were installed.

Copy this directory to `/opt/arbyte-agent`, create the system account/home,
install the verified binary in `/usr/local/bin/nullclaw`, then run:

```bash
sudo python3 /opt/arbyte-agent/configure.py
sudo systemctl daemon-reload
sudo systemctl enable --now arbyte-agent.service
sudo systemctl start arbyte-audit.service
sudo systemctl enable --now arbyte-maintenance.timer arbyte-audit.timer
```

`configure.py` reuses the dedicated router key, preserves the operator token and
policy, backs up the router SQLite database via SQLite's backup API, and backs
up existing agent state before changes. Do not run it concurrently with agent
state changes; stop the agent first on subsequent reinstalls.

Tests: `python3 -m unittest discover -s tools/arbyte-agent -p 'test_*.py' -v`.
Measured acceptance results are recorded in `VALIDATION.json` after deployment.

## Automatic recovery

Recovery extends the existing operator; it preserves Telegram pairing, the model,
source enable/disable operations and manual scan pauses. `RECOVERY.md` contains
the incident runbook. Its permissions supersede the old read-only deployment policy.

`arbyte-repair-monitor.timer` observes every minute without an LLM request. Two
observations confirm an incident; later successful traffic or a live proof is
required to clear inference failures. Idle traffic does not prove recovery.
The root broker at `/run/arbyte-repair/broker.sock` authenticates local peer UIDs,
returns asynchronous job IDs, and restricts operations to registered Arbyte targets.

```bash
ssh mot 'sudo -u arbyte-agent arbytectl repair inspect'
ssh mot 'sudo -u arbyte-agent arbytectl repair status JOB_ID'
ssh mot 'sudo -u arbyte-agent arbytectl repair prepare --component motgate --reason "confirmed reproduction"'
ssh mot 'sudo -u arbyte-agent arbytectl repair test PREPARE_JOB --component motgate --reason "fixture fix"'
ssh mot 'sudo -u arbyte-agent arbytectl repair deploy TEST_JOB --component motgate --reason "tests passed"'
ssh mot 'sudo -u arbyte-agent arbytectl repair rollback DEPLOY_JOB --component motgate'
```

Source workspaces are under `/var/lib/arbyte-agent/repair-workspaces`. Immutable
artifacts and state live under `/var/lib/arbyte-repair`; backups are under
`/var/backups/arbyte-repair`. Existing tests and lockfiles are protected. Builders
run as `arbyte-build` with a separate network namespace, offline dependencies,
one build at a time, MemoryMax 512 MiB and CPUQuota 50%. Builds are deferred below
900 MiB available RAM or 2 GiB free disk. Broker: 96 MiB; monitor: 64 MiB;
NullClaw remains capped at 128 MiB. Completed artifacts/evidence expire after 14 days.

Deployment requires the tested artifact hash and unchanged baseline. Live SQLite
backups include WAL. Migration fixtures reject changes to existing account/usage
values. Rollback restores binary/config and source baseline, retaining the live DB.
NodeBank candidates also open a copied bank and must preserve existing data.

`repair verify` samples baseline, Playground, Anthropic streaming, OpenAI non-stream and real
usage, with at most one manual verification per hour. Deployment validation uses
the same proof. No declared tool is executed and no mass inference scan is run.
The monitor distinguishes access policy, authentication, quota, compatibility,
transport and sync failures. It never treats access denial as permission to evade
limits. Telegram uses the existing paired owner and a durable deduplicated outbox.

Sensitive operations have narrow interfaces: `repair account --component motgate
--account-id ID --operation revoke|restore --reason ...`, `repair config --component
motgate --changes '{"MOTGATE_DEFAULT_HOURS":24}' --reason ...`, and `repair credential
--component motgate|caddy --reason ...`. New passwords are stored in a root-private
credential file, never returned to the model. No token waiver or guessed usage
resolution is exposed. Guard settings overridden by service flags require a tested release.

### Activation

Installation enables monitoring and tested service recovery. Code/config/customer
repair by the agent stays locked until the independent model is configured and
passes actual native tool execution with the router unavailable. Policy is root-owned
at `/etc/arbyte-repair.json`: `independent_verified`, `code_repair_enabled`,
`independent_provider`, and `provider_config_sha256` bind activation to the tested
active provider configuration. Any config change invalidates activation. The
Gemini authentication decision is intentionally deferred; the installer cannot
silently enable code repair or change the current provider.

`package_recovery.py` creates a source manifest, pinned toolchains and dependency
archives from the trusted laptop cache. `install_recovery.py` validates hashes and
the exact reproduced production router binary before installing. Initial fixture
tests use copies of VPS databases and isolated temporary services, not production
restarts. Installation preserves the existing Telegram/provider config and backs
up operator code/config/state before changing it.
