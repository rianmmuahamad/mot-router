"""Small, durable recovery state machine. Contains no privileged operations."""
import hashlib
import json
import re
import sqlite3
import time
import uuid
from pathlib import Path

RETENTION = 14 * 86400
TERMINAL = ('succeeded', 'failed', 'rolled_back', 'interrupted', 'deferred')


def classify(status, detail=''):
    """Error bodies are only inspected locally, never forwarded to the model."""
    text = detail.lower()
    if status == 429 or any(s in text for s in ('freeusagelimiterror', 'rate limit', 'quota exceeded')):
        return 'rate_limit'
    if status == 401 or any(s in text for s in ('invalid api key', 'invalid token', 'unauthorized')):
        return 'authentication'
    if 'freetiererror' in text or 'only be used from within opencode' in text:
        return 'access_policy'
    if any(s in text for s in ('first message is not system', 'no tool output', 'invalid_request',
                               'invalid request', 'tool name', 'non-stream chat', 'schema')):
        return 'compatibility'
    if status == 403:
        return 'access_policy'
    if status in (502, 503, 504) or any(s in text for s in ('malformed http', 'connection broken', 'timeout', 'eof')):
        return 'transport'
    return 'inference_failure' if status >= 400 else 'ok'


def digest_file(path):
    h = hashlib.sha256()
    with open(path, 'rb') as f:
        for block in iter(lambda: f.read(1024 * 1024), b''):
            h.update(block)
    return h.hexdigest()


def manifest(root, exclude=()):
    """Reject links, oversized files, and special files before source snapshotting."""
    root = Path(root)
    result = {}
    total = 0
    for p in sorted(root.rglob('*')):
        name = str(p.relative_to(root))
        if any(name == prefix or name.startswith(prefix + '/') for prefix in exclude):
            continue
        if p.is_symlink():
            raise ValueError('Source links are forbidden')
        if p.is_dir():
            continue
        if not p.is_file():
            raise ValueError('Source contains a special file')
        size = p.stat().st_size
        total += size
        if size > 8 * 1024**2 or total > 128 * 1024**2 or len(result) >= 10000:
            raise ValueError('Source exceeds snapshot limits')
        result[str(p.relative_to(root))] = digest_file(p)
    return result


def manifest_hash(entries):
    return hashlib.sha256(json.dumps(entries, sort_keys=True, separators=(',', ':')).encode()).hexdigest()


def valid_id(value):
    if not isinstance(value, str) or not re.fullmatch(r'[0-9a-f]{32}', value):
        raise ValueError('Invalid job ID')
    return value


class State:
    def __init__(self, path):
        self.path = Path(path)
        self.path.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
        with self.connect() as db:
            db.executescript('''
                CREATE TABLE IF NOT EXISTS jobs (
                    id TEXT PRIMARY KEY, action TEXT NOT NULL, component TEXT NOT NULL,
                    state TEXT NOT NULL, created INTEGER NOT NULL, updated INTEGER NOT NULL,
                    data TEXT NOT NULL, result TEXT NOT NULL DEFAULT '{}');
                CREATE TABLE IF NOT EXISTS incidents (
                    fingerprint TEXT PRIMARY KEY, kind TEXT NOT NULL, component TEXT NOT NULL,
                    state TEXT NOT NULL, first_seen INTEGER NOT NULL, last_seen INTEGER NOT NULL,
                    hits INTEGER NOT NULL DEFAULT 1, clear_hits INTEGER NOT NULL DEFAULT 0,
                    notified INTEGER NOT NULL DEFAULT 0, last_action INTEGER NOT NULL DEFAULT 0,
                    attempts INTEGER NOT NULL DEFAULT 0, evidence TEXT NOT NULL);
                CREATE TABLE IF NOT EXISTS samples (key TEXT PRIMARY KEY, value TEXT NOT NULL);
                CREATE TABLE IF NOT EXISTS notifications (
                    id INTEGER PRIMARY KEY, dedup TEXT UNIQUE NOT NULL,
                    text TEXT NOT NULL, delivered INTEGER NOT NULL DEFAULT 0);
            ''')
            if 'patch' not in {r[1] for r in db.execute('PRAGMA table_info(jobs)')}:
                db.execute('ALTER TABLE jobs ADD COLUMN patch INTEGER NOT NULL DEFAULT 0')
        self.path.chmod(0o600)

    def connect(self):
        db = sqlite3.connect(self.path, timeout=10)
        db.row_factory = sqlite3.Row
        db.execute('PRAGMA journal_mode=WAL')
        db.execute('PRAGMA busy_timeout=10000')
        return db

    def value(self, key, default=None):
        with self.connect() as db:
            row = db.execute('SELECT value FROM samples WHERE key=?', (key,)).fetchone()
        return json.loads(row[0]) if row else default

    def set_value(self, key, value):
        with self.connect() as db:
            db.execute('INSERT OR REPLACE INTO samples VALUES (?,?)', (key, json.dumps(value)))

    def new_job(self, action, component, data, exclusive=True, patch=False):
        now = int(time.time())
        with self.connect() as db:
            db.execute('BEGIN IMMEDIATE')
            if exclusive and db.execute("SELECT 1 FROM jobs WHERE state IN ('queued','running')").fetchone():
                raise ValueError('Another recovery job is active')
            if patch and db.execute("SELECT COUNT(*) FROM jobs WHERE patch=1 AND created>?", (now - 3600,)).fetchone()[0] >= 3:
                raise ValueError('Three patch candidates already attempted this hour')
            job = uuid.uuid4().hex
            db.execute('INSERT INTO jobs(id,action,component,state,created,updated,data,patch) VALUES(?,?,?,?,?,?,?,?)',
                       (job, action, component, 'queued', now, now, json.dumps(data), int(patch)))
        return job

    def update_job(self, job, state, result):
        valid_id(job)
        with self.connect() as db:
            db.execute('UPDATE jobs SET state=?,updated=?,result=? WHERE id=?',
                       (state, int(time.time()), json.dumps(result), job))

    def job(self, job):
        valid_id(job)
        with self.connect() as db:
            row = db.execute('SELECT * FROM jobs WHERE id=?', (job,)).fetchone()
        if not row:
            raise ValueError('Unknown job')
        result = dict(row)
        for key in ('data', 'result'):
            result[key] = json.loads(result[key])
        return result

    def jobs(self):
        with self.connect() as db:
            return [self.job(r[0]) for r in db.execute('SELECT id FROM jobs ORDER BY created DESC LIMIT 20')]

    def notify(self, dedup, text):
        with self.connect() as db:
            db.execute('INSERT OR IGNORE INTO notifications(dedup,text) VALUES(?,?)', (dedup, text[:2000]))

    def observe(self, problems, now=None):
        """Two consecutive observations confirm; two clean ones recover. Quiet otherwise."""
        now = int(time.time()) if now is None else now
        seen = set()
        # Monitor runs once a minute; duplicate observations in the same tick cannot confirm.
        previous_tick = self.value('observation_tick', 0)
        if now - previous_tick < 30:
            return self.incidents()
        self.set_value('observation_tick', now)
        with self.connect() as db:
            db.execute('BEGIN IMMEDIATE')
            for p in problems:
                fp = p['kind'] + ':' + p['component'] + (':' + p['scope'] if p.get('scope') else '')
                seen.add(fp)
                evidence = json.dumps(p.get('evidence', {}))
                row = db.execute('SELECT * FROM incidents WHERE fingerprint=?', (fp,)).fetchone()
                if row and row['state'] != 'resolved':
                    hits = row['hits'] + 1 if not row['clear_hits'] else 1
                    db.execute('UPDATE incidents SET last_seen=?,hits=?,clear_hits=0,evidence=? WHERE fingerprint=?',
                               (now, hits, evidence, fp))
                else:
                    db.execute('INSERT OR REPLACE INTO incidents VALUES(?,?,?,?,?,?,1,0,0,0,0,?)',
                               (fp, p['kind'], p['component'], 'suspected', now, now, evidence))
                    hits = 1
                if hits >= 2:
                    db.execute("UPDATE incidents SET state=CASE WHEN state='suspected' THEN 'confirmed' ELSE state END WHERE fingerprint=?", (fp,))
                    current = db.execute('SELECT * FROM incidents WHERE fingerprint=?', (fp,)).fetchone()
                    if not current['notified']:
                        db.execute('INSERT OR IGNORE INTO notifications(dedup,text) VALUES(?,?)',
                                   ('start:%s:%s' % (fp, current['first_seen']),
                                    'Arbyte: insiden terkonfirmasi — %s pada %s.' % (p['kind'], p['component'])))
                        db.execute('UPDATE incidents SET notified=1 WHERE fingerprint=?', (fp,))
            for row in db.execute("SELECT * FROM incidents WHERE state!='resolved'").fetchall():
                if row['fingerprint'] in seen:
                    continue
                clear = row['clear_hits'] + 1
                state = 'resolved' if clear >= 2 else row['state']
                db.execute('UPDATE incidents SET clear_hits=?,state=? WHERE fingerprint=?', (clear, state, row['fingerprint']))
                if state == 'resolved' and row['notified']:
                    db.execute('INSERT OR IGNORE INTO notifications(dedup,text) VALUES(?,?)',
                               ('end:%s:%s' % (row['fingerprint'], row['first_seen']),
                                'Arbyte: kondisi pulih — %s pada %s.' % (row['kind'], row['component'])))
        return self.incidents()

    def incidents(self):
        with self.connect() as db:
            rows = [dict(r) for r in db.execute("SELECT * FROM incidents WHERE state!='resolved' ORDER BY first_seen LIMIT 50")]
        for row in rows:
            row['evidence'] = json.loads(row['evidence'])
        return rows

    def claim_incident(self, fp, cooldown=900, now=None):
        now = int(time.time()) if now is None else now
        with self.connect() as db:
            db.execute('BEGIN IMMEDIATE')
            row = db.execute('SELECT * FROM incidents WHERE fingerprint=?', (fp,)).fetchone()
            if not row or row['hits'] < 2 or row['attempts'] >= 3 or now - row['last_action'] < cooldown:
                return False
            db.execute("UPDATE incidents SET last_action=?,attempts=attempts+1,state='recovering' WHERE fingerprint=?", (now, fp))
        return True

    def mark_attention(self, incident, reason):
        with self.connect() as db:
            db.execute("UPDATE incidents SET state='needs_attention' WHERE fingerprint=?", (incident['fingerprint'],))
        self.notify('attention:%s:%s' % (incident['fingerprint'], incident['first_seen']),
                    'Arbyte: perlu tindakan — %s pada %s; %s.' % (incident['kind'], incident['component'], reason))

    def prune(self):
        cutoff = int(time.time()) - RETENTION
        with self.connect() as db:
            db.execute("DELETE FROM jobs WHERE updated<? AND state NOT IN ('running','queued')", (cutoff,))
            db.execute("DELETE FROM incidents WHERE last_seen<? AND state='resolved'", (cutoff,))
            db.execute('DELETE FROM notifications WHERE delivered=1 AND id NOT IN (SELECT id FROM notifications ORDER BY id DESC LIMIT 500)')
            db.execute('PRAGMA wal_checkpoint(PASSIVE)')
