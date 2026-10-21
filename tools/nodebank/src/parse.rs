use crate::store::{Candidate, Db, Job};
use anyhow::{bail, Context, Result};
use base64::{
    engine::{general_purpose, GeneralPurpose, GeneralPurposeConfig},
    Engine,
};
use serde_json::{json, Value};
use sha2::{Digest, Sha256};
use std::{
    collections::BTreeMap,
    fs::{self, File},
    io::{self, BufRead, BufReader, Read, Write},
    path::{Path, PathBuf},
};
use yaml_rust2::{
    parser::{Event, Parser},
    scanner::TScalarStyle,
};

const MAX_RECORD: usize = 64 * 1024;
const MAX_FILE: u64 = 512 * 1024 * 1024;
fn hash(v: &Value) -> String {
    format!("{:x}", Sha256::digest(serde_json::to_vec(v).unwrap()))
}
fn text(v: &Value) -> String {
    match v {
        Value::String(s) => s.clone(),
        Value::Null => String::new(),
        _ => v.to_string(),
    }
}
fn s(v: &Value, k: &str) -> String {
    text(&v[k])
}
fn b(v: &Value, k: &str) -> bool {
    matches!(s(v, k).as_str(), "true" | "1")
}
fn decode(s: &str) -> Result<Vec<u8>> {
    for e in [
        &general_purpose::STANDARD,
        &general_purpose::STANDARD_NO_PAD,
        &general_purpose::URL_SAFE,
        &general_purpose::URL_SAFE_NO_PAD,
    ] {
        if let Ok(v) = e.decode(s) {
            return Ok(v);
        }
    }
    bail!("Invalid base64 node")
}
fn unescape(s: &str) -> String {
    percent_encoding::percent_decode_str(s)
        .decode_utf8_lossy()
        .into_owned()
}

fn candidate(mut config: Value, label: String, error: Option<String>) -> Candidate {
    // serde_json's ordered map yields a stable identity independent of source labels.
    if config["tls"]["reality"]["enabled"] == true && config["tls"]["utls"].is_null() {
        config["tls"]["utls"] = json!({"enabled":true,"fingerprint":"chrome"});
    }
    let protocol = s(&config, "type");
    let server = s(&config, "server");
    let port = config["server_port"].as_u64().unwrap_or(0) as u16;
    let mut label = label;
    for key in ["password", "uuid"] {
        if let Some(secret) = config[key].as_str() {
            if !secret.is_empty() {
                label = label.replace(secret, "••••");
            }
        }
    }
    if let Some(obj) = config.as_object_mut() {
        obj.remove("name");
        obj.remove("tag");
    }
    Candidate {
        id: hash(&config),
        label: if label.is_empty() {
            format!("{protocol} {server}:{port}")
        } else {
            label.chars().take(256).collect()
        },
        protocol,
        server,
        port,
        config,
        error,
    }
}
pub fn rejected(raw: &str, reason: &str) -> Candidate {
    candidate(
        json!({"type":"unsupported","input_hash":format!("{:x}",Sha256::digest(raw.as_bytes()))}),
        "Unsupported input".into(),
        Some(reason.chars().take(200).collect()),
    )
}

fn transport(kind: &str, path: &str, host: &str, service: &str) -> Result<Option<Value>> {
    Ok(match kind {
        "" | "tcp" => None,
        "ws" | "websocket" => Some(
            json!({"type":"ws","path":if path.is_empty(){"/"}else{path},"headers":if host.is_empty(){json!({})}else{json!({"Host":host})}}),
        ),
        "grpc" => Some(json!({"type":"grpc","service_name":service})),
        "h2" | "http" => Some(
            json!({"type":"http","path":if path.is_empty(){"/"}else{path},"host":if host.is_empty(){vec![]}else{vec![host]}}),
        ),
        "httpupgrade" => {
            Some(json!({"type":"httpupgrade","path":if path.is_empty(){"/"}else{path},"host":host}))
        }
        _ => bail!("Unsupported transport"),
    })
}
fn validate(config: &Value) -> Option<String> {
    let kind = s(config, "type");
    let server = s(config, "server");
    let port = config["server_port"].as_u64().unwrap_or(0);
    if server.is_empty() || port == 0 || port > 65535 {
        return Some("Invalid server or port".into());
    }
    if ["vless", "vmess"].contains(&kind.as_str())
        && uuid::Uuid::parse_str(&s(config, "uuid")).is_err()
    {
        return Some("Invalid UUID".into());
    }
    if kind == "vless" && !matches!(s(config, "flow").as_str(), "" | "xtls-rprx-vision") {
        return Some("Unsupported VLESS flow".into());
    }
    if kind == "shadowsocks"
        && !matches!(
            s(config, "method").as_str(),
            "aes-128-gcm"
                | "aes-256-gcm"
                | "chacha20-ietf-poly1305"
                | "xchacha20-ietf-poly1305"
                | "2022-blake3-aes-128-gcm"
                | "2022-blake3-aes-256-gcm"
                | "2022-blake3-chacha20-poly1305"
        )
    {
        return Some("Unsupported Shadowsocks cipher".into());
    }
    if config["tls"]["reality"]["enabled"] == true
        && s(&config["tls"]["reality"], "public_key").is_empty()
    {
        return Some("Missing Reality public key".into());
    }
    None
}
pub fn parse_uri(raw: &str) -> Result<Candidate> {
    let raw = raw.trim().trim_start_matches('\u{feff}');
    if raw.len() > MAX_RECORD {
        bail!("Node exceeds 64 KiB")
    }
    if raw.starts_with("vmess://") {
        let data = decode(
            raw.trim_start_matches("vmess://")
                .split('#')
                .next()
                .unwrap_or(""),
        )?;
        let v: Value = serde_json::from_slice(&data).context("Invalid VMess JSON")?;
        let mut c = json!({"type":"vmess","server":s(&v,"add"),"server_port":s(&v,"port").parse::<u16>().unwrap_or(0),"uuid":s(&v,"id"),"security":if s(&v,"scy").is_empty(){"auto".into()}else{s(&v,"scy")},"alter_id":s(&v,"aid").parse::<u32>().unwrap_or(0)});
        let mut err = None;
        match transport(
            &s(&v, "net"),
            &s(&v, "path"),
            &s(&v, "host"),
            &s(&v, "path"),
        ) {
            Ok(Some(t)) => c["transport"] = t,
            Ok(None) => (),
            Err(_) => err = Some("Unsupported VMess transport".into()),
        }
        if matches!(s(&v, "tls").as_str(), "tls" | "true" | "1") {
            c["tls"] = json!({"enabled":true,"server_name":if s(&v,"sni").is_empty(){s(&v,"add")}else{s(&v,"sni")},"insecure":b(&v,"allowInsecure")});
            if !s(&v, "fp").is_empty() {
                c["tls"]["utls"] = json!({"enabled":true,"fingerprint":s(&v,"fp")});
            }
        }
        if !s(&v, "type").is_empty() && s(&v, "type") != "none" && s(&v, "net") == "tcp" {
            err = Some("Unsupported VMess TCP header".into())
        }
        let err = err.or_else(|| validate(&c));
        return Ok(candidate(c, s(&v, "ps"), err));
    }
    let owned;
    if !raw.contains("://") {
        owned = format!("http://{raw}")
    } else {
        owned = raw.to_owned()
    }
    let mut u = url::Url::parse(&owned).map_err(|_| anyhow::anyhow!("Invalid node URI"))?;
    let protocol = match u.scheme() {
        "vless" => "vless",
        "trojan" => "trojan",
        "ss" => "shadowsocks",
        "http" | "https" => "http",
        "socks" | "socks5" | "socks5h" => "socks",
        _ => bail!("Unsupported protocol"),
    };
    if protocol == "shadowsocks"
        && u.host_str().is_some()
        && u.port().is_none()
        && u.username().is_empty()
    {
        let decoded = String::from_utf8(decode(
            owned
                .trim_start_matches("ss://")
                .split('#')
                .next()
                .unwrap_or(""),
        )?)
        .context("Invalid Shadowsocks URI")?;
        u = url::Url::parse(&format!("ss://{decoded}"))
            .map_err(|_| anyhow::anyhow!("Invalid Shadowsocks address"))?;
    }
    let q: BTreeMap<String, String> = u
        .query_pairs()
        .map(|(a, b)| (a.into_owned(), b.into_owned()))
        .collect();
    let get = |k: &str| q.get(k).map(String::as_str).unwrap_or("");
    let server = u
        .host_str()
        .unwrap_or("")
        .trim_matches(['[', ']'])
        .to_owned();
    let port = u
        .port_or_known_default()
        .unwrap_or(if protocol == "socks" { 1080 } else { 443 });
    let mut c = json!({"type":protocol,"server":server,"server_port":port});
    let mut err = None;
    match protocol {
        "vless" => {
            c["uuid"] = json!(unescape(u.username()));
            c["flow"] = json!(get("flow"));
        }
        "trojan" => {
            c["password"] = json!(unescape(u.username()));
        }
        "http" | "socks" => {
            if !u.username().is_empty() {
                c["username"] = json!(unescape(u.username()));
                c["password"] = json!(unescape(u.password().unwrap_or("")));
            }
        }
        "shadowsocks" => {
            let auth = if let Some(p) = u.password() {
                format!("{}:{}", unescape(u.username()), unescape(p))
            } else {
                String::from_utf8(decode(&unescape(u.username()))?)
                    .context("Invalid Shadowsocks credentials")?
            };
            let (method, password) = auth
                .split_once(':')
                .context("Invalid Shadowsocks credentials")?;
            c["method"] = json!(method);
            c["password"] = json!(password);
            if !get("plugin").is_empty() {
                err = Some("Shadowsocks plugins are unsupported in v1".into())
            }
        }
        _ => (),
    }
    if !["http", "socks", "shadowsocks"].contains(&protocol) {
        match transport(get("type"), get("path"), get("host"), get("serviceName")) {
            Ok(Some(t)) => c["transport"] = t,
            Ok(None) => (),
            Err(_) => err = Some("Unsupported transport".into()),
        }
    }
    let security = get("security");
    if !["", "none", "tls", "reality"].contains(&security) {
        err = Some("Unsupported security mode".into())
    }
    if protocol == "trojan" || u.scheme() == "https" || ["tls", "reality"].contains(&security) {
        c["tls"] = json!({"enabled":true,"server_name":if get("sni").is_empty(){server.as_str()}else{get("sni")},"insecure":matches!(get("allowInsecure"),"1"|"true")||matches!(get("insecure"),"1"|"true")});
        if !get("alpn").is_empty() {
            c["tls"]["alpn"] = json!(get("alpn").split(',').collect::<Vec<_>>());
        }
        if !get("fp").is_empty() {
            c["tls"]["utls"] = json!({"enabled":true,"fingerprint":get("fp")});
        }
        if security == "reality" {
            c["tls"]["reality"] =
                json!({"enabled":true,"public_key":get("pbk"),"short_id":get("sid")});
        }
    }
    if let Some(v) = q.get("ed") {
        if c["transport"]["type"] == "ws" {
            c["transport"]["max_early_data"] = json!(v.parse::<u32>().unwrap_or(0));
            c["transport"]["early_data_header_name"] = json!(if get("eh").is_empty() {
                "Sec-WebSocket-Protocol"
            } else {
                get("eh")
            });
        }
    }
    for key in q.keys() {
        if ![
            "type",
            "security",
            "flow",
            "pbk",
            "sid",
            "sni",
            "fp",
            "alpn",
            "path",
            "host",
            "serviceName",
            "allowInsecure",
            "insecure",
            "ed",
            "eh",
            "plugin",
            "encryption",
            "headerType",
            "spx",
            "mode",
        ]
        .contains(&key.as_str())
        {
            err = Some("Unmodeled URI connection option".into());
        }
    }
    if !matches!(get("encryption"), "" | "none")
        || !matches!(get("headerType"), "" | "none")
        || !matches!(get("mode"), "" | "gun")
    {
        err = Some("Unsupported URI encryption/header/mode".into());
    }
    let err = err.or_else(|| validate(&c));
    Ok(candidate(c, unescape(u.fragment().unwrap_or("")), err))
}

pub fn parse_clash(v: Value) -> Candidate {
    let protocol = match s(&v, "type").as_str() {
        "ss" => "shadowsocks",
        "socks5" => "socks",
        _ => return parse_clash_typed(v),
    };
    let mut v = v;
    v["type"] = json!(protocol);
    parse_clash_typed(v)
}
fn parse_clash_typed(v: Value) -> Candidate {
    let protocol = s(&v, "type");
    let server = s(&v, "server");
    let port = s(&v, "port").parse::<u16>().unwrap_or(0);
    let mut c = json!({"type":protocol,"server":server,"server_port":port});
    let mut err = None;
    if !["vless", "vmess", "trojan", "shadowsocks", "http", "socks"].contains(&protocol.as_str()) {
        err = Some("Unsupported protocol".into())
    }
    for k in ["uuid", "password", "username", "flow"] {
        if !v[k].is_null() {
            c[k] = v[k].clone()
        }
    }
    if protocol == "shadowsocks" {
        c["method"] = v["cipher"].clone();
        if !s(&v, "plugin").is_empty() {
            err = Some("Shadowsocks plugins are unsupported in v1".into())
        }
    }
    if protocol == "vmess" {
        c["security"] = if s(&v, "cipher").is_empty() {
            json!("auto")
        } else {
            v["cipher"].clone()
        };
        c["alter_id"] = json!(s(&v, "alterId").parse::<u32>().unwrap_or(0));
    }
    let network = s(&v, "network");
    let opt = if ["ws", "websocket"].contains(&network.as_str()) {
        &v["ws-opts"]
    } else if network == "grpc" {
        &v["grpc-opts"]
    } else if ["h2", "http"].contains(&network.as_str()) {
        &v["h2-opts"]
    } else {
        &v["http-upgrade-opts"]
    };
    let host = if opt["host"].is_array() {
        text(&opt["host"][0])
    } else if !s(opt, "host").is_empty() {
        s(opt, "host")
    } else {
        s(&opt["headers"], "Host")
    };
    match transport(
        &network,
        &s(opt, "path"),
        &host,
        &s(opt, "grpc-service-name"),
    ) {
        Ok(Some(mut t)) => {
            if network == "ws" {
                if opt["headers"].is_object() {
                    t["headers"] = opt["headers"].clone();
                }
                if !opt["max-early-data"].is_null() {
                    t["max_early_data"] =
                        json!(s(opt, "max-early-data").parse::<u32>().unwrap_or(0));
                    t["early_data_header_name"] =
                        json!(if s(opt, "early-data-header-name").is_empty() {
                            "Sec-WebSocket-Protocol".to_owned()
                        } else {
                            s(opt, "early-data-header-name")
                        });
                }
            }
            if ["h2", "http"].contains(&network.as_str()) && opt["host"].is_array() {
                t["host"] = opt["host"].clone();
            }
            c["transport"] = t;
        }
        Ok(None) => (),
        Err(_) => err = Some("Unsupported transport".into()),
    }
    if b(&v, "tls") || protocol == "trojan" || v["reality-opts"].is_object() {
        c["tls"] = json!({"enabled":true,"server_name":if !s(&v,"servername").is_empty(){s(&v,"servername")}else if !s(&v,"sni").is_empty(){s(&v,"sni")}else{server.clone()},"insecure":b(&v,"skip-cert-verify")});
        if v["alpn"].is_array() {
            c["tls"]["alpn"] = v["alpn"].clone();
        }
        if !s(&v, "client-fingerprint").is_empty() {
            c["tls"]["utls"] = json!({"enabled":true,"fingerprint":s(&v,"client-fingerprint")});
        }
        if v["reality-opts"].is_object() {
            c["tls"]["reality"] = json!({"enabled":true,"public_key":s(&v["reality-opts"],"public-key"),"short_id":s(&v["reality-opts"],"short-id")});
            if s(&v, "client-fingerprint").is_empty() {
                c["tls"]["utls"] = json!({"enabled":true,"fingerprint":"chrome"});
            }
        }
    }
    for k in ["dialer-proxy", "packet-encoding", "smux", "xudp"] {
        if !v[k].is_null() && v[k] != false {
            err = Some(format!("Unsupported Clash option: {k}"));
        }
    }
    if v["udp"].as_bool() == Some(true) { /* This engine checks TCP only; UDP preference is not needed for an HTTPS probe. */
    }
    let err = err.or_else(|| validate(&c));
    candidate(c, s(&v, "name"), err)
}

/// Filter whitespace without allocating the complete encoded subscription.
struct Compact<R>(R);
impl<R: Read> Read for Compact<R> {
    fn read(&mut self, out: &mut [u8]) -> io::Result<usize> {
        let mut buf = [0u8; 8192];
        loop {
            let n = self.0.read(&mut buf[..out.len().min(8192)])?;
            if n == 0 {
                return Ok(0);
            }
            let mut used = 0;
            for &ch in &buf[..n] {
                if !ch.is_ascii_whitespace() {
                    out[used] = match ch {
                        b'-' => b'+',
                        b'_' => b'/',
                        _ => ch,
                    };
                    used += 1;
                }
            }
            if used > 0 {
                return Ok(used);
            }
        }
    }
}
fn limited_copy<R: Read>(mut r: R, path: &Path) -> Result<()> {
    let mut f = File::create(path)?;
    let mut buf = [0u8; 65536];
    let mut size = 0u64;
    loop {
        let n = r.read(&mut buf)?;
        if n == 0 {
            break;
        };
        size += n as u64;
        if size > MAX_FILE {
            bail!("Decoded subscription exceeds 512 MiB")
        };
        if crate::runtime::disk_free(path.parent().unwrap_or(Path::new("."))) < 1024 * 1024 * 1024 {
            bail!("Insufficient disk space for decoded subscription")
        };
        f.write_all(&buf[..n])?;
    }
    f.flush()?;
    Ok(())
}
struct DecodedFiles(PathBuf);
impl Drop for DecodedFiles {
    fn drop(&mut self) {
        for layer in 0..3 {
            let _ = fs::remove_file(self.0.with_extension(format!("decoded-{layer}")));
        }
    }
}
pub fn prepare(path: &Path, format: &str) -> Result<(PathBuf, String)> {
    let mut current = path.to_owned();
    for layer in 0..3 {
        let mut f = File::open(&current)?;
        let mut head = [0u8; 4096];
        let n = f.read(&mut head)?;
        if head[..n].starts_with(&[0x1f, 0x8b]) {
            let next = path.with_extension(format!("decoded-{layer}"));
            limited_copy(flate2::read::GzDecoder::new(File::open(&current)?), &next)?;
            current = next;
            continue;
        }
        let prefix = String::from_utf8_lossy(&head[..n]);
        let first = prefix
            .lines()
            .map(str::trim)
            .find(|l| !l.is_empty() && !l.starts_with('#'))
            .unwrap_or("");
        if first.starts_with("proxies:")
            || first.starts_with("---")
            || first.starts_with("proxy-providers:")
            || format == "yaml"
            || (!first.contains("://")
                && first
                    .split_once(':')
                    .map(|(_, v)| v.is_empty() || v.starts_with(' '))
                    .unwrap_or(false))
        {
            return Ok((current, "yaml".into()));
        }
        if first.contains("://") || first.contains(':') || format == "uri" {
            return Ok((current, "uri".into()));
        }
        if first.is_empty() {
            return Ok((current, "uri".into()));
        }
        let next = path.with_extension(format!("decoded-{layer}"));
        let engine = GeneralPurpose::new(
            &base64::alphabet::STANDARD,
            GeneralPurposeConfig::new()
                .with_decode_padding_mode(base64::engine::DecodePaddingMode::Indifferent),
        );
        limited_copy(
            base64::read::DecoderReader::new(
                Compact(BufReader::new(File::open(&current)?)),
                &engine,
            ),
            &next,
        )
        .context("Invalid base64 subscription")?;
        current = next;
    }
    bail!("Too many nested subscription encodings")
}
struct Chars<R> {
    r: R,
    line: usize,
    failed: bool,
}
impl<R: Read> Iterator for Chars<R> {
    type Item = char;
    fn next(&mut self) -> Option<char> {
        if self.failed {
            return None;
        }
        let mut b = [0u8; 4];
        match self.r.read(&mut b[..1]) {
            Ok(0) => return None,
            Ok(_) => (),
            Err(_) => {
                self.failed = true;
                return Some('\0');
            }
        }
        if b[0] == b'\n' {
            self.line = 0
        } else {
            self.line += 1;
            if self.line > 1024 * 1024 {
                self.failed = true;
                return Some('\0');
            }
        }
        let count = match b[0] {
            0..=0x7f => 1,
            0xc2..=0xdf => 2,
            0xe0..=0xef => 3,
            0xf0..=0xf4 => 4,
            _ => {
                self.failed = true;
                return Some('\0');
            }
        };
        if count > 1 && self.r.read_exact(&mut b[1..count]).is_err() {
            self.failed = true;
            return Some('\0');
        }
        match std::str::from_utf8(&b[..count]) {
            Ok(s) => s.chars().next(),
            Err(_) => {
                self.failed = true;
                Some('\0')
            }
        }
    }
}
struct Frame {
    value: Value,
    key: Option<String>,
}
fn push(frames: &mut [Frame], value: Value) -> Result<()> {
    let parent = frames.last_mut().context("Invalid YAML proxy structure")?;
    match &mut parent.value {
        Value::Array(a) => a.push(value),
        Value::Object(m) => {
            if let Some(k) = parent.key.take() {
                m.insert(k, value);
            } else {
                parent.key = Some(text(&value));
            }
        }
        _ => bail!("Invalid YAML container"),
    };
    Ok(())
}
fn scalar(value: String, style: TScalarStyle) -> Value {
    if style != TScalarStyle::Plain {
        return json!(value);
    }
    match value.as_str() {
        "true" | "True" | "TRUE" => json!(true),
        "false" | "False" | "FALSE" => json!(false),
        "null" | "~" | "" => Value::Null,
        _ => {
            // Keep numeric-looking credentials and Reality short IDs verbatim.
            json!(value)
        }
    }
}
pub fn stream_yaml<R: Read, F: FnMut(Candidate) -> Result<()>>(
    reader: R,
    mut emit: F,
) -> Result<()> {
    let mut parser = Parser::new(Chars {
        r: BufReader::with_capacity(65536, reader),
        line: 0,
        failed: false,
    });
    let mut depth = 0usize;
    let mut expect = false;
    let mut seq_depth = None;
    let mut frames: Vec<Frame> = Vec::new();
    let mut size = 0;
    let mut found = false;
    loop {
        let (event, _) = parser
            .next_token()
            .map_err(|_| anyhow::anyhow!("Invalid YAML syntax, encoding, or line over 1 MiB"))?;
        if let Event::StreamEnd = event {
            break;
        }
        let anchor = match &event {
            Event::Scalar(_, _, a, _) | Event::MappingStart(a, _) | Event::SequenceStart(a, _) => {
                *a
            }
            _ => 0,
        };
        if anchor > 256 {
            bail!("YAML with more than 256 anchors is unsupported; use URI subscriptions")
        }
        if !frames.is_empty() {
            match event {
                Event::MappingStart(_, _) => {
                    depth += 1;
                    frames.push(Frame {
                        value: json!({}),
                        key: None,
                    });
                }
                Event::SequenceStart(_, _) => {
                    depth += 1;
                    frames.push(Frame {
                        value: json!([]),
                        key: None,
                    });
                }
                Event::Scalar(v, style, _, _) => {
                    size += v.len();
                    if size > MAX_RECORD {
                        bail!("YAML proxy exceeds 64 KiB")
                    };
                    push(&mut frames, scalar(v, style))?;
                }
                Event::MappingEnd | Event::SequenceEnd => {
                    depth = depth.saturating_sub(1);
                    let frame = frames.pop().unwrap();
                    if frames.is_empty() {
                        emit(parse_clash(frame.value))?;
                        size = 0;
                    } else {
                        push(&mut frames, frame.value)?;
                    }
                }
                Event::Alias(_) => bail!(
                    "YAML aliases within proxies are unsupported; expand aliases before import"
                ),
                _ => (),
            }
            if frames.len() > 16 {
                bail!("YAML proxy nesting exceeds 16 levels")
            };
            continue;
        }
        match event {
            Event::Scalar(v, _, _, _) => {
                if seq_depth == Some(depth) {
                    emit(rejected(&v, "Proxy entry must be a mapping"))?;
                } else {
                    expect = depth == 1 && v == "proxies";
                }
            }
            Event::SequenceStart(_, _) => {
                depth += 1;
                if expect {
                    seq_depth = Some(depth);
                    found = true;
                    expect = false;
                }
            }
            Event::MappingStart(_, _) => {
                if seq_depth == Some(depth) {
                    frames.push(Frame {
                        value: json!({}),
                        key: None,
                    });
                }
                depth += 1;
            }
            Event::SequenceEnd | Event::MappingEnd => {
                if seq_depth == Some(depth) {
                    seq_depth = None;
                }
                depth = depth.saturating_sub(1);
                expect = false;
            }
            Event::Alias(_) => {
                if seq_depth == Some(depth) {
                    emit(rejected("alias", "YAML proxy aliases are unsupported"))?;
                }
            }
            _ => (),
        }
    }
    if !found {
        bail!("YAML must contain a top-level proxies sequence")
    }
    Ok(())
}
pub fn stream_uri<R: Read, F: FnMut(Candidate) -> Result<()>>(
    reader: R,
    mut emit: F,
) -> Result<()> {
    let mut r = BufReader::with_capacity(65536, reader);
    let mut line = Vec::with_capacity(1024);
    loop {
        line.clear();
        let mut over = false;
        let mut ended = false;
        loop {
            let buf = r.fill_buf()?;
            if buf.is_empty() {
                ended = true;
                break;
            }
            let pos = buf.iter().position(|&b| b == b'\n');
            let n = pos.map_or(buf.len(), |p| p + 1);
            if line.len() + n <= MAX_RECORD {
                line.extend_from_slice(&buf[..n])
            } else {
                over = true;
            }
            r.consume(n);
            if pos.is_some() {
                break;
            }
        }
        if line.is_empty() && ended {
            break;
        }
        if over {
            emit(rejected("oversized", "Node exceeds 64 KiB"))?;
            continue;
        }
        let raw = String::from_utf8_lossy(&line);
        let raw = raw.trim();
        if raw.is_empty() || raw.starts_with('#') {
            if ended {
                break;
            } else {
                continue;
            }
        }
        emit(parse_uri(raw).unwrap_or_else(|e| rejected(raw, &e.to_string())))?;
        if ended {
            break;
        }
    }
    Ok(())
}
pub fn import(db: &Db, job: &Job) -> Result<()> {
    let _decoded = DecodedFiles(PathBuf::from(&job.path));
    let (path, format) = prepare(Path::new(&job.path), &job.format)?;
    let mut position = 0i64;
    let mut batch = Vec::with_capacity(500);
    let mut emit = |c: Candidate| -> Result<()> {
        position += 1;
        if position <= job.processed {
            return Ok(());
        }
        batch.push(c);
        if batch.len() == 500 {
            db.import_batch(&job.id, &job.source_id, &batch, position)?;
            batch.clear();
            if db.job(&job.id)?.map(|j| j.state) != (Some("running".into())) {
                bail!("Import paused")
            }
        }
        Ok(())
    };
    let file = File::open(&path)?;
    let result = if format == "yaml" {
        stream_yaml(file, &mut emit)
    } else {
        stream_uri(file, &mut emit)
    };
    if !batch.is_empty() {
        db.import_batch(&job.id, &job.source_id, &batch, position)?;
    }
    result?;
    db.finish_import(job)?;
    // Decoded intermediate files are temporary, not a second permanent bank.
    for layer in 0..3 {
        let p = Path::new(&job.path).with_extension(format!("decoded-{layer}"));
        if p.exists() {
            let _ = fs::remove_file(p);
        }
    }
    let original = Path::new(&job.path);
    if original.parent() == Some(db.dir.as_path())
        && original
            .file_name()
            .and_then(|s| s.to_str())
            .map(|s| s.starts_with("upload-") || s.starts_with("subscription-"))
            .unwrap_or(false)
    {
        let _ = fs::remove_file(original);
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn yaml_credentials_keep_leading_zeroes() {
        let body="proxies:\n  - {name: safe, type: vless, server: test.example, port: 443, uuid: 11111111-1111-4111-8111-111111111111, reality-opts: {public-key: public, short-id: 000123}}\n  - {name: second, type: ss, server: test.example, port: 443, cipher: aes-128-gcm, password: 001122}\n";
        let mut nodes = Vec::new();
        stream_yaml(body.as_bytes(), |n| {
            nodes.push(n);
            Ok(())
        })
        .unwrap();
        assert_eq!(nodes[0].config["tls"]["reality"]["short_id"], "000123");
        assert_eq!(nodes[0].config["tls"]["utls"]["fingerprint"], "chrome");
        assert_eq!(nodes[1].config["password"], "001122");
    }
    #[test]
    fn full_clash_auto_detection() {
        let tmp = tempfile::tempdir().unwrap();
        let p = tmp.path().join("clash");
        fs::write(
            &p,
            "mixed-port: 7890\nproxies:\n  - {type: http, server: test.example, port: 80}\n",
        )
        .unwrap();
        assert_eq!(prepare(&p, "auto").unwrap().1, "yaml");
    }
    #[test]
    fn reality_and_transports_survive() {
        let n=parse_uri("vless://11111111-1111-4111-8111-111111111111@edge.example:443?security=reality&pbk=public&sid=0123&sni=www.example.org&fp=chrome&flow=xtls-rprx-vision#hello").unwrap();
        assert!(n.error.is_none());
        assert_eq!(n.config["tls"]["reality"]["short_id"], "0123");
        assert_eq!(n.config["tls"]["server_name"], "www.example.org");
        assert_eq!(n.config["flow"], "xtls-rprx-vision");
        let v = parse_uri(
            "trojan://secret@example.org:443?type=grpc&serviceName=tunnel&sni=front.example",
        )
        .unwrap();
        assert_eq!(v.config["transport"]["service_name"], "tunnel");
        assert_eq!(v.config["password"], "secret");
    }
    #[test]
    fn streamed_yaml_handles_flow_entries_and_nested_options() {
        let body="proxies:\n  - {name: first, type: vless, server: edge.example, port: 443, uuid: 11111111-1111-4111-8111-111111111111, tls: true, network: ws, ws-opts: {path: /abc, headers: {Host: front.example}}}\n  - name: second\n    type: ss\n    server: 127.0.0.1\n    port: 1000\n    cipher: aes-128-gcm\n    password: test\n";
        let mut nodes = vec![];
        stream_yaml(body.as_bytes(), |n| {
            nodes.push(n);
            Ok(())
        })
        .unwrap();
        assert_eq!(nodes.len(), 2);
        assert!(nodes.iter().all(|n| n.error.is_none()));
        assert_eq!(
            nodes[0].config["transport"]["headers"]["Host"],
            "front.example"
        );
        assert_eq!(nodes[1].protocol, "shadowsocks");
    }
    #[test]
    fn unsupported_is_not_dead() {
        let n = parse_uri("vless://11111111-1111-4111-8111-111111111111@example.org:443?type=kcp")
            .unwrap();
        assert!(n.error.is_some());
        let n = parse_clash(
            json!({"name":"old","type":"ss","server":"example.org","port":443,"cipher":"rc4-md5","password":"secret"}),
        );
        assert_eq!(n.error.as_deref(), Some("Unsupported Shadowsocks cipher"));
    }
    #[test]
    fn base64_subscription_is_decoded_to_disk() {
        let tmp = tempfile::tempdir().unwrap();
        let p = tmp.path().join("input");
        fs::write(
            &p,
            general_purpose::STANDARD.encode(b"http://127.0.0.1:1000\nhttp://127.0.0.1:1001\n"),
        )
        .unwrap();
        let (decoded, f) = prepare(&p, "auto").unwrap();
        assert_eq!(f, "uri");
        let mut count = 0;
        stream_uri(File::open(decoded).unwrap(), |_| {
            count += 1;
            Ok(())
        })
        .unwrap();
        assert_eq!(count, 2);
    }
    #[test]
    fn huge_line_is_discarded_without_unbounded_read() {
        let input = vec![b'A'; MAX_RECORD * 2];
        let mut c = 0;
        stream_uri(input.as_slice(), |n| {
            assert!(n.error.is_some());
            c += 1;
            Ok(())
        })
        .unwrap();
        assert_eq!(c, 1);
    }
}
