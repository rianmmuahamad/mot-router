use crate::{
    runtime::{blocking, disk_free, App},
    store::Source,
};
use anyhow::{bail, Result};
use axum::{
    body::Body,
    extract::{DefaultBodyLimit, Multipart, Path, Query, State},
    http::{HeaderMap, Request, StatusCode},
    middleware::{self, Next},
    response::{
        sse::{Event, KeepAlive, Sse},
        Html, IntoResponse, Response,
    },
    routing::{get, post},
    Json, Router,
};
use futures_util::StreamExt;
use serde::Deserialize;
use serde_json::{json, Value};
use std::{
    convert::Infallible,
    sync::{atomic::Ordering, Arc},
    time::Duration,
};
use tokio::io::AsyncWriteExt;
type A = Arc<App>;
struct Error(anyhow::Error);
impl<E: Into<anyhow::Error>> From<E> for Error {
    fn from(e: E) -> Self {
        Self(e.into())
    }
}
impl IntoResponse for Error {
    fn into_response(self) -> Response {
        (
            StatusCode::BAD_REQUEST,
            Json(json!({"error":self.0.to_string()})),
        )
            .into_response()
    }
}
type Api<T> = std::result::Result<Json<T>, Error>;
async fn guard(req: Request<Body>, next: Next) -> Response {
    let host = req
        .headers()
        .get("host")
        .and_then(|h| h.to_str().ok())
        .unwrap_or("");
    let host_ok = host.starts_with("127.0.0.1:")
        || host.starts_with("localhost:")
        || host.starts_with("[::1]:");
    let origin_ok = req
        .headers()
        .get("origin")
        .and_then(|v| v.to_str().ok())
        .map(|v| v == format!("http://{host}"))
        .unwrap_or(true);
    let fetch_ok = req
        .headers()
        .get("sec-fetch-site")
        .and_then(|v| v.to_str().ok())
        .map(|v| v != "cross-site")
        .unwrap_or(true);
    if !host_ok || !origin_ok || !fetch_ok {
        return StatusCode::FORBIDDEN.into_response();
    }
    let mut response = next.run(req).await;
    response
        .headers_mut()
        .insert("cache-control", "no-store".parse().unwrap());
    response
        .headers_mut()
        .insert("x-content-type-options", "nosniff".parse().unwrap());
    response.headers_mut().insert("content-security-policy","default-src 'self'; script-src 'self'; style-src 'self'; connect-src 'self'; frame-ancestors 'none'".parse().unwrap());
    response
}
pub async fn serve(app: A, address: std::net::SocketAddr) -> Result<()> {
    let router = Router::new()
        .route(
            "/",
            get(|| async { Html(include_str!("../assets/index.html")) }),
        )
        .route(
            "/app.js",
            get(|| async {
                (
                    [("content-type", "text/javascript")],
                    include_str!("../assets/app.js"),
                )
            }),
        )
        .route(
            "/style.css",
            get(|| async {
                (
                    [("content-type", "text/css")],
                    include_str!("../assets/style.css"),
                )
            }),
        )
        .route(
            "/api/catalog",
            get(|| async {
                Json(
                    serde_json::from_str::<Value>(include_str!("../assets/source-references.json"))
                        .unwrap(),
                )
            }),
        )
        .route("/api/stats", get(stats))
        .route("/api/events", get(events))
        .route("/api/sources", get(sources).post(save_source))
        .route("/api/sources/{id}/import", post(source_import))
        .route("/api/sources/{id}/enabled", post(source_enabled))
        .route("/api/jobs", get(jobs))
        .route("/api/jobs/{id}", get(job))
        .route("/api/jobs/{id}/control", post(control))
        .route(
            "/api/upload",
            post(upload).layer(DefaultBodyLimit::max(512 * 1024 * 1024 + 1024 * 1024)),
        )
        .route("/api/nodes", get(nodes))
        .route("/api/best", get(best))
        .route("/api/runtime/candidates", get(runtime_candidates))
        .route("/api/scan", post(scan))
        .route("/api/export", post(export))
        .route("/api/push", post(push))
        .route("/api/export/download", get(download))
        .layer(DefaultBodyLimit::max(64 * 1024))
        .layer(middleware::from_fn(guard))
        .with_state(app);
    let listener = tokio::net::TcpListener::bind(address).await?;
    println!("Mot NodeBank: http://{address} — scanning paused");
    axum::serve(listener, router)
        .with_graceful_shutdown(async {
            let _ = tokio::signal::ctrl_c().await;
        })
        .await?;
    Ok(())
}
async fn stats(State(a): State<A>) -> Json<Value> {
    Json(a.statistics())
}
async fn events(
    State(a): State<A>,
) -> Sse<impl futures_util::Stream<Item = std::result::Result<Event, Infallible>>> {
    Sse::new(futures_util::stream::unfold(a, |a| async move {
        tokio::time::sleep(Duration::from_secs(2)).await;
        let e = Event::default()
            .event("stats")
            .data(a.statistics().to_string());
        Some((Ok(e), a))
    }))
    .keep_alive(KeepAlive::default())
}
fn safe_source(s: Source) -> Value {
    let mut v = serde_json::to_value(&s).unwrap();
    if let Ok(mut url) = url::Url::parse(&s.url) {
        if url.query().is_some() {
            url.set_query(Some("redacted"))
        };
        v["url"] = json!(url.to_string())
    };
    v
}
async fn sources(State(a): State<A>) -> Api<Value> {
    let db = a.db.clone();
    Ok(Json(
        json!({"sources":blocking(move||db.sources()).await?.into_iter().map(safe_source).collect::<Vec<_>>()}),
    ))
}
async fn save_source(State(a): State<A>, Json(mut s): Json<Source>) -> Api<Value> {
    if s.id.is_empty() {
        s.id = uuid::Uuid::new_v4().to_string()
    }
    if s.url.contains("?redacted") {
        let db = a.db.clone();
        let id = s.id.clone();
        s.url = blocking(move || db.sources())
            .await?
            .into_iter()
            .find(|v| v.id == id)
            .ok_or_else(|| anyhow::anyhow!("Unknown source"))?
            .url;
    };
    let db = a.db.clone();
    blocking(move || {
        if db.sources()?.len() >= 100 && !db.sources()?.iter().any(|v| v.id == s.id) {
            bail!("Maximum 100 sources")
        };
        db.save_source(&s)
    })
    .await?;
    Ok(Json(json!({"ok":true})))
}
fn safe_job(j: crate::store::Job) -> Value {
    let mut v = serde_json::to_value(j).unwrap();
    v.as_object_mut().unwrap().remove("path");
    v
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct SourceEnabled {
    enabled: bool,
}
async fn source_enabled(
    State(a): State<A>,
    Path(id): Path<String>,
    Json(v): Json<SourceEnabled>,
) -> Api<Value> {
    let db = a.db.clone();
    let source_id = id.clone();
    blocking(move || db.set_source_enabled(&source_id, v.enabled)).await?;
    Ok(Json(json!({"id":id,"enabled":v.enabled,"ok":true})))
}
async fn jobs(State(a): State<A>) -> Api<Value> {
    let db = a.db.clone();
    Ok(Json(
        json!({"jobs":blocking(move||db.jobs()).await?.into_iter().map(safe_job).collect::<Vec<_>>()}),
    ))
}
async fn job(State(a): State<A>, Path(id): Path<String>) -> Api<Value> {
    let db = a.db.clone();
    Ok(Json(
        blocking(move || db.job(&id))
            .await?
            .map(safe_job)
            .unwrap_or(json!(null)),
    ))
}
#[derive(Deserialize)]
struct Action {
    action: String,
}
async fn control(State(a): State<A>, Path(id): Path<String>, Json(v): Json<Action>) -> Api<Value> {
    let db = a.db.clone();
    let j = blocking(move || db.job(&id))
        .await?
        .ok_or_else(|| anyhow::anyhow!("Unknown job"))?;
    if !matches!(j.kind.as_str(), "import" | "scan") {
        return Err(anyhow::anyhow!("Use Create export to retry an export job").into());
    }
    let next = match v.action.as_str() {
        "pause" if matches!(j.state.as_str(), "running" | "queued") => "paused",
        "resume"
            if matches!(j.state.as_str(), "paused" | "failed")
                || (j.kind == "scan"
                    && j.state == "running"
                    && a.paused.load(Ordering::Relaxed)) =>
        {
            if j.kind == "import" {
                "queued"
            } else {
                "running"
            }
        }
        "cancel" if !matches!(j.state.as_str(), "completed" | "cancelled") => "cancelled",
        _ => return Err(anyhow::anyhow!("Invalid job transition").into()),
    };
    if j.kind == "scan" {
        if next != "running" {
            a.stop("Paused by user")
        }
    }
    let is_scan = j.kind == "scan";
    if is_scan {
        a.background
            .store(j.format == "background", Ordering::Relaxed);
    }
    let db = a.db.clone();
    blocking(move || db.state(&j.id, next, "")).await?;
    if is_scan && next == "running" {
        a.start()
    }
    Ok(Json(json!({"ok":true})))
}
#[derive(Deserialize, Default)]
struct Scan {
    #[serde(default)]
    background: bool,
}
async fn scan(State(a): State<A>, Json(v): Json<Scan>) -> Api<Value> {
    let db = a.db.clone();
    let j = blocking(move || {
        let total = db.due_total(false)?;
        let j = db.new_job(
            "scan",
            "",
            std::path::Path::new(""),
            if v.background { "background" } else { "manual" },
            total,
        )?;
        db.state(&j.id, "running", "")?;
        Ok(j)
    })
    .await?;
    a.background.store(v.background, Ordering::Relaxed);
    a.start();
    Ok(Json(json!({"job_id":j.id})))
}
#[derive(Deserialize)]
struct Filter {
    #[serde(default)]
    status: String,
    #[serde(default)]
    after: String,
    #[serde(default)]
    protocol: String,
    #[serde(default)]
    source: String,
    #[serde(default = "page")]
    limit: usize,
}
fn page() -> usize {
    100
}
async fn nodes(State(a): State<A>, Query(q): Query<Filter>) -> Api<Value> {
    let db = a.db.clone();
    Ok(Json(
        blocking(move || db.list(&q.status, &q.after, q.limit, &q.protocol, &q.source)).await?,
    ))
}
async fn best(State(a): State<A>) -> Api<Value> {
    let db = a.db.clone();
    Ok(Json(blocking(move || db.best()).await?))
}
async fn runtime_candidates(State(a): State<A>, headers: HeaderMap) -> Result<Response, Error> {
    if headers
        .get("x-nodebank-runtime")
        .and_then(|v| v.to_str().ok())
        != Some("1")
    {
        return Ok(StatusCode::FORBIDDEN.into_response());
    }
    let db = a.db.clone();
    Ok(Json(blocking(move || db.runtime_candidates()).await?).into_response())
}
async fn export(State(a): State<A>) -> Api<Value> {
    let db = a.db.clone();
    let path = db.dir.join("healthy-v1.json");
    let j = blocking(move || db.new_job("export", "", &path, "json", 0)).await?;
    let id = j.id.clone();
    tokio::spawn(async move {
        let db = a.db.clone();
        let _ = blocking(move || {
            db.state(&j.id, "running", "")?;
            match db.export(&std::path::PathBuf::from(&j.path), 300) {
                Ok(_) => db.state(&j.id, "completed", ""),
                Err(e) => db.state(&j.id, "failed", &e.to_string()),
            }
        })
        .await;
    });
    Ok(Json(json!({"job_id":id})))
}

const VPS_RECEIVE_COMMAND: &str = "python3 /home/ubuntu/.local/libexec/mot-nodebank/receive.py --output /opt/mot-router/data/nodebank/healthy-v1.json";

fn valid_ssh_target(target: &str) -> bool {
    !target.is_empty()
        && !target.starts_with('-')
        && target.len() <= 253
        && target
            .bytes()
            .all(|b| b.is_ascii_alphanumeric() || b"_.@-".contains(&b))
}

async fn push(State(a): State<A>) -> Api<Value> {
    let target = std::env::var("NODEBANK_VPS").unwrap_or_else(|_| "mot".to_owned());
    if !valid_ssh_target(&target) {
        return Err(anyhow::anyhow!("NODEBANK_VPS must be a host alias or user@host").into());
    }
    let db = a.db.clone();
    let path = db.dir.join("healthy-v1.json");
    let j = blocking(move || db.new_job("push", &target, &path, "ssh", 0)).await?;
    let id = j.id.clone();
    tokio::spawn(async move {
        let db = a.db.clone();
        let export_job_id = j.id.clone();
        let push_target = j.source_id.clone();
        let export_path = std::path::PathBuf::from(j.path.clone());
        let result: Result<Value> = async {
            let export_db = db.clone();
            let state_job_id = export_job_id.clone();
            let state_path = export_path.clone();
            blocking(move || {
                export_db.state(&state_job_id, "running", "")?;
                export_db.export(&state_path, 300)
            })
            .await?;

            let mut child = tokio::process::Command::new("ssh")
                .args([
                    "-T",
                    "-o",
                    "BatchMode=yes",
                    "-o",
                    "ConnectTimeout=10",
                    "-o",
                    "ServerAliveInterval=15",
                    "-o",
                    "ServerAliveCountMax=2",
                    "--",
                    &push_target,
                    VPS_RECEIVE_COMMAND,
                ])
                .stdin(std::process::Stdio::piped())
                .stdout(std::process::Stdio::piped())
                .stderr(std::process::Stdio::null())
                .kill_on_drop(true)
                .spawn()?;
            let mut input = child
                .stdin
                .take()
                .ok_or_else(|| anyhow::anyhow!("Could not open SSH input"))?;
            let mut file = tokio::fs::File::open(&export_path).await?;
            let transfer = tokio::io::copy(&mut file, &mut input).await;
            drop(input);
            transfer?;
            let output = child.wait_with_output().await?;
            if !output.status.success() {
                bail!("SSH receiver rejected the snapshot (exit status {:?})", output.status.code())
            }
            let ack: Value = serde_json::from_slice(&output.stdout)
                .map_err(|_| anyhow::anyhow!("VPS receiver returned an invalid acknowledgement"))?;
            if ack["ok"] != true {
                bail!("VPS receiver did not accept the snapshot")
            }
            Ok(json!({"count":ack["count"],"generated_at":ack["generated_at"],"received_at":ack["received_at"],"sha256":ack["sha256"]}))
        }
        .await;
        let db = a.db.clone();
        let job_id = export_job_id;
        let (state, message) = match result {
            Ok(ack) => (
                "completed",
                format!(
                    "Transferred and validated {} candidates on VPS",
                    ack["count"]
                ),
            ),
            Err(error) => ("failed", error.to_string()),
        };
        let _ = blocking(move || db.state(&job_id, state, &message)).await;
    });
    Ok(Json(
        json!({"job_id":id,"target":std::env::var("NODEBANK_VPS").unwrap_or_else(|_| "mot".to_owned())}),
    ))
}

async fn download(State(a): State<A>, headers: HeaderMap) -> std::result::Result<Response, Error> {
    if headers
        .get("x-nodebank-export")
        .and_then(|v| v.to_str().ok())
        != Some("1")
    {
        return Err(anyhow::anyhow!(
            "Export contains credentials; use the dashboard download button"
        )
        .into());
    }
    let b = tokio::fs::read(a.db.dir.join("healthy-v1.json")).await?;
    Ok((
        [
            ("content-type", "application/json"),
            (
                "content-disposition",
                "attachment; filename=healthy-v1.json",
            ),
        ],
        b,
    )
        .into_response())
}
async fn capacity(a: &A) -> Result<()> {
    if disk_free(&a.db.dir) < 1536 * 1024 * 1024 {
        bail!("Keep at least 1.5 GiB free for SQLite and temporary input")
    }
    let db = a.db.clone();
    if blocking(move || db.jobs())
        .await?
        .iter()
        .filter(|j| {
            matches!(
                j.state.as_str(),
                "running" | "queued" | "downloading" | "paused"
            )
        })
        .count()
        >= 8
    {
        bail!("Maximum eight unfinished import/check jobs")
    };
    Ok(())
}
async fn upload(State(a): State<A>, mut multipart: Multipart) -> Api<Value> {
    let _permit = a
        .import_gate
        .clone()
        .try_acquire_owned()
        .map_err(|_| anyhow::anyhow!("Another upload/download is active"))?;
    capacity(&a).await?;
    let path =
        a.db.dir
            .join(format!("upload-{}.input", uuid::Uuid::new_v4()));
    let mut f = tokio::fs::OpenOptions::new()
        .create_new(true)
        .write(true)
        .open(&path)
        .await?;
    let result: Result<()> = async {
        let mut size = 0u64;
        let mut found = false;
        while let Some(mut field) = multipart.next_field().await? {
            if field.name() != Some("file") {
                continue;
            }
            if found {
                bail!("Only one file per import")
            };
            found = true;
            while let Some(chunk) = field.chunk().await? {
                size += chunk.len() as u64;
                if size > 512 * 1024 * 1024 || disk_free(&a.db.dir) < 1024 * 1024 * 1024 {
                    bail!("Input size or disk limit reached")
                };
                f.write_all(&chunk).await?;
            }
        }
        if !found {
            bail!("No file uploaded")
        };
        f.sync_all().await?;
        Ok(())
    }
    .await;
    if let Err(e) = result {
        drop(f);
        let _ = tokio::fs::remove_file(&path).await;
        return Err(e.into());
    }
    let db = a.db.clone();
    let j = blocking(move || db.new_job("import", "upload", &path, "auto", 0)).await?;
    Ok(Json(json!({"job_id":j.id})))
}
async fn source_import(State(a): State<A>, Path(id): Path<String>) -> Api<Value> {
    let permit = a
        .import_gate
        .clone()
        .try_acquire_owned()
        .map_err(|_| anyhow::anyhow!("Another upload/download is active"))?;
    capacity(&a).await?;
    let db = a.db.clone();
    let sources = blocking(move || db.sources()).await?;
    let s = sources
        .into_iter()
        .find(|s| s.id == id)
        .ok_or_else(|| anyhow::anyhow!("Unknown source"))?;
    if !s.enabled {
        return Err(anyhow::anyhow!("Enable this source before importing").into());
    }
    let path =
        a.db.dir
            .join(format!("subscription-{}.input", uuid::Uuid::new_v4()));
    let db = a.db.clone();
    let p = path.clone();
    let source = s.id.clone();
    let j = blocking(move || {
        let j = db.new_job("import", &source, &p, "auto", 0)?;
        db.state(&j.id, "downloading", "")?;
        Ok(j)
    })
    .await?;
    let id = j.id.clone();
    tokio::spawn(async move {
        let _permit = permit;
        let result: Result<()> = async {
            let client = reqwest::Client::builder()
                .timeout(Duration::from_secs(120))
                .build()?;
            let response = client
                .get(&s.url)
                .send()
                .await
                .map_err(|_| anyhow::anyhow!("Subscription download failed"))?;
            if !response.status().is_success() {
                bail!("Subscription returned HTTP {}", response.status().as_u16())
            };
            if response.content_length().unwrap_or(0) > 512 * 1024 * 1024 {
                bail!("Subscription exceeds 512 MiB")
            };
            let mut f = tokio::fs::OpenOptions::new()
                .create_new(true)
                .write(true)
                .open(&path)
                .await?;
            let mut stream = response.bytes_stream();
            let mut size = 0u64;
            while let Some(chunk) = stream.next().await {
                let db = a.db.clone();
                let jid = j.id.clone();
                if blocking(move || db.job(&jid)).await?.map(|v| v.state)
                    != Some("downloading".into())
                {
                    bail!("Download cancelled")
                }
                let chunk = chunk.map_err(|_| anyhow::anyhow!("Subscription body interrupted"))?;
                size += chunk.len() as u64;
                if size > 512 * 1024 * 1024 || disk_free(&a.db.dir) < 1024 * 1024 * 1024 {
                    bail!("Subscription size or disk limit reached")
                };
                f.write_all(&chunk).await?;
            }
            f.sync_all().await?;
            Ok(())
        }
        .await;
        let db = a.db.clone();
        let _ = blocking(move || {
            let (next, error) = match result {
                Ok(()) => ("queued", String::new()),
                Err(e) => {
                    let _ = std::fs::remove_file(&path);
                    ("failed", e.to_string())
                }
            };
            if !db.state_if(&j.id, "downloading", next, &error)? {
                let _ = std::fs::remove_file(&path);
            }
            Ok(())
        })
        .await;
    });
    Ok(Json(json!({"job_id":id})))
}

#[cfg(test)]
mod runtime_handoff_tests {
    use super::*;
    use crate::{runtime::Settings, store::Db};
    use std::path::PathBuf;

    #[tokio::test]
    async fn candidate_handoff_requires_explicit_private_header() {
        let temp = tempfile::tempdir().unwrap();
        let app = App::new(
            Arc::new(Db::open(temp.path()).unwrap()),
            Settings {
                worker: PathBuf::from("unused"),
                health: "https://example.com".into(),
                exit: "https://example.com/ip".into(),
                expected: 204,
                check_ca_pem: String::new(),
            },
        );
        let missing = runtime_candidates(State(app.clone()), HeaderMap::new()).await;
        assert_eq!(missing.ok().unwrap().status(), StatusCode::FORBIDDEN);
        let mut headers = HeaderMap::new();
        headers.insert("x-nodebank-runtime", "1".parse().unwrap());
        let allowed = runtime_candidates(State(app), headers).await.ok().unwrap();
        assert_eq!(allowed.status(), StatusCode::OK);
        let body = axum::body::to_bytes(allowed.into_body(), 1024)
            .await
            .unwrap();
        assert_eq!(
            serde_json::from_slice::<Value>(&body).unwrap()["nodes"],
            json!([])
        );
    }
}
