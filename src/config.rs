//! Config resolution: env (if allowed) > exe-dir `telemetry.json` > `~/.config/wake/config.json`.

use serde::{Deserialize, Serialize};
use std::collections::BTreeMap;
use std::path::{Path, PathBuf};

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum IdentityMode {
    Email,
    Anonymous,
}

impl IdentityMode {
    fn parse(s: &str) -> Option<Self> {
        match s {
            "email" => Some(Self::Email),
            "anonymous" => Some(Self::Anonymous),
            _ => None,
        }
    }
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Source {
    Env,
    ExeDir,
    UserConfig,
    Default,
}

impl Source {
    pub fn as_str(&self) -> &'static str {
        match self {
            Source::Env => "env",
            Source::ExeDir => "telemetry.json",
            Source::UserConfig => "config.json",
            Source::Default => "default",
        }
    }
}

#[derive(Debug, Clone)]
pub struct Config {
    pub endpoint: Option<String>,
    /// Includes the derived `Authorization: Basic` header when both keys resolved and none was set explicitly.
    pub headers: Vec<(String, String)>,
    pub identity: IdentityMode,
    pub identity_salt: Option<String>,
    pub allow_override: bool,
    /// True when the `Authorization` header in `headers` was derived from `public_key` and `secret_key`.
    pub basic_from_keys: bool,
    pub disabled: bool,
    /// Keys: "endpoint", "headers", "identity", "public_key", "secret_key".
    pub sources: BTreeMap<&'static str, Source>,
}

pub trait Env {
    fn var(&self, key: &str) -> Option<String>;
}

/// The only reader of the real process environment.
pub struct ProcessEnv;

impl Env for ProcessEnv {
    fn var(&self, key: &str) -> Option<String> {
        std::env::var(key).ok()
    }
}

#[derive(Serialize, Deserialize, Default)]
pub struct FileConfig {
    pub endpoint: Option<String>,
    pub public_key: Option<String>,
    pub secret_key: Option<String>,
    pub headers: Option<BTreeMap<String, String>>,
    pub identity: Option<String>,
    pub identity_salt: Option<String>,
    pub allow_override: Option<bool>,
}

pub fn file_path_user(home: &Path) -> PathBuf {
    home.join(".config").join("wake").join("config.json")
}

/// The user config file actually read: `$XDG_CONFIG_HOME/wake/config.json` when set, else `file_path_user`.
pub fn user_config_path(env: &dyn Env, home: &Path) -> PathBuf {
    match env.var("XDG_CONFIG_HOME").filter(|x| !x.is_empty()) {
        Some(x) => PathBuf::from(x).join("wake").join("config.json"),
        None => file_path_user(home),
    }
}

fn load(path: &Path) -> Option<FileConfig> {
    let bytes = std::fs::read(path).ok()?;
    serde_json::from_slice(&bytes).ok()
}

/// Standard base64 (RFC 4648, with padding).
pub fn base64_encode(input: &[u8]) -> String {
    const T: &[u8; 64] = b"ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/";
    let mut out = String::with_capacity(input.len().div_ceil(3) * 4);
    for chunk in input.chunks(3) {
        let n = (u32::from(chunk[0]) << 16)
            | (u32::from(*chunk.get(1).unwrap_or(&0)) << 8)
            | u32::from(*chunk.get(2).unwrap_or(&0));
        for i in 0..4 {
            if i <= chunk.len() {
                out.push(T[((n >> (18 - 6 * i)) & 63) as usize] as char);
            } else {
                out.push('=');
            }
        }
    }
    out
}

fn percent_decode(s: &str) -> String {
    let b = s.as_bytes();
    let mut out = Vec::with_capacity(b.len());
    let mut i = 0;
    while i < b.len() {
        if b[i] == b'%' && i + 2 < b.len() {
            let hex = |c: u8| (c as char).to_digit(16);
            if let (Some(h), Some(l)) = (hex(b[i + 1]), hex(b[i + 2])) {
                out.push((h * 16 + l) as u8);
                i += 3;
                continue;
            }
        }
        out.push(b[i]);
        i += 1;
    }
    String::from_utf8_lossy(&out).into_owned()
}

/// Parse the OTel `k=v,k2=v2` header format. Only the first `=` splits; values are percent-decoded.
pub fn parse_headers(s: &str) -> Vec<(String, String)> {
    s.split(',')
        .filter_map(|pair| {
            let (k, v) = pair.split_once('=')?;
            let k = k.trim();
            if k.is_empty() {
                return None;
            }
            Some((k.to_string(), percent_decode(v.trim())))
        })
        .collect()
}

/// `<scheme>://<host>[:port]` of a URL: no userinfo, path, query or fragment.
pub(crate) fn origin(url: &str) -> Option<String> {
    let (scheme, rest) = url.split_once("://")?;
    let authority = rest.split(['/', '?', '#']).next().unwrap_or("");
    let authority = authority.rsplit('@').next().unwrap_or("");
    if scheme.is_empty() || authority.is_empty() {
        return None;
    }
    Some(format!("{scheme}://{authority}"))
}

pub(crate) fn valid_endpoint(url: &str) -> bool {
    if url.starts_with("https://") {
        return true;
    }
    let Some(rest) = url.strip_prefix("http://") else {
        return false;
    };
    let authority = rest.split(['/', '?', '#']).next().unwrap_or("");
    if authority.contains('@') {
        return false;
    }
    let host = if authority.starts_with('[') {
        match authority.find(']') {
            Some(end) => {
                let after = &authority[end + 1..];
                if !(after.is_empty() || after.starts_with(':')) {
                    return false;
                }
                &authority[..=end]
            }
            None => return false,
        }
    } else {
        authority.split(':').next().unwrap_or("")
    };
    matches!(host, "localhost" | "127.0.0.1" | "[::1]")
}

pub fn resolve(env: &dyn Env, exe_dir: &Path, home: &Path) -> Config {
    let exe = load(&exe_dir.join("telemetry.json"));
    let user = load(&user_config_path(env, home));
    // File layers in priority order.
    let files: Vec<(Source, FileConfig)> = [(Source::ExeDir, exe), (Source::UserConfig, user)]
        .into_iter()
        .filter_map(|(s, f)| f.map(|f| (s, f)))
        .collect();

    let allow_override = files
        .iter()
        .find_map(|(_, f)| f.allow_override)
        .unwrap_or(true);
    let identity_salt = files
        .iter()
        .find_map(|(_, f)| f.identity_salt.clone().filter(|v| !v.is_empty()));
    let disabled = matches!(
        env.var("WAKE_DISABLED").as_deref(),
        Some("1") | Some("true")
    );

    let envv = |k: &str| {
        if allow_override {
            env.var(k).filter(|v| !v.is_empty())
        } else {
            None
        }
    };

    let mut sources = BTreeMap::new();

    let mut endpoint_cands: Vec<(Source, String)> = Vec::new();
    if let Some(v) = envv("WAKE_OTLP_ENDPOINT") {
        endpoint_cands.push((Source::Env, v));
    }
    for (s, f) in &files {
        if let Some(v) = f.endpoint.as_ref().filter(|v| !v.is_empty()) {
            endpoint_cands.push((*s, v.clone()));
        }
    }
    let (endpoint_src, endpoint) = match endpoint_cands.into_iter().next() {
        Some((s, v)) if valid_endpoint(&v) => (s, Some(v)),
        Some((s, _)) => (s, None),
        None => (Source::Default, None),
    };
    sources.insert("endpoint", endpoint_src);

    let mut header_cands: Vec<(Source, Vec<(String, String)>)> = Vec::new();
    if let Some(v) = envv("WAKE_OTLP_HEADERS") {
        let h = parse_headers(&v);
        if !h.is_empty() {
            header_cands.push((Source::Env, h));
        }
    }
    for (s, f) in &files {
        if let Some(m) = f.headers.as_ref().filter(|m| !m.is_empty()) {
            header_cands.push((*s, m.iter().map(|(k, v)| (k.clone(), v.clone())).collect()));
        }
    }
    let (headers_src, mut headers) = header_cands
        .into_iter()
        .next()
        .unwrap_or((Source::Default, Vec::new()));
    sources.insert("headers", headers_src);

    // Each key resolves on its own: env > telemetry.json > config.json. Empty strings are absent.
    let key = |env_name: &str, pick: fn(&FileConfig) -> Option<&String>| {
        envv(env_name).map(|v| (Source::Env, v)).or_else(|| {
            files
                .iter()
                .find_map(|(s, f)| pick(f).filter(|v| !v.is_empty()).map(|v| (*s, v.clone())))
        })
    };
    let public_key = key("WAKE_PUBLIC_KEY", |f| f.public_key.as_ref());
    let secret_key = key("WAKE_SECRET_KEY", |f| f.secret_key.as_ref());
    if let Some((s, _)) = &public_key {
        sources.insert("public_key", *s);
    }
    if let Some((s, _)) = &secret_key {
        sources.insert("secret_key", *s);
    }
    // Generic HTTP Basic auth; an explicit Authorization header wins.
    let mut basic_from_keys = false;
    if let (Some((_, pk)), Some((_, sk))) = (&public_key, &secret_key) {
        if !headers
            .iter()
            .any(|(k, _)| k.eq_ignore_ascii_case("authorization"))
        {
            let token = base64_encode(format!("{pk}:{sk}").as_bytes());
            headers.push(("Authorization".to_string(), format!("Basic {token}")));
            basic_from_keys = true;
        }
    }

    let mut id_cands: Vec<(Source, IdentityMode)> = Vec::new();
    if let Some(m) = envv("WAKE_IDENTITY").and_then(|v| IdentityMode::parse(&v)) {
        id_cands.push((Source::Env, m));
    }
    for (s, f) in &files {
        if let Some(m) = f.identity.as_deref().and_then(IdentityMode::parse) {
            id_cands.push((*s, m));
        }
    }
    let (identity_src, identity) = id_cands
        .into_iter()
        .next()
        .unwrap_or((Source::Default, IdentityMode::Email));
    sources.insert("identity", identity_src);

    Config {
        endpoint,
        headers,
        identity,
        identity_salt,
        allow_override,
        basic_from_keys,
        disabled,
        sources,
    }
}
