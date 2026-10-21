mod parse;
mod runtime;
mod store;
mod web;
use anyhow::{bail, Result};
use clap::{Parser, Subcommand};
use runtime::{App, Settings};
use std::{fs, os::fd::AsRawFd, path::PathBuf, sync::Arc};
use store::Db;
#[derive(Parser)]
#[command(version, about = "Disk-backed laptop proxy candidate bank")]
struct Cli {
    #[arg(long, global = true, default_value = "data")]
    data: PathBuf,
    #[command(subcommand)]
    command: Commands,
}
#[derive(Subcommand)]
enum Commands {
    Serve {
        #[arg(long, default_value = "127.0.0.1:8792")]
        bind: String,
        #[arg(long, default_value = "dist/nodebank-probe")]
        worker: PathBuf,
        #[arg(long, default_value = "https://www.gstatic.com/generate_204")]
        health_url: String,
        #[arg(long, default_value = "https://api.ipify.org")]
        exit_url: String,
        #[arg(long, default_value_t = 204)]
        expected_status: u16,
        #[arg(long)]
        check_ca: Option<PathBuf>,
    },
    Import {
        file: PathBuf,
        #[arg(long, default_value = "auto")]
        format: String,
        #[arg(long, default_value = "local")]
        source: String,
    },
    Stats,
    Export {
        #[arg(long, default_value = "data/healthy-v1.json")]
        output: PathBuf,
        #[arg(long, default_value_t = 300)]
        limit: usize,
    },
}
fn lock(dir: &std::path::Path) -> Result<fs::File> {
    fs::create_dir_all(dir)?;
    let file = fs::OpenOptions::new()
        .read(true)
        .write(true)
        .create(true)
        .truncate(false)
        .open(dir.join("engine.lock"))?;
    if unsafe { libc::flock(file.as_raw_fd(), libc::LOCK_EX | libc::LOCK_NB) } != 0 {
        bail!("This bank already has an active engine; use its dashboard/API")
    };
    Ok(file)
}
fn validate_https(raw: &str) -> Result<()> {
    let u = url::Url::parse(raw)?;
    if u.scheme() != "https"
        || u.host_str().is_none()
        || u.username() != ""
        || u.password().is_some()
    {
        bail!("Probe targets must be HTTPS URLs without credentials")
    }
    Ok(())
}
fn main() -> Result<()> {
    unsafe { libc::umask(0o077) };
    let cli = Cli::parse();
    let _lock = if matches!(cli.command, Commands::Stats) {
        None
    } else {
        Some(lock(&cli.data)?)
    };
    let db = Arc::new(Db::open(&cli.data)?);
    match cli.command {
        Commands::Stats => println!("{}", serde_json::to_string(&db.counts()?)?),
        Commands::Export { output, limit } => println!("{}", db.export(&output, limit)?),
        Commands::Import {
            file,
            format,
            source,
        } => {
            if !["auto", "uri", "yaml", "base64"].contains(&format.as_str()) {
                bail!("Unknown input format")
            };
            if fs::metadata(&file)?.len() > 512 * 1024 * 1024 {
                bail!("Input exceeds 512 MiB")
            };
            db.recover()?;
            let j = db.new_job("import", &source, &file, &format, 0)?;
            db.state(&j.id, "running", "")?;
            if let Err(e) = parse::import(&db, &j) {
                db.state(&j.id, "failed", &e.to_string())?;
                return Err(e);
            }
            println!("{}", serde_json::to_string(&db.job(&j.id)?)?);
        }
        Commands::Serve {
            bind,
            worker,
            health_url,
            exit_url,
            expected_status,
            check_ca,
        } => {
            let address: std::net::SocketAddr = bind.parse()?;
            if !address.ip().is_loopback() {
                bail!("Dashboard must bind to loopback")
            };
            validate_https(&health_url)?;
            validate_https(&exit_url)?;
            db.recover()?;
            let app = App::new(
                db,
                Settings {
                    worker,
                    health: health_url,
                    exit: exit_url,
                    expected: expected_status,
                    check_ca_pem: if let Some(p) = check_ca {
                        if fs::metadata(&p)?.len() > 16384 {
                            bail!("Check CA file exceeds 16 KiB")
                        };
                        fs::read_to_string(p)?
                    } else {
                        String::new()
                    },
                },
            );
            let rt = tokio::runtime::Builder::new_multi_thread()
                .worker_threads(2)
                .max_blocking_threads(2)
                .enable_all()
                .build()?;
            rt.block_on(async {
                runtime::launch(app.clone());
                web::serve(app, address).await
            })?;
        }
    }
    Ok(())
}
