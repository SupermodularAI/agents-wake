//! Canonical event model and the string allowlist every outgoing attribute passes.

use serde::{Deserialize, Serialize};

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Kind {
    Skill,
    Subagent,
    McpTool,
    Command,
    Inventory,
}

impl Kind {
    pub fn as_str(&self) -> &'static str {
        match self {
            Kind::Skill => "skill",
            Kind::Subagent => "subagent",
            Kind::McpTool => "mcp_tool",
            Kind::Command => "command",
            Kind::Inventory => "inventory",
        }
    }
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Invoker {
    User,
    Model,
    Nested,
}

impl Invoker {
    pub fn as_str(&self) -> &'static str {
        match self {
            Invoker::User => "user",
            Invoker::Model => "model",
            Invoker::Nested => "nested",
        }
    }
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Outcome {
    Ok,
    Error,
    Interrupted,
}

impl Outcome {
    pub fn as_str(&self) -> &'static str {
        match self {
            Outcome::Ok => "ok",
            Outcome::Error => "error",
            Outcome::Interrupted => "interrupted",
        }
    }
}

#[derive(Default, Clone, Copy, PartialEq, Debug, Serialize, Deserialize)]
pub struct Tokens {
    pub input: Option<u64>,
    pub output: Option<u64>,
    pub cache_read: Option<u64>,
    pub cache_creation: Option<u64>,
}

fn add_opt(a: &mut Option<u64>, b: Option<u64>) {
    if let Some(x) = b {
        *a = Some(a.unwrap_or(0).saturating_add(x));
    }
}

impl Tokens {
    /// Field-wise sum. `None + Some(x)` is `Some(x)`; `None + None` stays `None`.
    pub fn add(&mut self, other: &Tokens) {
        add_opt(&mut self.input, other.input);
        add_opt(&mut self.output, other.output);
        add_opt(&mut self.cache_read, other.cache_read);
        add_opt(&mut self.cache_creation, other.cache_creation);
    }
}

#[derive(Debug, Clone, PartialEq)]
pub struct McpAgg {
    pub server: String,
    pub calls: u64,
    pub errors: u64,
    pub total_duration_ms: u64,
    pub max_duration_ms: u64,
}

#[derive(Debug, Clone, PartialEq)]
pub struct Invocation {
    pub span_id: [u8; 8],
    pub parent_span_id: Option<[u8; 8]>,
    pub start_ns: u64,
    pub end_ns: u64,
    pub kind: Kind,
    pub name: String,
    pub version: Option<String>,
    pub plugin: Option<String>,
    pub marketplace: Option<String>,
    pub context_tokens: Option<u64>,
    pub invoker: Option<Invoker>,
    pub outcome: Option<Outcome>,
    pub tool_calls: Option<u64>,
    pub tool_errors: Option<u64>,
    pub tools: Vec<(String, u64)>,
    pub mcp: Option<McpAgg>,
    pub model: Option<String>,
    pub tokens: Tokens,
    /// Only for `Kind::Inventory`.
    pub inventory: Vec<String>,
}

#[derive(Debug, Clone, PartialEq)]
pub enum UserId {
    Email(String),
    Hash(String),
    Unknown,
}

#[derive(Debug, Clone, PartialEq)]
pub struct Context {
    pub harness: &'static str,
    pub harness_version: Option<String>,
    pub session_id: String,
    pub user: UserId,
    pub repo: Option<String>,
}

/// `^[A-Za-z0-9][A-Za-z0-9._:@+_-]{0,127}$`
pub fn is_token(s: &str) -> bool {
    let b = s.as_bytes();
    if b.is_empty() || b.len() > 128 || !b[0].is_ascii_alphanumeric() {
        return false;
    }
    b[1..]
        .iter()
        .all(|c| c.is_ascii_alphanumeric() || matches!(c, b'.' | b'_' | b':' | b'@' | b'+' | b'-'))
}

/// 2..=8 `/`-separated segments, each a token, total at most 255 bytes, no `..`.
pub fn is_repo(s: &str) -> bool {
    if s.len() > 255 || s.contains("..") {
        return false;
    }
    let n = s.split('/').count();
    (2..=8).contains(&n) && s.split('/').all(is_token)
}

/// One `@`, non-empty local part `[A-Za-z0-9._%+-]` and domain `[A-Za-z0-9.-]`, at most 254 bytes.
pub fn is_email(s: &str) -> bool {
    if s.len() > 254 {
        return false;
    }
    let mut parts = s.split('@');
    let (Some(local), Some(domain), None) = (parts.next(), parts.next(), parts.next()) else {
        return false;
    };
    !local.is_empty()
        && !domain.is_empty()
        && local
            .bytes()
            .all(|c| c.is_ascii_alphanumeric() || matches!(c, b'.' | b'_' | b'%' | b'+' | b'-'))
        && domain
            .bytes()
            .all(|c| c.is_ascii_alphanumeric() || matches!(c, b'.' | b'-'))
}

fn keep_token(o: Option<String>) -> Option<String> {
    o.filter(|s| is_token(s))
}

impl Invocation {
    /// `None` if the name fails `is_token`; otherwise drops every failing optional string,
    /// tools entry and inventory entry.
    pub fn sanitized(mut self) -> Option<Invocation> {
        if !is_token(&self.name) {
            return None;
        }
        self.version = keep_token(self.version);
        self.plugin = keep_token(self.plugin);
        self.marketplace = keep_token(self.marketplace);
        self.model = keep_token(self.model);
        self.tools.retain(|(n, _)| is_token(n));
        self.inventory.retain(|n| is_token(n));
        if self.mcp.as_ref().is_some_and(|m| !is_token(&m.server)) {
            self.mcp = None;
        }
        Some(self)
    }
}

impl Context {
    pub fn sanitized(mut self) -> Context {
        self.harness_version = keep_token(self.harness_version);
        self.repo = self.repo.filter(|s| is_repo(s));
        self.user = match self.user {
            UserId::Email(e) if is_email(&e) => UserId::Email(e),
            UserId::Hash(h)
                if h.len() == 64 && h.bytes().all(|c| matches!(c, b'0'..=b'9' | b'a'..=b'f')) =>
            {
                UserId::Hash(h)
            }
            _ => UserId::Unknown,
        };
        self
    }
}
