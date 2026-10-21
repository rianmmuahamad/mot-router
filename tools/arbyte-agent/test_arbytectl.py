import importlib.util
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location('ops', Path(__file__).with_name('arbytectl.py'))
ops = importlib.util.module_from_spec(spec)
spec.loader.exec_module(ops)


class OperationsTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.home = patch.object(ops, 'HOME', Path(self.tmp.name))
        self.home.start()
        self.ram = patch.object(ops, 'memory', return_value={'available_bytes': 1500 * 1024**2})
        self.ram.start()

    def tearDown(self):
        self.ram.stop(); self.home.stop(); self.tmp.cleanup()

    def test_manual_pause_not_resumed(self):
        snapshot = {'errors': [], 'bank': {'paused': True, 'pause_reason': 'Paused by user'}}
        with patch.object(ops, 'status', return_value=snapshot), patch.object(ops, 'refresh'), patch.object(ops, 'scan') as scan:
            ops.maintain()
            scan.assert_not_called()
        self.assertFalse(ops.load(ops.HOME / 'policy.json', {})['auto_scan'])

    def test_network_recovery_has_cooldown(self):
        snapshot = {'errors': [], 'bank': {'paused': True, 'pause_reason': 'Baseline HTTPS failed'}}
        with patch.object(ops, 'status', return_value=snapshot), patch.object(ops, 'refresh'), patch.object(ops, 'scan') as scan:
            ops.maintain(); ops.maintain()
            self.assertEqual(scan.call_count, 1)

    def test_dashboard_resume_reenables_auto_scan(self):
        ops.save(ops.HOME / 'policy.json', {'auto_scan': False})
        snapshot = {'errors': [], 'bank': {'paused': False, 'background': True}}
        with patch.object(ops, 'status', return_value=snapshot), patch.object(ops, 'refresh'):
            ops.maintain()
        self.assertTrue(ops.load(ops.HOME / 'policy.json', {})['auto_scan'])

    def test_only_one_enabled_due_source(self):
        sources = {'sources': [
            {'id': 'disabled', 'enabled': False},
            {'id': 'b', 'enabled': True, 'last_fetch': 0},
            {'id': 'a', 'enabled': True, 'last_fetch': 0}]}
        calls = []
        def api(base, path, body=None):
            calls.append((path, body))
            return sources if path == '/api/sources' else {'job_id': 'test'}
        with patch.object(ops, 'jobs', return_value=[]), patch.object(ops, 'api', side_effect=api):
            result = ops.refresh()
        self.assertEqual(result['source_id'], 'a')
        self.assertEqual([p for p, b in calls if b is not None], ['/api/sources/a/import'])

    def test_import_busy_is_not_duplicated(self):
        with patch.object(ops, 'jobs', return_value=[{'kind': 'import', 'state': 'running'}]), patch.object(ops, 'api') as api:
            self.assertIn('deferred', ops.refresh())
            api.assert_not_called()

    def test_low_memory_prevents_scan_and_import(self):
        with patch.object(ops, 'memory', return_value={'available_bytes': 700 * 1024**2}), patch.object(ops, 'api') as api:
            self.assertIn('deferred', ops.scan('resume'))
            self.assertIn('deferred', ops.refresh())
            api.assert_not_called()

    def test_source_failure_backoff_persists(self):
        sources = {'sources': [{'id': 'a', 'enabled': True, 'last_fetch': 0}]}
        def api(base, path, body=None):
            if path == '/api/sources': return sources
            raise RuntimeError('Local API HTTP 400')
        with patch.object(ops, 'jobs', return_value=[]), patch.object(ops, 'api', side_effect=api):
            with self.assertRaises(RuntimeError): ops.refresh()
            self.assertTrue(ops.refresh()['up_to_date'])

    def test_source_toggle_only_updates_enabled_and_hides_url(self):
        source = {'id': 'existing', 'name': 'Private feed', 'enabled': False,
                  'url': 'https://source.example/sub?token=SECRET'}
        calls = []
        def api(base, path, body=None):
            calls.append((path, body))
            if path == '/api/sources': return {'sources': [source]}
            return {'enabled': body['enabled']}
        with patch.object(ops, 'api', side_effect=api):
            for enabled in (True, False):
                result = ops.source_enabled('existing', enabled)
                self.assertEqual(result['enabled'], enabled)
                self.assertFalse(result['import_started'])
                self.assertNotIn('SECRET', str(result))
        self.assertEqual([body for path, body in calls if body is not None],
                         [{'enabled': True}, {'enabled': False}])

    def test_unknown_source_cannot_be_created_by_toggle(self):
        with patch.object(ops, 'api', return_value={'sources': []}) as api:
            with self.assertRaises(RuntimeError): ops.source_enabled('*', True)
            api.assert_called_once_with(ops.BANK, '/api/sources')


if __name__ == '__main__':
    unittest.main()
