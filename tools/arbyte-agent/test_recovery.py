import concurrent.futures
import importlib.util
import json
import os
from pathlib import Path
import sqlite3
import tempfile
import unittest
from unittest.mock import patch

from recovery_core import State, classify, manifest, manifest_hash, valid_id
import recovery_broker as broker
import recovery_monitor as monitor
from recovery_verify import anthropic_stream, verify_nonstream


class RecoveryStateTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.state = State(Path(self.tmp.name) / 'state.db')

    def tearDown(self):
        self.tmp.cleanup()

    def test_error_classification_does_not_confuse_limit_with_compatibility(self):
        cases = [(403, 'FreeUsageLimitError Rate limit exceeded', 'rate_limit'),
                 (403, 'FreeTierError only be used from within OpenCode', 'access_policy'),
                 (401, 'invalid API key', 'authentication'),
                 (400, 'first message is not system prompt', 'compatibility'),
                 (400, 'No tool output found', 'compatibility'),
                 (502, 'malformed HTTP response "2a0"', 'transport'), (403, '', 'access_policy')]
        for status, detail, expected in cases:
            self.assertEqual(classify(status, detail), expected)

    def test_single_sample_is_not_incident_and_notifications_are_deduplicated(self):
        p = [{'kind': 'service_unhealthy', 'component': 'motgate'}]
        self.assertEqual(self.state.observe(p, 1000)[0]['state'], 'suspected')
        self.assertEqual(self.state.observe(p, 1060)[0]['state'], 'confirmed')
        self.state.observe(p, 1120)
        with self.state.connect() as db:
            self.assertEqual(db.execute('SELECT COUNT(*) FROM notifications').fetchone()[0], 1)
        self.state.observe([], 1180)
        self.assertEqual(len(self.state.incidents()), 1)
        self.state.observe([], 1240)
        self.assertFalse(self.state.incidents())
        with self.state.connect() as db:
            self.assertEqual(db.execute('SELECT COUNT(*) FROM notifications').fetchone()[0], 2)

    def test_one_active_job_and_three_candidates_per_hour(self):
        for i in range(3):
            job = self.state.new_job('test', 'mot-router', {}, patch=True)
            with self.assertRaises(ValueError):
                self.state.new_job('deploy', 'mot-router', {})
            self.state.update_job(job, 'failed', {'reason': 'fixture'})
        with self.assertRaises(ValueError):
            self.state.new_job('test', 'mot-router', {}, patch=True)

    def test_concurrent_submit_only_one_wins(self):
        def submit(_):
            try:
                return self.state.new_job('test', 'mot-router', {})
            except ValueError:
                return None
        with concurrent.futures.ThreadPoolExecutor(max_workers=2) as pool:
            result = list(pool.map(submit, range(2)))
        self.assertEqual(sum(bool(x) for x in result), 1)

    def test_incident_cooldown_and_attempt_budget(self):
        p = [{'kind': 'service_unhealthy', 'component': 'motgate'}]
        self.state.observe(p, 1000); self.state.observe(p, 1060)
        fp = 'service_unhealthy:motgate'
        for t in (2000, 3000, 4000):
            self.assertTrue(self.state.claim_incident(fp, now=t))
            self.assertFalse(self.state.claim_incident(fp, now=t + 1))
        self.assertFalse(self.state.claim_incident(fp, now=5000))

    def test_source_links_and_traversal_are_rejected(self):
        source = Path(self.tmp.name) / 'source'
        source.mkdir(); (source / 'a.go').write_text('package main')
        self.assertEqual(len(manifest(source)), 1)
        (source / 'secret').symlink_to('/etc/passwd')
        with self.assertRaises(ValueError): manifest(source)
        for value in ('../evil', '123', None):
            with self.assertRaises(ValueError): valid_id(value)


class BrokerTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        p = Path(self.tmp.name)
        self.patches = [patch.object(broker, 'ROOT', p / 'root'), patch.object(broker, 'BACKUPS', p / 'backups'),
                        patch.object(broker, 'WORKSPACES', p / 'workspaces'), patch.object(broker, 'POLICY', p / 'policy.json')]
        for item in self.patches: item.start()
        broker.POLICY.write_text(json.dumps({'independent_verified': False, 'code_repair_enabled': False}))
        self.b = broker.Broker()

    def tearDown(self):
        self.b.pool.shutdown(wait=True)
        for item in self.patches: item.stop()
        self.tmp.cleanup()

    def test_provider_gate_is_fail_closed_but_inspection_works(self):
        with self.assertRaises(ValueError):
            self.b.dispatch({'action': 'test', 'job_id': 'a' * 32, 'reason': 'fixture'}, 1234)
        with patch.object(broker, 'resources', return_value={}):
            self.assertEqual(self.b.inspect()['activation'], 'tested_procedures')

    def test_unknown_components_and_shell_fields_rejected(self):
        for data in ({'action': 'restart', 'component': 'sshd'},
                     {'action': 'restart', 'command': 'rm -rf /'},
                     {'action': 'test', 'job_id': '../../etc/passwd', 'reason': 'fixture'}):
            with self.assertRaises(ValueError): self.b.dispatch(data, 0)

    def test_restart_skips_healthy_service(self):
        with patch.object(broker, 'healthy', return_value=True), patch.object(self.b, 'activate') as restart:
            self.assertTrue(self.b.do_restart({'component': 'motgate'})['already_healthy'])
            restart.assert_not_called()

    def test_restart_failure_is_reported(self):
        with patch.object(broker, 'healthy', return_value=False), patch.object(self.b, 'activate'), patch.object(self.b, 'wait_health', return_value=False):
            with self.assertRaises(ValueError): self.b.do_restart({'component': 'motgate'})

    def test_low_memory_defers_before_touching_workspace(self):
        with patch.object(broker, 'resources', return_value={'available_bytes': 700 * 1024**2, 'disk_free_bytes': 10 * 1024**3}):
            result = self.b.do_test({'id': 'a' * 32, 'data': {}})
        self.assertEqual(result['_state'], 'deferred')

    def test_interrupted_jobs_become_available_without_redeploy(self):
        job = self.b.state.new_job('test', 'mot-router', {})
        self.b.state.update_job(job, 'running', {})
        self.b.recover_interrupted()
        self.assertEqual(self.b.state.job(job)['state'], 'interrupted')
        self.b.state.new_job('test', 'mot-router', {})

    def test_interrupted_deploy_restores_previous_binary(self):
        job = self.b.state.new_job('deploy', 'mot-router', {})
        self.b.state.update_job(job, 'running', {'backup': str(broker.BACKUPS / job)})
        with patch.object(self.b, 'restore') as restore:
            self.b.recover_interrupted()
        restore.assert_called_once()
        self.assertEqual(self.b.state.job(job)['state'], 'rolled_back')

    def test_tampered_artifact_cannot_deploy(self):
        tested = self.b.state.new_job('test', 'mot-router', {})
        self.b.state.update_job(tested, 'succeeded', {'artifact_sha256': 'not-the-hash'})
        p = broker.ROOT / 'releases' / tested
        p.mkdir(parents=True); (p / 'artifact').write_bytes(b'changed')
        with self.assertRaises(ValueError):
            self.b.do_deploy({'data': {'job_id': tested}, 'component': 'mot-router'})

    def test_failed_live_validation_rolls_back_without_replacing_database(self):
        target = broker.ROOT / 'binary'
        target.write_bytes(b'old')
        dbpath = broker.ROOT / 'customer.db'
        with sqlite3.connect(dbpath) as db:
            db.execute('CREATE TABLE usage(tokens INTEGER)'); db.execute('INSERT INTO usage VALUES(42)')
        component = {'binary': str(target), 'db': str(dbpath), 'kind': 'go'}
        tested = self.b.state.new_job('test', 'mot-router', {})
        p = broker.ROOT / 'releases' / tested
        p.mkdir(parents=True); (p / 'artifact').write_bytes(b'new')
        self.b.state.update_job(tested, 'succeeded', {'artifact_sha256': broker.digest_file(p / 'artifact')})
        job = self.b.state.new_job('deploy', 'mot-router', {'job_id': tested})
        row = self.b.state.job(job)
        with patch.dict(broker.COMPONENTS, {'mot-router': component}), patch.object(self.b, 'activate'), \
             patch.object(self.b, 'wait_health', return_value=True), patch.object(self.b, 'live_verify', return_value={'ok': False}):
            result = self.b.do_deploy(row)
        self.assertEqual(result['_state'], 'rolled_back')
        self.assertEqual(target.read_bytes(), b'old')
        with sqlite3.connect(dbpath) as db:
            self.assertEqual(db.execute('SELECT tokens FROM usage').fetchone()[0], 42)

    def test_database_backup_includes_wal_and_migration_retains_old_columns(self):
        src = broker.ROOT / 'fixture.db'
        db = sqlite3.connect(src)
        db.execute('PRAGMA journal_mode=WAL')
        db.execute('CREATE TABLE accounts(id INTEGER PRIMARY KEY, tokens INTEGER)')
        db.execute('INSERT INTO accounts VALUES(1,17)'); db.commit()
        dest = broker.ROOT / 'copy.db'
        broker.database_backup(src, dest)
        before = broker.database_fingerprint(dest)
        with sqlite3.connect(dest) as copy:
            copy.execute('ALTER TABLE accounts ADD COLUMN plan TEXT DEFAULT "unlimited"')
        broker.verify_database_fingerprint(dest, before)
        with sqlite3.connect(dest) as copy: copy.execute('UPDATE accounts SET tokens=0')
        with self.assertRaises(ValueError): broker.verify_database_fingerprint(dest, before)
        db.close()

    def test_build_is_isolated_and_bounded(self):
        job = 'a' * 32
        cwd = broker.ROOT / 'builds' / job / 'source'
        cwd.mkdir(parents=True)
        with patch.object(broker.subprocess, 'run') as command:
            command.return_value.returncode = 0
            self.b.build_run({'id': job, 'component': 'mot-router'}, cwd, ['go', 'test', './...'], 'tests')
        argv = command.call_args[0][0]
        for item in ('--property=MemoryMax=512M', '--property=CPUQuota=50%', '--property=PrivateNetwork=true', '--setenv=GOPROXY=off'):
            self.assertIn(item, argv)

    def test_dashboard_manual_pause_is_preserved_before_restart(self):
        path = broker.ROOT / 'bank.db'
        with sqlite3.connect(path) as db:
            db.execute('CREATE TABLE jobs(kind TEXT,state TEXT,error TEXT,created_at INTEGER)')
            db.execute('INSERT INTO jobs VALUES("scan","paused","",1)')
        with patch.dict(broker.COMPONENTS, {'mot-nodebank': {'db': str(path)}}), \
             patch.object(broker.subprocess, 'run') as command, \
             patch.object(broker.Path, 'read_text', return_value='{"auto_scan":false}'):
            self.b.preserve_bank_pause()
        self.assertEqual(command.call_args.args[0][-2:], ['scan', 'pause'])

    def test_changed_source_during_snapshot_rejected(self):
        src = broker.ROOT / 'source'; src.mkdir()
        (src / 'main.go').write_text('before')
        copy = broker.shutil.copytree
        def changing(a, b):
            result = copy(a, b)
            (src / 'main.go').write_text('after')
            return result
        with patch.object(broker.shutil, 'copytree', side_effect=changing):
            with self.assertRaises(ValueError): broker.snapshot(src, broker.ROOT / 'snapshot')


class MonitorTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.s = State(Path(self.tmp.name) / 'state.db')

    def tearDown(self): self.tmp.cleanup()

    def test_rate_and_policy_errors_never_rotate_or_probe(self):
        for kind in ('rate_limit', 'access_policy', 'authentication'):
            self.s.observe([{'kind': kind, 'component': 'mot-router'}], 1000)
            rows = self.s.observe([{'kind': kind, 'component': 'mot-router'}], 1060)
            with patch.object(monitor, 'call') as call:
                monitor.remediate(self.s, rows)
                call.assert_not_called()

    def test_ai_is_not_called_when_independent_provider_unavailable(self):
        self.s.observe([{'kind': 'transport', 'component': 'mot-router'}], 1000)
        rows = self.s.observe([{'kind': 'transport', 'component': 'mot-router'}], 1060)
        with patch.object(monitor, 'call', return_value={'activation': 'tested_procedures'}), patch.object(monitor, 'launch_ai') as ai:
            monitor.remediate(self.s, rows)
            ai.assert_not_called()
        self.assertEqual(self.s.incidents()[0]['state'], 'needs_attention')

    def test_manual_scan_and_sources_are_not_touched_by_recovery(self):
        self.s.observe([{'kind': 'node_sync', 'component': 'mot-nodebank-runtime'}], 1000)
        rows = self.s.observe([{'kind': 'node_sync', 'component': 'mot-nodebank-runtime'}], 1060)
        with patch.object(monitor, 'call', return_value={'activation': 'tested_procedures'}) as call:
            monitor.remediate(self.s, rows)
        self.assertEqual([c.args[0]['action'] for c in call.call_args_list], ['inspect'])

    def test_anthropic_stream_requires_stop_and_real_usage(self):
        events = [
            {'type': 'message_start', 'message': {'usage': {'input_tokens': 0, 'output_tokens': 0}}},
            {'type': 'content_block_delta', 'delta': {'type': 'text_delta', 'text': 'AGENT_OK'}},
            {'type': 'message_delta', 'usage': {'input_tokens': 17, 'output_tokens': 3}},
            {'type': 'message_delta', 'usage': {'input_tokens': 17, 'output_tokens': 3}},
            {'type': 'message_stop'}]
        raw = ''.join('data: ' + json.dumps(e) + '\n\n' for e in events).encode()
        result = anthropic_stream(raw)
        self.assertTrue(result['ok']); self.assertTrue(result['usage_reported'])
        self.assertEqual(result['usage'], {'input_tokens': 17, 'output_tokens': 3})
        truncated = ''.join('data: ' + json.dumps(e) + '\n' for e in events[:-1]).encode()
        self.assertFalse(anthropic_stream(truncated)['ok'])
        initial_only = b'data: {"type":"message_start","message":{"usage":{"input_tokens":0,"output_tokens":0}}}\n'
        self.assertFalse(anthropic_stream(initial_only)['usage_reported'])

    def test_idle_or_other_provider_success_does_not_clear_failure(self):
        path = Path(self.tmp.name) / 'router.db'
        connect = sqlite3.connect
        with connect(path) as db:
            db.execute('CREATE TABLE requests(id INTEGER PRIMARY KEY,ts INTEGER,status INTEGER,error TEXT,provider TEXT,model TEXT)')
            db.executemany('INSERT INTO requests VALUES(?,?,?,?,?,?)',
                           [(1,2000000,403,'Rate limit exceeded','codebuddy','deepseek-v4.1-flash'),
                            (2,2000000,403,'Rate limit exceeded','codebuddy','deepseek-v4.1-flash')])
        self.s.set_value('request_cursor', 0)
        def local_connect(name, **kwargs):
            if str(name).startswith('file:/opt/mot-router/data/mot-router.db'):
                return connect(path)
            return connect(name, **kwargs)
        with patch.object(monitor.sqlite3, 'connect', side_effect=local_connect):
            problems, available = monitor.failures(self.s, 2000)
        self.assertTrue(available); self.assertEqual(problems[0]['kind'], 'rate_limit')
        with connect(path) as db:
            db.execute('INSERT INTO requests VALUES(3,2060000,200,"","opencode-zen","muse-spark-1.3-contributor-free")')
        with patch.object(monitor.sqlite3, 'connect', side_effect=local_connect):
            problems, _ = monitor.failures(self.s, 2060)
            idle, _ = monitor.failures(self.s, 3000)
        self.assertEqual(problems[0]['evidence']['provider'], 'codebuddy')
        self.assertEqual(idle[0]['kind'], 'rate_limit')

    def test_nonstream_proof_uses_nullclaw_openai_json_contract(self):
        result = {'choices': [{'message': {'content': 'AGENT_OK'}}],
                  'usage': {'prompt_tokens': 31, 'completion_tokens': 4, 'total_tokens': 35}}
        with patch.object(broker, 'request', return_value=(200, json.dumps(result).encode())) as request:
            self.assertTrue(verify_nonstream('fixture-key')['ok'])
        self.assertTrue(request.call_args.args[0].endswith('/v1/chat/completions'))
        body = request.call_args.args[1]
        self.assertIs(body['stream'], False)
        self.assertEqual(body['tools'][0]['function']['name'], 'shell')
        with patch.object(broker, 'request', return_value=(200, b'data: {"text":"partial"}\n')):
            self.assertFalse(verify_nonstream('fixture-key')['ok'])


if __name__ == '__main__': unittest.main()
