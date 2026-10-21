use crate::{
    parse,
    store::{now, Db, ProbeNode, ProbeResult},
};
use anyhow::{bail, Context, Result};
use futures_util::{stream, StreamExt};
use serde_json::{json, Value};
use std::{
    collections::{HashMap, VecDeque},
    fs,
    path::{Path, PathBuf},
    sync::{
        atomic::{AtomicBool, AtomicU32, AtomicU64, Ordering},
        Arc, Mutex,
    },
    time::{Duration, Instant},
};
use tokio::{
    io::{AsyncBufReadExt, AsyncWriteExt, BufReader},
    process::{Child, ChildStdin, ChildStdout, Command},
};

pub async fn blocking<T: Send + 'static>(
    f: impl FnOnce() -> Result<T> + Send + 'static,
) -> Result<T> {
    tokio::task::spawn_blocking(f).await?
}
pub fn rss(pid: u32) -> u64 {
    fs::read_to_string(format!("/proc/{pid}/status"))
        .ok()
        .and_then(|s| {
            s.lines()
                .find(|l| l.starts_with("VmRSS:"))
                .and_then(|l| l.split_whitespace().nth(1))
                .and_then(|n| n.parse::<u64>().ok())
        })
        .unwrap_or(0)
        * 1024
}
pub fn available() -> u64 {
    fs::read_to_string("/proc/meminfo")
        .ok()
        .and_then(|s| {
            s.lines()
                .find(|l| l.starts_with("MemAvailable:"))
                .and_then(|l| l.split_whitespace().nth(1))
                .and_then(|n| n.parse::<u64>().ok())
        })
        .unwrap_or(0)
        * 1024
}
pub fn pressure(engine: u64, worker: u64, free: u64) -> Option<&'static str> {
    if engine + worker >= 256 * 1024 * 1024 {
        Some("Combined RSS reached 256 MiB")
    } else if free < 768 * 1024 * 1024 {
        Some("Laptop available RAM below 768 MiB")
    } else {
        None
    }
}
pub fn disk_free(path: &Path) -> u64 {
    use std::os::unix::ffi::OsStrExt;
    let Ok(p) = std::ffi::CString::new(path.as_os_str().as_bytes()) else {
        return 0;
    };
    let mut stat = std::mem::MaybeUninit::<libc::statvfs>::uninit();
    unsafe {
        if libc::statvfs(p.as_ptr(), stat.as_mut_ptr()) != 0 {
            return 0;
        }
        let s = stat.assume_init();
        s.f_bavail * s.f_frsize
    }
}
#[derive(Clone)]
pub struct Settings {
    pub worker: PathBuf,
    pub health: String,
    pub exit: String,
    pub expected: u16,
    pub check_ca_pem: String,
}
pub struct App {
    pub db: Arc<Db>,
    pub settings: Settings,
    pub background: AtomicBool,
    pub paused: AtomicBool,
    pub worker_pid: AtomicU32,
    pub metrics: Mutex<Value>,
    pub reason: Mutex<String>,
    pub protocol: AtomicU32,
    pub tcp: AtomicU32,
    pub checked: AtomicU64,
    pub import_gate: Arc<tokio::sync::Semaphore>,
}
impl App {
    pub fn new(db: Arc<Db>, settings: Settings) -> Arc<Self> {
        Arc::new(Self {
            db,
            settings,
            background: AtomicBool::new(false),
            paused: AtomicBool::new(true),
            worker_pid: AtomicU32::new(0),
            metrics: Mutex::new(json!({})),
            reason: Mutex::new("Paused on startup".into()),
            protocol: AtomicU32::new(16),
            tcp: AtomicU32::new(128),
            checked: AtomicU64::new(0),
            import_gate: Arc::new(tokio::sync::Semaphore::new(1)),
        })
    }
    pub fn stop(&self, reason: &str) {
        self.paused.store(true, Ordering::Relaxed);
        *self.reason.lock().unwrap() = reason.into()
    }
    pub fn start(&self) {
        self.paused.store(false, Ordering::Relaxed);
        self.reason.lock().unwrap().clear()
    }
    pub fn should_stop(&self) -> bool {
        if self.paused.load(Ordering::Relaxed) {
            return true;
        }
        if let Some(reason) = pressure(
            rss(std::process::id()),
            rss(self.worker_pid.load(Ordering::Relaxed)),
            available(),
        ) {
            self.stop(reason);
            return true;
        }
        false
    }
    pub fn statistics(&self) -> Value {
        let mut v = self.metrics.lock().unwrap().clone();
        let Some(o) = v.as_object_mut() else {
            return json!({});
        };
        let e = rss(std::process::id());
        let w = rss(self.worker_pid.load(Ordering::Relaxed));
        o.insert("engine_rss_bytes".into(), json!(e));
        o.insert("worker_rss_bytes".into(), json!(w));
        o.insert("combined_rss_bytes".into(), json!(e + w));
        o.insert("available_ram_bytes".into(), json!(available()));
        o.insert("paused".into(), json!(self.paused.load(Ordering::Relaxed)));
        o.insert(
            "pause_reason".into(),
            json!(self.reason.lock().unwrap().clone()),
        );
        o.insert(
            "tcp_parallelism".into(),
            json!(self.tcp.load(Ordering::Relaxed)),
        );
        o.insert(
            "protocol_parallelism".into(),
            json!(self.protocol.load(Ordering::Relaxed)),
        );
        o.insert(
            "background".into(),
            json!(self.background.load(Ordering::Relaxed)),
        );
        v
    }
}
struct Worker {
    child: Child,
    input: ChildStdin,
    output: BufReader<ChildStdout>,
}
impl Worker {
    async fn spawn(app: &App) -> Result<Self> {
        let mut command = Command::new(&app.settings.worker);
        // Prevent an orphan worker if the owner is killed before Rust destructors run.
        let parent = std::process::id();
        unsafe {
            command.pre_exec(move || {
                if libc::prctl(libc::PR_SET_PDEATHSIG, libc::SIGTERM) != 0 {
                    return Err(std::io::Error::last_os_error());
                }
                if libc::getppid() as u32 != parent {
                    return Err(std::io::Error::other("Engine exited before worker started"));
                }
                Ok(())
            });
        }
        let mut child = command
            .env("GOMAXPROCS", "2")
            .env("GOMEMLIMIT", "96MiB")
            .stdin(std::process::Stdio::piped())
            .stdout(std::process::Stdio::piped())
            .stderr(std::process::Stdio::null())
            .kill_on_drop(true)
            .spawn()
            .context("Cannot start protocol worker; build probe first")?;
        app.worker_pid
            .store(child.id().unwrap_or(0), Ordering::Relaxed);
        Ok(Self {
            input: child.stdin.take().unwrap(),
            output: BufReader::new(child.stdout.take().unwrap()),
            child,
        })
    }
    async fn send(&mut self, n: &ProbeNode, app: &App) -> Result<()> {
        let mut data = serde_json::to_vec(
            &json!({"id":n.id,"config":n.config,"resolved_ip":n.resolved_ip,"timeout_ms":15000,"deadline_ms":now()*1000+15000,"health_url":app.settings.health,"exit_url":app.settings.exit,"expected_status":app.settings.expected,"check_ca_pem":app.settings.check_ca_pem}),
        )?;
        if data.len() > 128 * 1024 {
            bail!("Worker frame limit exceeded")
        };
        data.push(b'\n');
        tokio::time::timeout(Duration::from_secs(2), self.input.write_all(&data)).await??;
        Ok(())
    }
    async fn receive(&mut self) -> Result<ProbeResult> {
        let mut line = Vec::new();
        loop {
            let buf =
                tokio::time::timeout(Duration::from_secs(18), self.output.fill_buf()).await??;
            if buf.is_empty() {
                bail!("Worker exited")
            };
            let end = buf.iter().position(|b| *b == b'\n');
            let n = end.map_or(buf.len(), |p| p + 1);
            if line.len() + n > 128 * 1024 {
                bail!("Worker result frame limit exceeded")
            };
            line.extend_from_slice(&buf[..n]);
            self.output.consume(n);
            if end.is_some() {
                break;
            }
        }
        Ok(serde_json::from_slice(&line)?)
    }
    async fn batch(&mut self, app: &Arc<App>, job: &str, nodes: Vec<ProbeNode>) -> Result<()> {
        let mut waiting = VecDeque::from(nodes);
        let mut active: HashMap<String, Instant> = HashMap::new();
        let mut failures = Vec::new();
        loop {
            let db = app.db.clone();
            let jid = job.to_owned();
            let job_running =
                blocking(move || Ok(db.active_scan()?.map(|j| j.id) == Some(jid))).await?;
            while job_running
                && active.len() < (app.protocol.load(Ordering::Relaxed) as usize)
                && !app.should_stop()
            {
                let Some(n) = waiting.pop_front() else { break };
                if let Err(e) = self.send(&n, app).await {
                    waiting.push_front(n);
                    let ids: Vec<_> = active
                        .keys()
                        .cloned()
                        .chain(waiting.iter().map(|n| n.id.clone()))
                        .collect();
                    let db = app.db.clone();
                    blocking(move || db.release(&ids)).await?;
                    return Err(e);
                }
                active.insert(n.id.clone(), Instant::now());
            }
            if active.is_empty() {
                let ids: Vec<_> = waiting.iter().map(|n| n.id.clone()).collect();
                let db = app.db.clone();
                blocking(move || db.release(&ids)).await?;
                break;
            }
            let remaining = active
                .values()
                .map(|sent| Duration::from_secs(18).saturating_sub(sent.elapsed()))
                .min()
                .unwrap_or(Duration::from_secs(18));
            let received = tokio::time::timeout(remaining, self.receive())
                .await
                .unwrap_or_else(|_| Err(anyhow::anyhow!("Worker job deadline exceeded")));
            match received {
                Ok(mut r) => {
                    if active.remove(&r.id).is_none() {
                        bail!("Worker returned unexpected job")
                    };
                    if !r.latency_ms.is_finite() || r.latency_ms < 0.0 {
                        r.status = "failed".into();
                        r.error = "Invalid worker latency".into()
                    }
                    if r.status == "healthy" && r.exit_ip.parse::<std::net::IpAddr>().is_err() {
                        r.status = "failed".into();
                        r.error = "Invalid worker exit IP".into()
                    }
                    if r.status == "failed" {
                        failures.push(r)
                    } else {
                        let db = app.db.clone();
                        let job = job.to_owned();
                        blocking(move || db.result(&job, &r)).await?;
                        app.checked.fetch_add(1, Ordering::Relaxed);
                    }
                }
                Err(e) => {
                    let ids: Vec<_> = active
                        .keys()
                        .cloned()
                        .chain(waiting.iter().map(|n| n.id.clone()))
                        .collect();
                    let db = app.db.clone();
                    blocking(move || db.release(&ids)).await?;
                    return Err(e);
                }
            }
        }
        if !failures.is_empty() {
            if baseline(app).await.is_err() {
                let ids = failures.into_iter().map(|r| r.id).collect::<Vec<_>>();
                let db = app.db.clone();
                blocking(move || db.release(&ids)).await?;
                app.stop(
                    "Laptop connection or check endpoint failed; unverified failures requeued",
                );
            } else {
                let n = failures.len();
                let db = app.db.clone();
                let job = job.to_owned();
                blocking(move || {
                    for r in failures {
                        db.result(&job, &r)?
                    }
                    Ok(())
                })
                .await?;
                app.checked.fetch_add(n as u64, Ordering::Relaxed);
            }
        }
        Ok(())
    }
}
async fn baseline(app: &App) -> Result<()> {
    let mut builder = reqwest::Client::builder().no_proxy();
    if !app.settings.check_ca_pem.is_empty() {
        builder = builder.add_root_certificate(reqwest::Certificate::from_pem(
            app.settings.check_ca_pem.as_bytes(),
        )?);
    }
    let client = builder
        .timeout(Duration::from_secs(8))
        .redirect(reqwest::redirect::Policy::none())
        .build()?;
    for (target, expected) in [
        (&app.settings.health, app.settings.expected),
        (&app.settings.exit, 200),
    ] {
        let resp = client
            .get(target)
            .send()
            .await
            .map_err(|e| anyhow::anyhow!("Baseline HTTPS: {:#}", e.without_url()))?;
        if resp.status().as_u16() != expected {
            bail!(
                "Check endpoint returned HTTP {} (expected {})",
                resp.status().as_u16(),
                expected
            )
        };
        let mut body = resp.bytes_stream();
        let mut size = 0;
        while let Some(b) = body.next().await {
            size += b?.len();
            if size > 16384 {
                bail!("Baseline response exceeds 16 KiB")
            }
        }
    }
    Ok(())
}
async fn tcp(server: String, port: u16) -> Option<String> {
    tokio::time::timeout(Duration::from_secs(3), async move {
        let addrs = tokio::net::lookup_host((server.as_str(), port)).await?;
        for addr in addrs.take(4) {
            if tokio::net::TcpStream::connect(addr).await.is_ok() {
                return Ok::<_, std::io::Error>(Some(addr.ip().to_string()));
            }
        }
        Ok(None)
    })
    .await
    .ok()
    .and_then(Result::ok)
    .flatten()
}
pub fn launch(app: Arc<App>) {
    let a = app.clone();
    tokio::spawn(async move {
        let started = Instant::now();
        loop {
            let db = a.db.clone();
            if let Ok((counts, jobs)) = blocking(move || Ok((db.counts()?, db.jobs()?))).await {
                let progress=jobs.into_iter().map(|j|json!({"id":j.id,"kind":j.kind,"state":j.state,"processed":j.processed,"total":j.total,"added":j.added,"ok":j.ok,"failed":j.failed,"unsupported":j.unsupported})).collect::<Vec<_>>();
                let checked = a.checked.load(Ordering::Relaxed);
                *a.metrics.lock().unwrap() = json!({"counts":counts,"jobs":progress,"checked_since_start":checked,"uptime_secs":started.elapsed().as_secs(),"average_nodes_per_second":checked as f64/started.elapsed().as_secs_f64().max(1.0)});
            }
            let total = rss(std::process::id()) + rss(a.worker_pid.load(Ordering::Relaxed));
            if total > 220 * 1024 * 1024 {
                a.protocol.store(8, Ordering::Relaxed);
                a.tcp.store(64, Ordering::Relaxed);
            } else if total < 180 * 1024 * 1024
                && available() > 1536 * 1024 * 1024
                && !a.paused.load(Ordering::Relaxed)
            {
                a.protocol.store(
                    (a.protocol.load(Ordering::Relaxed) + 1).min(32),
                    Ordering::Relaxed,
                );
                a.tcp.store(
                    (a.tcp.load(Ordering::Relaxed) + 8).min(256),
                    Ordering::Relaxed,
                );
            }
            if !a.paused.load(Ordering::Relaxed) {
                a.should_stop();
            }
            tokio::time::sleep(Duration::from_secs(5)).await;
        }
    });
    let a = app.clone();
    tokio::spawn(async move {
        loop {
            let db = a.db.clone();
            let work = blocking(move || db.claim_import()).await;
            match work {
                Ok(Some(job)) => {
                    let db = a.db.clone();
                    let j = job.clone();
                    let _ = blocking(move || {
                        let result = parse::import(&db, &j);
                        if result.is_err()
                            && db.job(&j.id)?.map(|x| x.state) == Some("running".into())
                        {
                            db.state(&j.id, "failed", &result.unwrap_err().to_string())?
                        }
                        db.cleanup()?;
                        Ok(())
                    })
                    .await;
                }
                _ => tokio::time::sleep(Duration::from_millis(500)).await,
            }
        }
    });
    tokio::spawn(async move {
        let mut worker = None;
        let mut last_baseline = Instant::now() - Duration::from_secs(60);
        let mut cache: HashMap<(String, u16), (Instant, Option<String>)> = HashMap::new();
        loop {
            if worker.is_none() {
                match Worker::spawn(&app).await {
                    Ok(w) => worker = Some(w),
                    Err(_) => {
                        app.stop("Protocol worker unavailable; build or check its path");
                        tokio::time::sleep(Duration::from_secs(5)).await;
                        continue;
                    }
                }
            }
            if app.should_stop() {
                cache.clear();
                last_baseline = Instant::now() - Duration::from_secs(60);
                tokio::time::sleep(Duration::from_millis(500)).await;
                continue;
            }
            if last_baseline.elapsed() > Duration::from_secs(30) {
                if let Err(e) = baseline(&app).await {
                    app.stop(&format!("Laptop/check endpoint unavailable: {e}"));
                    continue;
                }
                last_baseline = Instant::now();
            }
            let db = app.db.clone();
            let job = match blocking(move || db.active_scan()).await {
                Ok(Some(j)) => j,
                _ => {
                    app.stop("No active scan; start or resume a job");
                    continue;
                }
            };
            let db = app.db.clone();
            let nodes = match blocking(move || db.lease(128)).await {
                Ok(n) => n,
                Err(_) => {
                    app.stop("Database busy or unavailable; jobs retained");
                    continue;
                }
            };
            if nodes.is_empty() {
                if app.background.load(Ordering::Relaxed) {
                    tokio::time::sleep(Duration::from_secs(10)).await;
                } else {
                    let db = app.db.clone();
                    let id = job.id;
                    let _ = blocking(move || db.state(&id, "completed", "")).await;
                    app.stop("Scan completed");
                }
                continue;
            }
            cache.retain(|_, (at, _)| at.elapsed() < Duration::from_secs(60));
            if cache.len() > 8192 {
                cache.clear()
            }
            let mut endpoints = HashMap::new();
            for n in &nodes {
                if !cache.contains_key(&(n.server.clone(), n.port)) {
                    endpoints.insert((n.server.clone(), n.port), ());
                }
            }
            let tests = stream::iter(endpoints.into_keys().map(|(server, port)| async move {
                let ok = tcp(server.clone(), port).await;
                ((server, port), ok)
            }))
            .buffer_unordered(app.tcp.load(Ordering::Relaxed) as usize);
            tokio::pin!(tests);
            while let Some((key, ok)) = tests.next().await {
                cache.insert(key, (Instant::now(), ok));
            }
            if app.should_stop() {
                let ids = nodes.iter().map(|n| n.id.clone()).collect::<Vec<_>>();
                let db = app.db.clone();
                let _ = blocking(move || db.release(&ids)).await;
                continue;
            }
            // Confirm the laptop is still online before committing a batch of failures.
            if nodes
                .iter()
                .all(|n| cache[&(n.server.clone(), n.port)].1.is_none())
                && baseline(&app).await.is_err()
            {
                let ids = nodes.iter().map(|n| n.id.clone()).collect::<Vec<_>>();
                let db = app.db.clone();
                let _ = blocking(move || db.release(&ids)).await;
                app.stop("Connection lost; untested nodes retained");
                continue;
            }
            let mut ready = Vec::new();
            for mut n in nodes {
                if let Some(ip) = &cache[&(n.server.clone(), n.port)].1 {
                    n.resolved_ip = ip.clone();
                    ready.push(n)
                } else {
                    let db = app.db.clone();
                    let id = job.id.clone();
                    let _ = blocking(move || {
                        db.result(
                            &id,
                            &ProbeResult {
                                id: n.id,
                                status: "failed".into(),
                                latency_ms: 0.0,
                                exit_ip: String::new(),
                                error: "DNS / TCP failed within 3 seconds".into(),
                            },
                        )
                    })
                    .await;
                    app.checked.fetch_add(1, Ordering::Relaxed);
                }
            }
            let ready_ids = ready.iter().map(|n| n.id.clone()).collect::<Vec<_>>();
            if let Err(_) = worker.as_mut().unwrap().batch(&app, &job.id, ready).await {
                let db = app.db.clone();
                let _ = blocking(move || db.release(&ready_ids)).await;
                if let Some(mut w) = worker.take() {
                    let _ = w.child.kill().await;
                }
                app.worker_pid.store(0, Ordering::Relaxed);
                app.stop("Worker crashed or stalled; unfinished nodes requeued. Resume to retry");
            }
        }
    });
}
#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn memory_pressure_thresholds() {
        assert_eq!(
            pressure(200 * 1024 * 1024, 56 * 1024 * 1024, 2 * 1024 * 1024 * 1024),
            Some("Combined RSS reached 256 MiB")
        );
        assert!(pressure(40 * 1024 * 1024, 40 * 1024 * 1024, 767 * 1024 * 1024).is_some());
        assert!(pressure(40 * 1024 * 1024, 40 * 1024 * 1024, 2 * 1024 * 1024 * 1024).is_none());
    }
}
