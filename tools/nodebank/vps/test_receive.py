import json
import pathlib
import subprocess
import tempfile
import time
import unittest

RECEIVER = pathlib.Path(__file__).with_name("receive.py")


def sample():
    now = int(time.time())
    return {
        "schema_version": 1,
        "generated_at": now,
        "vantage": "laptop",
        "zen_verified": False,
        "nodes": [{
            "id": "fixture-1",
            "name": "fixture",
            "config": {"type": "http", "server": "proxy.example", "server_port": 8080},
            "latency_ms": 50,
            "exit_ip": "192.0.2.1",
            "checked_at": now,
            "ok": 3,
            "fail": 0,
            "score": 0.8,
        }],
    }


class ReceiverTest(unittest.TestCase):
    def run_receiver(self, output, value):
        return subprocess.run(
            ["python3", str(RECEIVER), "--output", str(output)],
            input=json.dumps(value).encode(), capture_output=True, check=False,
        )

    def test_accepts_candidate_and_sets_private_mode(self):
        with tempfile.TemporaryDirectory() as tmp:
            output = pathlib.Path(tmp) / "private" / "healthy-v1.json"
            result = self.run_receiver(output, sample())
            self.assertEqual(result.returncode, 0, result.stderr)
            ack = json.loads(result.stdout)
            self.assertEqual(ack["count"], 1)
            self.assertEqual(json.loads(output.read_text())["nodes"][0]["id"], "fixture-1")
            self.assertEqual(output.stat().st_mode & 0o777, 0o600)
            self.assertEqual(output.parent.stat().st_mode & 0o777, 0o700)

    def test_invalid_snapshot_does_not_replace_last_good_file(self):
        with tempfile.TemporaryDirectory() as tmp:
            output = pathlib.Path(tmp) / "healthy-v1.json"
            good = sample()
            self.assertEqual(self.run_receiver(output, good).returncode, 0)
            invalid = sample()
            invalid["nodes"][0]["config"]["type"] = "unknown"
            result = self.run_receiver(output, invalid)
            self.assertNotEqual(result.returncode, 0)
            self.assertEqual(json.loads(output.read_text()), good)

    def test_rejects_stale_or_duplicate_exits(self):
        with tempfile.TemporaryDirectory() as tmp:
            output = pathlib.Path(tmp) / "healthy-v1.json"
            stale = sample()
            stale["generated_at"] -= 7200
            self.assertNotEqual(self.run_receiver(output, stale).returncode, 0)
            duplicate = sample()
            duplicate["nodes"].append({**duplicate["nodes"][0], "id": "fixture-2"})
            self.assertNotEqual(self.run_receiver(output, duplicate).returncode, 0)
            self.assertFalse(output.exists())

    def test_five_thousand_candidates_boundary(self):
        value = sample()
        node = value['nodes'][0]
        value['nodes'] = [{**node, 'id': 'fixture-' + str(i),
                           'exit_ip': '198.18.%d.%d' % (i // 256, i % 256)}
                          for i in range(5000)]
        with tempfile.TemporaryDirectory() as tmp:
            output = pathlib.Path(tmp) / 'healthy-v1.json'
            self.assertEqual(self.run_receiver(output, value).returncode, 0)
            value['nodes'].append({**node, 'id': 'over-cap', 'exit_ip': '198.19.0.1'})
            self.assertNotEqual(self.run_receiver(output, value).returncode, 0)
            self.assertEqual(len(json.loads(output.read_text())['nodes']), 5000)


if __name__ == "__main__":
    unittest.main()
