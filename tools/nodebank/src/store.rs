use anyhow::{bail, Context, Result};
use rusqlite::{params, Connection, OptionalExtension};
use serde::{Deserialize, Serialize};
use serde_json::{json, Value};
use std::{
    collections::HashSet,
    fs,
    io::Write,
    os::unix::fs::PermissionsExt,
    path::{Path, PathBuf},
    sync::Mutex,
    time::{SystemTime, UNIX_EPOCH},
};

pub const MAX_CANDIDATES: usize = 5000;

pub fn now() -> i64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .unwrap_or_default()
        .as_secs() as i64
}

#[derive(Clone, Debug)]
pub struct Candidate {
    pub id: String,
    pub label: String,
    pub protocol: String,
    pub server: String,
    pub port: u16,
    pub config: Value,
    pub error: Option<String>,
}
#[derive(Clone, Debug, Serialize, Deserialize)]
pub struct Source {
    pub id: String,
    pub name: String,
    pub url: String,
    pub enabled: bool,
    pub interval_secs: i64,
    pub last_fetch: i64,
}
#[derive(Clone, Debug, Serialize, Deserialize)]
pub struct Job {
    pub id: String,
    pub kind: String,
    pub state: String,
    pub source_id: String,
    pub path: String,
    pub format: String,
    pub processed: i64,
    pub added: i64,
    pub ok: i64,
    pub failed: i64,
    pub unsupported: i64,
    pub total: i64,
    pub error: String,
    pub created_at: i64,
    pub updated_at: i64,
}
#[derive(Clone, Debug, Serialize, Deserialize)]
pub struct ProbeNode {
    pub id: String,
    pub config: Value,
    pub server: String,
    pub port: u16,
    #[serde(default)]
    pub resolved_ip: String,
}
#[derive(Clone, Debug, Serialize, Deserialize)]
pub struct ProbeResult {
    pub id: String,
    pub status: String,
    #[serde(default)]
    pub latency_ms: f64,
    #[serde(default)]
    pub exit_ip: String,
    #[serde(default)]
    pub error: String,
}
pub struct Db {
    conn: Mutex<Connection>,
    pub dir: PathBuf,
}

impl Db {
    pub fn open(dir: &Path) -> Result<Self> {
        fs::create_dir_all(dir)?;
        fs::set_permissions(dir, fs::Permissions::from_mode(0o700))?;
        let path = dir.join("bank.db");
        let conn = Connection::open(&path)?;
        conn.busy_timeout(std::time::Duration::from_secs(5))?;
        conn.execute_batch("PRAGMA journal_mode=WAL; PRAGMA synchronous=NORMAL; PRAGMA foreign_keys=ON;
            PRAGMA cache_size=-16384; PRAGMA mmap_size=0; PRAGMA temp_store=FILE; PRAGMA wal_autocheckpoint=1000;
            CREATE TABLE IF NOT EXISTS counters(status TEXT PRIMARY KEY,n INTEGER NOT NULL DEFAULT 0);
            INSERT OR IGNORE INTO counters VALUES('pending',0),('healthy',0),('failed',0),('unsupported',0);
            CREATE TABLE IF NOT EXISTS sources(id TEXT PRIMARY KEY,name TEXT NOT NULL,url TEXT NOT NULL,enabled INTEGER NOT NULL DEFAULT 0,interval_secs INTEGER NOT NULL DEFAULT 3600,last_fetch INTEGER NOT NULL DEFAULT 0);
            CREATE TABLE IF NOT EXISTS nodes(id TEXT PRIMARY KEY,label TEXT NOT NULL,protocol TEXT NOT NULL,server TEXT NOT NULL,port INTEGER NOT NULL,config TEXT NOT NULL,status TEXT NOT NULL DEFAULT 'pending',error TEXT NOT NULL DEFAULT '',ok INTEGER NOT NULL DEFAULT 0,fail INTEGER NOT NULL DEFAULT 0,latency_ms REAL NOT NULL DEFAULT 0,exit_ip TEXT NOT NULL DEFAULT '',checked_at INTEGER NOT NULL DEFAULT 0,next_check INTEGER NOT NULL DEFAULT 0,lease_until INTEGER NOT NULL DEFAULT 0,score REAL NOT NULL DEFAULT 0,created_at INTEGER NOT NULL);
            DROP INDEX IF EXISTS nodes_due; DROP INDEX IF EXISTS nodes_schedule; DROP INDEX IF EXISTS nodes_rank; DROP INDEX IF EXISTS membership_node;
            CREATE INDEX IF NOT EXISTS nodes_schedule_v2 ON nodes(next_check) WHERE status IN ('pending','healthy','failed');
            CREATE INDEX IF NOT EXISTS nodes_rank_v2 ON nodes(score DESC,latency_ms,checked_at DESC,id) WHERE status='healthy';
            CREATE TABLE IF NOT EXISTS memberships(source_id TEXT NOT NULL,node_id TEXT NOT NULL,PRIMARY KEY(source_id,node_id)) WITHOUT ROWID;
            CREATE TABLE IF NOT EXISTS jobs(id TEXT PRIMARY KEY,kind TEXT NOT NULL,state TEXT NOT NULL,source_id TEXT NOT NULL DEFAULT '',path TEXT NOT NULL DEFAULT '',format TEXT NOT NULL DEFAULT 'auto',processed INTEGER NOT NULL DEFAULT 0,added INTEGER NOT NULL DEFAULT 0,ok INTEGER NOT NULL DEFAULT 0,failed INTEGER NOT NULL DEFAULT 0,unsupported INTEGER NOT NULL DEFAULT 0,total INTEGER NOT NULL DEFAULT 0,error TEXT NOT NULL DEFAULT '',created_at INTEGER NOT NULL,updated_at INTEGER NOT NULL);
            CREATE INDEX IF NOT EXISTS jobs_state ON jobs(kind,state,created_at);
            CREATE TRIGGER IF NOT EXISTS node_added AFTER INSERT ON nodes BEGIN UPDATE counters SET n=n+1 WHERE status=NEW.status; END;
            CREATE TRIGGER IF NOT EXISTS node_status AFTER UPDATE OF status ON nodes WHEN OLD.status<>NEW.status BEGIN UPDATE counters SET n=n-1 WHERE status=OLD.status; UPDATE counters SET n=n+1 WHERE status=NEW.status; END;
            CREATE TRIGGER IF NOT EXISTS node_removed AFTER DELETE ON nodes BEGIN UPDATE counters SET n=n-1 WHERE status=OLD.status; END;
            PRAGMA user_version=2;")?;
        fs::set_permissions(&path, fs::Permissions::from_mode(0o600))?;
        for suffix in ["bank.db-wal", "bank.db-shm"] {
            let p = dir.join(suffix);
            if p.exists() {
                fs::set_permissions(p, fs::Permissions::from_mode(0o600))?;
            }
        }
        let db = Self {
            conn: Mutex::new(conn),
            dir: dir.to_owned(),
        };
        db.seed_sources()?;
        Ok(db)
    }
    pub fn recover(&self) -> Result<()> {
        self.conn.lock().unwrap().execute_batch("UPDATE nodes SET lease_until=0 WHERE lease_until<>0; UPDATE jobs SET state='paused',error='Interrupted; resume to continue' WHERE state IN ('running','downloading') AND kind<>'push'; UPDATE jobs SET state='failed',error='Interrupted before VPS transfer completed' WHERE state IN ('running','queued') AND kind='push';")?;
        Ok(())
    }
    fn seed_sources(&self) -> Result<()> {
        let conn = self.conn.lock().unwrap();
        let mut seeds=vec![
            ("RADIKAL".to_owned(),"https://raw.githubusercontent.com/0xRadikal/Free-v2ray-Configs/main/all/clash.yaml".to_owned(),900),
            ("DELTA".to_owned(),"https://raw.githubusercontent.com/Delta-Kronecker/V2ray-Config/main/config/clash.yaml".to_owned(),3600),
            ("BARABAMA".to_owned(),"https://raw.githubusercontent.com/Barabama/FreeNodes/feat/ai-crawler-v2/nodes/clashnode.yaml".to_owned(),86400),
            ("ANAER".to_owned(),"https://raw.githubusercontent.com/anaer/Sub/main/clash.yaml".to_owned(),3600),
            ("KORT".to_owned(),"https://raw.githubusercontent.com/kort0881/vpn-checker-backend/main/checked/RU_Best/ru_white_all_BLACK.txt".to_owned(),3600),
            ("MORPHEUS-all".to_owned(),"https://raw.githubusercontent.com/morpheusadam/v2ray-config/main/subs/bundles/all.txt".to_owned(),3600),
            ("MORPHEUS-best".to_owned(),"https://raw.githubusercontent.com/morpheusadam/v2ray-config/main/subs/bundles/best.txt".to_owned(),3600),
            ("MORPHEUS-vless".to_owned(),"https://raw.githubusercontent.com/morpheusadam/v2ray-config/main/subs/bundles/vless.txt".to_owned(),3600),
            ("MORPHEUS-reality".to_owned(),"https://raw.githubusercontent.com/morpheusadam/v2ray-config/main/subs/bundles/reality.txt".to_owned(),3600),
        ];
        for (name,url) in [
            ("DukeMehdi","https://raw.githubusercontent.com/DukeMehdi/All-DukeMehdi-Configs/main/All-DukeMehdi-Configs.txt"),
            ("Hidashimora","https://raw.githubusercontent.com/Hidashimora/free-vpn-anti-rkn/main/configs/1.14.txt"),
            ("VestraNet","https://raw.githubusercontent.com/MustafaBaqer/VestraNet-Nodes/main/subscriptions/mix-normal.txt"),
            ("iboxz","https://raw.githubusercontent.com/iboxz/free-v2ray-collector/main/all.txt"),
            ("vsvavan2","https://raw.githubusercontent.com/vsvavan2/vpn-config-rkn/main/output/all_working_keys.txt"),
            ("haha-aggregator","https://raw.githubusercontent.com/hahayoutookind/vpn-configs-aggregator/main/all-vpn.txt"),
            ("slxkware-white","https://raw.githubusercontent.com/slxkware/Vless-list/main/White%20Vless.txt"),
            ("slxkware-black","https://raw.githubusercontent.com/slxkware/Vless-list/main/Black%20Vless.txt"),
            ("A-K-6","https://raw.githubusercontent.com/A-K-6/v2ray_scrapper_repo/main/subscription.txt"),
            ("jafarm83","https://raw.githubusercontent.com/jafarm83/ConfigV2Ray/main/all.txt"),
        ]{seeds.push((name.to_owned(),url.to_owned(),86400));}
        for n in 1..=26 {
            seeds.push((format!("NIKITA-{n}"),format!("https://raw.githubusercontent.com/nikita29a/FreeProxyList/refs/heads/main/mirror/{n}.txt"),86400));
        }
        for n in [9, 15, 18, 23] {
            seeds.push((format!("BATCH-starwin-{n}"),format!("https://raw.githubusercontent.com/aplodismenty/starwin_conf/main/githubmirror/default/{n}.txt"),86400));
        }
        for country in ["JP", "US", "SG", "NL", "DE"] {
            seeds.push((format!("AU-{country}"),format!("https://raw.githubusercontent.com/Au1rxx/free-vpn-subscriptions/main/output/by-country/clash-{country}.yaml"),86400));
        }
        for (name, url, interval) in seeds {
            conn.execute(
                "INSERT OR IGNORE INTO sources(id,name,url,interval_secs) VALUES(?1,?1,?2,?3)",
                params![name, url, interval],
            )?;
        }
        Ok(())
    }
    pub fn sources(&self) -> Result<Vec<Source>> {
        let conn = self.conn.lock().unwrap();
        let mut q = conn.prepare(
            "SELECT id,name,url,enabled,interval_secs,last_fetch FROM sources ORDER BY name",
        )?;
        let out = q
            .query_map([], |r| {
                Ok(Source {
                    id: r.get(0)?,
                    name: r.get(1)?,
                    url: r.get(2)?,
                    enabled: r.get(3)?,
                    interval_secs: r.get(4)?,
                    last_fetch: r.get(5)?,
                })
            })?
            .collect::<rusqlite::Result<Vec<_>>>()?;
        Ok(out)
    }
    pub fn save_source(&self, s: &Source) -> Result<()> {
        if s.id.len() > 128 || s.url.len() > 8192 {
            bail!("Source ID or URL exceeds limits")
        }
        if s.name.is_empty() || s.name.len() > 128 {
            bail!("Source name must contain 1–128 characters")
        }
        let u = url::Url::parse(&s.url).context("Invalid subscription URL")?;
        if !["https", "http"].contains(&u.scheme())
            || !u.username().is_empty()
            || u.password().is_some()
        {
            bail!("Use an HTTP(S) URL without embedded credentials")
        }
        self.conn.lock().unwrap().execute("INSERT INTO sources(id,name,url,enabled,interval_secs) VALUES(?1,?2,?3,?4,?5) ON CONFLICT(id) DO UPDATE SET name=excluded.name,url=excluded.url,enabled=excluded.enabled,interval_secs=excluded.interval_secs",params![s.id,s.name,s.url,s.enabled,s.interval_secs.clamp(900,86400*7)])?;
        Ok(())
    }
    pub fn set_source_enabled(&self, id: &str, enabled: bool) -> Result<()> {
        let changed = self.conn.lock().unwrap().execute(
            "UPDATE sources SET enabled=?2 WHERE id=?1",
            params![id, enabled],
        )?;
        if changed != 1 {
            bail!("Unknown source")
        }
        Ok(())
    }
    pub fn new_job(
        &self,
        kind: &str,
        source: &str,
        path: &Path,
        format: &str,
        total: i64,
    ) -> Result<Job> {
        let id = uuid::Uuid::new_v4().to_string();
        let t = now();
        let conn = self.conn.lock().unwrap();
        let queued: i64 = conn.query_row(
            "SELECT COUNT(*) FROM jobs WHERE state IN ('queued','running','paused','downloading')",
            [],
            |r| r.get(0),
        )?;
        if queued >= 8 {
            bail!("Maximum eight unfinished jobs")
        };
        if kind == "scan" {
            let running:i64=conn.query_row("SELECT COUNT(*) FROM jobs WHERE kind='scan' AND state IN ('queued','running','paused')",[],|r|r.get(0))?;
            if running > 0 {
                bail!("Resume or finish the existing scan first")
            }
        }
        conn.execute("INSERT INTO jobs(id,kind,state,source_id,path,format,total,created_at,updated_at) VALUES(?1,?2,'queued',?3,?4,?5,?6,?7,?7)",params![id,kind,source,path.to_string_lossy(),format,total,t])?;
        drop(conn);
        self.job(&id)?.context("Job was not stored")
    }
    fn map_job(r: &rusqlite::Row<'_>) -> rusqlite::Result<Job> {
        Ok(Job {
            id: r.get(0)?,
            kind: r.get(1)?,
            state: r.get(2)?,
            source_id: r.get(3)?,
            path: r.get(4)?,
            format: r.get(5)?,
            processed: r.get(6)?,
            added: r.get(7)?,
            ok: r.get(8)?,
            failed: r.get(9)?,
            unsupported: r.get(10)?,
            total: r.get(11)?,
            error: r.get(12)?,
            created_at: r.get(13)?,
            updated_at: r.get(14)?,
        })
    }
    pub fn job(&self, id: &str) -> Result<Option<Job>> {
        Ok(self
            .conn
            .lock()
            .unwrap()
            .query_row("SELECT * FROM jobs WHERE id=?1", [id], Self::map_job)
            .optional()?)
    }
    pub fn jobs(&self) -> Result<Vec<Job>> {
        let conn = self.conn.lock().unwrap();
        let mut q = conn.prepare("SELECT * FROM jobs ORDER BY created_at DESC LIMIT 100")?;
        let out = q
            .query_map([], Self::map_job)?
            .collect::<rusqlite::Result<Vec<_>>>()?;
        Ok(out)
    }
    pub fn claim_import(&self) -> Result<Option<Job>> {
        let conn = self.conn.lock().unwrap();
        let job = conn.query_row("SELECT * FROM jobs WHERE kind='import' AND state='queued' ORDER BY created_at LIMIT 1", [], Self::map_job).optional()?;
        if let Some(ref job) = job {
            conn.execute(
                "UPDATE jobs SET state='running',updated_at=?2 WHERE id=?1 AND state='queued'",
                params![job.id, now()],
            )?;
        }
        Ok(job)
    }
    pub fn state_if(&self, id: &str, expected: &str, next: &str, error: &str) -> Result<bool> {
        let changed = self.conn.lock().unwrap().execute(
            "UPDATE jobs SET state=?3,error=?4,updated_at=?5 WHERE id=?1 AND state=?2",
            params![id, expected, next, error, now()],
        )?;
        if changed > 0 && matches!(next, "completed" | "failed" | "cancelled") {
            self.cleanup()?;
        }
        Ok(changed > 0)
    }
    pub fn active_scan(&self) -> Result<Option<Job>> {
        Ok(self.conn.lock().unwrap().query_row("SELECT * FROM jobs WHERE kind='scan' AND state='running' ORDER BY created_at LIMIT 1",[],Self::map_job).optional()?)
    }
    pub fn state(&self, id: &str, state: &str, error: &str) -> Result<()> {
        self.conn.lock().unwrap().execute(
            "UPDATE jobs SET state=?2,error=?3,updated_at=?4 WHERE id=?1",
            params![id, state, error, now()],
        )?;
        if state == "cancelled" {
            if let Some(job) = self.job(id)? {
                self.remove_spool(&job.path);
            }
        }
        if matches!(state, "completed" | "failed" | "cancelled") {
            self.cleanup()?;
        }
        Ok(())
    }
    pub fn import_batch(
        &self,
        job: &str,
        source: &str,
        items: &[Candidate],
        position: i64,
    ) -> Result<i64> {
        let mut conn = self.conn.lock().unwrap();
        let tx = conn.transaction()?;
        let mut added = 0;
        let mut unsupported = 0;
        {
            let mut insert=tx.prepare_cached("INSERT OR IGNORE INTO nodes(id,label,protocol,server,port,config,status,error,created_at) VALUES(?1,?2,?3,?4,?5,?6,?7,?8,?9)")?;
            let mut membership = tx.prepare_cached(
                "INSERT OR IGNORE INTO memberships(source_id,node_id) VALUES(?1,?2)",
            )?;
            for c in items {
                let status = if c.error.is_some() {
                    "unsupported"
                } else {
                    "pending"
                };
                unsupported += i64::from(c.error.is_some());
                added += insert.execute(params![
                    c.id,
                    c.label,
                    c.protocol,
                    c.server,
                    c.port,
                    serde_json::to_string(&c.config)?,
                    status,
                    c.error.as_deref().unwrap_or(""),
                    now()
                ])? as i64;
                membership.execute(params![source, c.id])?;
            }
        }
        tx.execute("UPDATE jobs SET processed=?2,added=added+?3,unsupported=unsupported+?4,updated_at=?5 WHERE id=?1",params![job,position,added,unsupported,now()])?;
        tx.commit()?;
        Ok(added)
    }
    pub fn finish_import(&self, job: &Job) -> Result<()> {
        let conn = self.conn.lock().unwrap();
        conn.execute(
            "UPDATE jobs SET state='completed',total=processed,updated_at=?2 WHERE id=?1",
            params![job.id, now()],
        )?;
        conn.execute(
            "UPDATE sources SET last_fetch=?2 WHERE id=?1",
            params![job.source_id, now()],
        )?;
        conn.execute_batch("PRAGMA wal_checkpoint(PASSIVE);")?;
        Ok(())
    }
    pub fn counts(&self) -> Result<Value> {
        let conn = self.conn.lock().unwrap();
        let mut q = conn.prepare("SELECT status,n FROM counters")?;
        let mut out = serde_json::Map::new();
        let mut total = 0_i64;
        for r in q.query_map([], |r| Ok((r.get::<_, String>(0)?, r.get::<_, i64>(1)?)))? {
            let (k, v) = r?;
            total += v;
            out.insert(k, json!(v));
        }
        out.insert("total".into(), json!(total));
        Ok(Value::Object(out))
    }
    pub fn due_total(&self, background: bool) -> Result<i64> {
        Ok(self.conn.lock().unwrap().query_row(
            "SELECT COUNT(*) FROM nodes WHERE status IN ('pending','healthy','failed') AND next_check<=?1",
            [if background { now() } else { now() }],
            |r| r.get(0),
        )?)
    }
    pub fn lease(&self, limit: usize) -> Result<Vec<ProbeNode>> {
        let mut conn = self.conn.lock().unwrap();
        let tx = conn.transaction()?;
        let mut out = Vec::new();
        {
            let mut q=tx.prepare("SELECT id,config,server,port FROM nodes WHERE status IN ('pending','healthy','failed') AND next_check<=?1 AND lease_until<=?1 ORDER BY next_check,rowid LIMIT ?2")?;
            let rows = q.query_map(params![now(), limit.min(128) as i64], |r| {
                Ok((
                    r.get::<_, String>(0)?,
                    r.get::<_, String>(1)?,
                    r.get::<_, String>(2)?,
                    r.get::<_, u16>(3)?,
                ))
            })?;
            for r in rows {
                let (id, c, server, port) = r?;
                out.push(ProbeNode {
                    id,
                    config: serde_json::from_str(&c)?,
                    server,
                    port,
                    resolved_ip: String::new(),
                });
            }
        }
        for n in &out {
            tx.execute(
                "UPDATE nodes SET lease_until=?2 WHERE id=?1",
                params![n.id, now() + 60],
            )?;
        }
        tx.commit()?;
        Ok(out)
    }
    pub fn release(&self, ids: &[String]) -> Result<()> {
        let mut conn = self.conn.lock().unwrap();
        let tx = conn.transaction()?;
        for id in ids {
            tx.execute("UPDATE nodes SET lease_until=0 WHERE id=?1", [id])?;
        }
        tx.commit()?;
        Ok(())
    }
    pub fn result(&self, job: &str, r: &ProbeResult) -> Result<()> {
        let mut conn = self.conn.lock().unwrap();
        let tx = conn.transaction()?;
        let t = now();
        let (ok, fail, old_latency): (i64, i64, f64) = tx.query_row(
            "SELECT ok,fail,latency_ms FROM nodes WHERE id=?1",
            [&r.id],
            |row| Ok((row.get(0)?, row.get(1)?, row.get(2)?)),
        )?;
        let status = if r.status == "healthy" {
            "healthy"
        } else if r.status == "unsupported" {
            "unsupported"
        } else {
            "failed"
        };
        let good = i64::from(status == "healthy");
        let bad = i64::from(status == "failed");
        let latency = if good == 1 {
            if old_latency > 0.0 {
                0.3 * r.latency_ms + 0.7 * old_latency
            } else {
                r.latency_ms
            }
        } else {
            old_latency
        };
        let next = if good == 1 {
            t + 1800
        } else {
            t + (300_i64.saturating_mul(1_i64 << ((fail + bad).min(7) as u32))).min(21600)
        };
        let score = (ok + good + 1) as f64 / (ok + good + fail + bad + 2) as f64;
        tx.execute("UPDATE nodes SET status=?2,error=?3,ok=ok+?4,fail=fail+?5,latency_ms=?6,exit_ip=CASE WHEN ?4=1 THEN ?7 ELSE exit_ip END,checked_at=?8,next_check=?9,lease_until=0,score=?10 WHERE id=?1",params![r.id,status,r.error.chars().take(200).collect::<String>(),good,bad,latency,r.exit_ip,t,next,score])?;
        tx.execute("UPDATE jobs SET processed=processed+1,ok=ok+?2,failed=failed+?3,unsupported=unsupported+?4,updated_at=?5 WHERE id=?1",params![job,good,bad,i64::from(status=="unsupported"),t])?;
        tx.commit()?;
        Ok(())
    }
    pub fn list(
        &self,
        status: &str,
        after: &str,
        limit: usize,
        protocol: &str,
        source: &str,
    ) -> Result<Value> {
        let position = if after.is_empty() {
            0
        } else {
            after.parse::<i64>().context("Invalid page cursor")?
        };
        let conn = self.conn.lock().unwrap();
        let mut q=conn.prepare("SELECT id,label,protocol,server,port,status,error,ok,fail,latency_ms,exit_ip,checked_at,next_check,rowid FROM nodes WHERE rowid>?1 AND (?2='' OR status=?2) AND (?3='' OR protocol=?3) AND (?4='' OR EXISTS(SELECT 1 FROM memberships m WHERE m.node_id=nodes.id AND m.source_id=?4)) ORDER BY rowid LIMIT ?5")?;
        let mut rows=q.query_map(params![position,status,protocol,source,limit.clamp(1,200) as i64],|r|Ok(json!({"id":r.get::<_,String>(0)?,"label":r.get::<_,String>(1)?,"protocol":r.get::<_,String>(2)?,"server":r.get::<_,String>(3)?,"port":r.get::<_,i64>(4)?,"status":r.get::<_,String>(5)?,"error":r.get::<_,String>(6)?,"ok":r.get::<_,i64>(7)?,"fail":r.get::<_,i64>(8)?,"latency_ms":r.get::<_,f64>(9)?,"exit_ip":r.get::<_,String>(10)?,"checked_at":r.get::<_,i64>(11)?,"next_check":r.get::<_,i64>(12)?,"_cursor":r.get::<_,i64>(13)?})))?.collect::<rusqlite::Result<Vec<_>>>()?;
        let cursor = rows
            .last()
            .and_then(|v| v["_cursor"].as_i64())
            .map(|v| v.to_string())
            .unwrap_or_default();
        for row in &mut rows {
            row.as_object_mut().unwrap().remove("_cursor");
        }
        Ok(json!({"nodes":rows,"next_cursor":cursor}))
    }
    // Private, bounded handoff to the VPS runtime. Older successful candidates
    // are included for revalidation; this does not assert current health.
    pub fn runtime_candidates(&self) -> Result<Value> {
        let conn = self.conn.lock().unwrap();
        let mut q = conn.prepare("SELECT config,exit_ip FROM nodes WHERE status='healthy' AND exit_ip<>'' ORDER BY score DESC,latency_ms,checked_at DESC,id")?;
        let mut rows = q.query([])?;
        let mut nodes = Vec::new();
        let mut ips = HashSet::new();
        while let Some(r) = rows.next()? {
            let config: String = r.get(0)?;
            if config.len() > 128 * 1024 {
                continue;
            }
            let ip: String = r.get(1)?;
            if !ips.insert(ip) {
                continue;
            }
            nodes.push(json!({"config":serde_json::from_str::<Value>(&config)?}));
            if nodes.len() >= MAX_CANDIDATES {
                break;
            }
        }
        Ok(json!({"nodes":nodes}))
    }
    pub fn export(&self, output: &Path, limit: usize) -> Result<Value> {
        let conn = self.conn.lock().unwrap();
        let mut q=conn.prepare("SELECT id,label,config,latency_ms,exit_ip,checked_at,ok,fail,score FROM nodes WHERE status='healthy' AND checked_at>=?1 AND latency_ms<=3000 AND exit_ip<>'' ORDER BY score DESC,latency_ms,checked_at DESC,id")?;
        let mut rows = q.query([now() - 3600])?;
        let mut nodes = Vec::new();
        let mut ips = HashSet::new();
        while let Some(r) = rows.next()? {
            let ip: String = r.get(4)?;
            if !ips.insert(ip.clone()) {
                continue;
            }
            let config: String = r.get(2)?;
            nodes.push(json!({"id":r.get::<_,String>(0)?,"name":r.get::<_,String>(1)?,"config":serde_json::from_str::<Value>(&config)?,"latency_ms":r.get::<_,f64>(3)?,"exit_ip":ip,"checked_at":r.get::<_,i64>(5)?,"ok":r.get::<_,i64>(6)?,"fail":r.get::<_,i64>(7)?,"score":r.get::<_,f64>(8)?}));
            if nodes.len() >= limit.clamp(1, MAX_CANDIDATES) {
                break;
            }
        }
        if nodes.is_empty() {
            bail!("No fresh healthy candidates; previous export retained")
        }
        let value = json!({"schema_version":1,"generated_at":now(),"vantage":"laptop","zen_verified":false,"nodes":nodes});
        let parent = output.parent().unwrap_or(Path::new("."));
        fs::create_dir_all(parent)?;
        let temp = parent.join(format!(".nodebank-export-{}.partial", uuid::Uuid::new_v4()));
        let mut f = fs::OpenOptions::new()
            .write(true)
            .create_new(true)
            .open(&temp)?;
        fs::set_permissions(&temp, fs::Permissions::from_mode(0o600))?;
        serde_json::to_writer(&mut f, &value)?;
        f.write_all(b"\n")?;
        f.sync_all()?;
        fs::rename(&temp, output)?;
        fs::File::open(parent)?.sync_all()?;
        Ok(
            json!({"count":nodes.len(),"path":output.to_string_lossy(),"generated_at":value["generated_at"]}),
        )
    }
    pub fn best(&self) -> Result<Value> {
        let conn = self.conn.lock().unwrap();
        let mut q=conn.prepare("SELECT id,label,protocol,server,port,latency_ms,exit_ip,checked_at,ok,fail,score FROM nodes WHERE status='healthy' AND checked_at>=?1 ORDER BY score DESC,latency_ms,checked_at DESC LIMIT 200")?;
        let rows=q.query_map([now()-3600],|r|Ok(json!({"id":r.get::<_,String>(0)?,"label":r.get::<_,String>(1)?,"protocol":r.get::<_,String>(2)?,"server":r.get::<_,String>(3)?,"port":r.get::<_,i64>(4)?,"latency_ms":r.get::<_,f64>(5)?,"exit_ip":r.get::<_,String>(6)?,"checked_at":r.get::<_,i64>(7)?,"ok":r.get::<_,i64>(8)?,"fail":r.get::<_,i64>(9)?,"score":r.get::<_,f64>(10)?})))?.collect::<rusqlite::Result<Vec<_>>>()?;
        Ok(json!({"nodes":rows}))
    }
    fn remove_spool(&self, raw: &str) {
        let p = Path::new(raw);
        if p.parent() == Some(self.dir.as_path())
            && p.file_name()
                .and_then(|n| n.to_str())
                .map(|n| n.starts_with("upload-") || n.starts_with("subscription-"))
                .unwrap_or(false)
        {
            let _ = fs::remove_file(p);
            for n in 0..3 {
                let _ = fs::remove_file(p.with_extension(format!("decoded-{n}")));
            }
        }
    }
    pub fn cleanup(&self) -> Result<()> {
        let conn = self.conn.lock().unwrap();
        let mut q=conn.prepare("DELETE FROM jobs WHERE state IN ('completed','failed','cancelled') AND id NOT IN (SELECT id FROM jobs ORDER BY created_at DESC,rowid DESC LIMIT 100) RETURNING path")?;
        for row in q.query_map([], |r| r.get::<_, String>(0))? {
            self.remove_spool(&row?);
        }
        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn source_toggle_preserves_private_configuration() {
        let tmp = tempfile::tempdir().unwrap();
        let db = Db::open(tmp.path()).unwrap();
        let source = Source {
            id: "private".into(),
            name: "Private".into(),
            url: "https://source.example/sub?token=SECRET".into(),
            enabled: false,
            interval_secs: 86400,
            last_fetch: 0,
        };
        db.save_source(&source).unwrap();
        for enabled in [true, false] {
            db.set_source_enabled(&source.id, enabled).unwrap();
            let saved = db
                .sources()
                .unwrap()
                .into_iter()
                .find(|s| s.id == source.id)
                .unwrap();
            assert_eq!(saved.enabled, enabled);
            assert_eq!(saved.url, source.url);
            assert_eq!(saved.name, source.name);
            assert_eq!(saved.interval_secs, source.interval_secs);
            assert_eq!(saved.last_fetch, source.last_fetch);
        }
        assert!(db.set_source_enabled("unknown", true).is_err());
    }

    #[test]
    fn five_thousand_candidates_and_export_remain_deduplicated_and_bounded() {
        let tmp = tempfile::tempdir().unwrap();
        let db = Db::open(tmp.path()).unwrap();
        let job = db
            .new_job("import", "fixture", Path::new(""), "uri", 0)
            .unwrap();
        let nodes: Vec<_> = (0..MAX_CANDIDATES + 2)
            .map(|i| crate::parse::parse_uri(&format!("http://127.0.0.1:{}", 10000 + i)).unwrap())
            .collect();
        db.import_batch(&job.id, "fixture", &nodes, nodes.len() as i64)
            .unwrap();
        {
            let conn = db.conn.lock().unwrap();
            for (i, node) in nodes.iter().enumerate() {
                let index = i % (MAX_CANDIDATES + 1);
                let ip = format!("198.18.{}.{}", index / 256, index % 256);
                conn.execute("UPDATE nodes SET status='healthy',exit_ip=?2,checked_at=?3,latency_ms=20 WHERE id=?1",params![node.id,ip,now()]).unwrap();
            }
        }
        assert_eq!(
            db.runtime_candidates().unwrap()["nodes"]
                .as_array()
                .unwrap()
                .len(),
            MAX_CANDIDATES
        );
        let out = tmp.path().join("export.json");
        assert_eq!(
            db.export(&out, usize::MAX).unwrap()["count"],
            MAX_CANDIDATES
        );
        let exported: Value = serde_json::from_slice(&fs::read(out).unwrap()).unwrap();
        let ips: HashSet<_> = exported["nodes"]
            .as_array()
            .unwrap()
            .iter()
            .map(|n| n["exit_ip"].as_str().unwrap())
            .collect();
        assert_eq!(ips.len(), MAX_CANDIDATES);
    }
    use crate::parse::parse_uri;
    #[test]
    fn dedup_and_export_are_complete_and_atomic() {
        let tmp = tempfile::tempdir().unwrap();
        let db = Db::open(tmp.path()).unwrap();
        let job = db
            .new_job("import", "test", Path::new(""), "uri", 0)
            .unwrap();
        let a=parse_uri("vless://11111111-1111-4111-8111-111111111111@example.com:443?security=tls&sni=example.org&type=ws&path=%2Fsocket#first").unwrap();
        let mut b = a.clone();
        b.label = "other label".into();
        db.import_batch(&job.id, "test", &[a.clone(), b], 2)
            .unwrap();
        assert_eq!(db.counts().unwrap()["total"], 1);
        let other=parse_uri("vless://22222222-2222-4222-8222-222222222222@example.com:443?security=tls&sni=example.org&type=ws&path=%2Fsocket").unwrap();
        db.import_batch(&job.id, "test", &[other.clone()], 3)
            .unwrap();
        assert_eq!(db.counts().unwrap()["total"], 2);
        let first = db.list("", "", 1, "", "test").unwrap();
        let cursor = first["next_cursor"].as_str().unwrap();
        let second = db.list("", cursor, 1, "", "test").unwrap();
        assert_ne!(first["nodes"][0]["id"], second["nodes"][0]["id"]);
        assert!(first["nodes"][0]["_cursor"].is_null());
        for id in [&a.id, &other.id] {
            db.result(
                &job.id,
                &ProbeResult {
                    id: id.clone(),
                    status: "healthy".into(),
                    latency_ms: 20.0,
                    exit_ip: "203.0.113.1".into(),
                    error: "".into(),
                },
            )
            .unwrap();
        }
        let out = tmp.path().join("hot.json");
        assert_eq!(db.export(&out, 300).unwrap()["count"], 1);
        let content = fs::read(&out).unwrap();
        let v: Value = serde_json::from_slice(&content).unwrap();
        assert_eq!(v["nodes"][0]["config"]["tls"]["server_name"], "example.org");
        assert_eq!(v["nodes"][0]["config"]["transport"]["path"], "/socket");
        db.conn
            .lock()
            .unwrap()
            .execute("UPDATE nodes SET checked_at=0", [])
            .unwrap();
        assert!(db.export(&out, 300).is_err());
        assert_eq!(fs::read(&out).unwrap(), content);
        // Stale successful candidates remain eligible for a new VPS check,
        // while the public export retains its strict freshness contract.
        let candidates = db.runtime_candidates().unwrap();
        assert_eq!(candidates["nodes"].as_array().unwrap().len(), 1);
        assert_eq!(
            candidates["nodes"][0]["config"]["tls"]["server_name"],
            "example.org"
        );
    }
    #[test]
    fn sqlite_busy_preserves_checkpoint() {
        let tmp = tempfile::tempdir().unwrap();
        let db = Db::open(tmp.path()).unwrap();
        db.conn
            .lock()
            .unwrap()
            .busy_timeout(std::time::Duration::from_millis(50))
            .unwrap();
        let job = db.new_job("import", "t", Path::new(""), "uri", 0).unwrap();
        let other = Connection::open(tmp.path().join("bank.db")).unwrap();
        other.execute_batch("BEGIN IMMEDIATE").unwrap();
        let nodes = [parse_uri("http://127.0.0.1:8123").unwrap()];
        assert!(db.import_batch(&job.id, "t", &nodes, 1).is_err());
        assert_eq!(db.job(&job.id).unwrap().unwrap().processed, 0);
        other.execute_batch("ROLLBACK").unwrap();
        db.import_batch(&job.id, "t", &nodes, 1).unwrap();
        assert_eq!(db.counts().unwrap()["total"], 1);
    }
    #[test]
    fn queue_is_bounded_and_history_trimmed() {
        let tmp = tempfile::tempdir().unwrap();
        let db = Db::open(tmp.path()).unwrap();
        let mut ids = Vec::new();
        for _ in 0..8 {
            ids.push(
                db.new_job("import", "t", Path::new(""), "uri", 0)
                    .unwrap()
                    .id,
            )
        }
        assert!(db.new_job("import", "t", Path::new(""), "uri", 0).is_err());
        for id in ids {
            db.state(&id, "cancelled", "").unwrap()
        }
        for _ in 0..120 {
            let j = db.new_job("export", "", Path::new(""), "json", 0).unwrap();
            db.state(&j.id, "completed", "").unwrap()
        }
        assert_eq!(
            db.conn
                .lock()
                .unwrap()
                .query_row("SELECT count(*) FROM jobs", [], |r| r.get::<_, i64>(0))
                .unwrap(),
            100
        );
    }
    #[test]
    fn interrupted_leases_and_jobs_recover() {
        let tmp = tempfile::tempdir().unwrap();
        let id;
        {
            let db = Db::open(tmp.path()).unwrap();
            let j = db.new_job("import", "t", Path::new(""), "uri", 0).unwrap();
            id = j.id.clone();
            db.state(&id, "running", "").unwrap();
            db.import_batch(&id, "t", &[parse_uri("http://127.0.0.1:8123").unwrap()], 1)
                .unwrap();
            assert_eq!(db.lease(128).unwrap().len(), 1);
        }
        let db = Db::open(tmp.path()).unwrap();
        assert_eq!(db.job(&id).unwrap().unwrap().state, "running");
        assert!(db.lease(128).unwrap().is_empty());
        db.recover().unwrap();
        assert_eq!(db.job(&id).unwrap().unwrap().state, "paused");
        assert_eq!(db.lease(128).unwrap().len(), 1);
    }
}
