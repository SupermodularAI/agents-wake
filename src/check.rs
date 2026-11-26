//! `wake check`: print the resolved config and send one test event.
//!
//! Never prints header values, the endpoint path or query, or the identity itself.

use crate::config::{self, IdentityMode};
use crate::event::{Context, Invocation, Kind, Tokens, UserId};
use crate::harness::claude_code::Deps;
use crate::ids::span_id;
use crate::send::SendError;
use crate::{identity, otlp};
use std::io::Write;
use std::time::UNIX_EPOCH;

/// Print the resolved config and post one test event directly (not via the outbox).
/// Returns the exit code: 0 if sent (or disabled), 1 otherwise.
pub fn run(deps: &Deps, out: &mut dyn Write) -> i32 {
    let cfg = config::resolve(deps.env, &deps.exe_dir, &deps.home);
    let src = |k: &str| cfg.sources.get(k).map_or("default", |s| s.as_str());

    let endpoint = cfg.endpoint.as_deref().and_then(config::origin);
    match &endpoint {
        Some(o) => {
            let _ = writeln!(out, "endpoint: {o} (from {})", src("endpoint"));
        }
        None => {
            let _ = writeln!(out, "endpoint: none (from {})", src("endpoint"));
        }
    }
    let _ = writeln!(
        out,
        "headers: {} set (from {})",
        cfg.headers.len(),
        src("headers")
    );

    let cwd = std::env::current_dir().unwrap_or_else(|_| deps.home.clone());
    let user = identity::user_id(&cfg, identity::email(&deps.home, &cwd));
    let mode = match cfg.identity {
        IdentityMode::Email => "email",
        IdentityMode::Anonymous => "anonymous",
    };
    let found = match user {
        UserId::Email(_) => "email found",
        UserId::Hash(_) => "hash",
        UserId::Unknown => "unknown",
    };
    let _ = writeln!(
        out,
        "identity: {mode} (from {}) \u{2014} user: {found}",
        src("identity")
    );
    let _ = writeln!(out, "disabled: {}", if cfg.disabled { "yes" } else { "no" });

    if cfg.disabled {
        let _ = writeln!(out, "test event: skipped (disabled)");
        return 0;
    }
    let Some(url) = cfg.endpoint.as_deref() else {
        let _ = writeln!(out, "test event: skipped (no endpoint)");
        return 1;
    };

    let now_secs = deps
        .now
        .duration_since(UNIX_EPOCH)
        .map_or(0, |d| d.as_secs());
    let now_ns = now_secs.saturating_mul(1_000_000_000);
    let session_id = format!("check-{now_secs}");
    let ctx = Context {
        harness: "claude-code",
        harness_version: None,
        session_id: session_id.clone(),
        user,
        repo: None,
    };
    let inv = Invocation {
        span_id: span_id(&format!("{session_id}:check")),
        parent_span_id: None,
        start_ns: now_ns,
        end_ns: now_ns,
        kind: Kind::Command,
        name: "check".to_string(),
        version: None,
        plugin: None,
        marketplace: None,
        context_tokens: None,
        invoker: None,
        outcome: None,
        tool_calls: None,
        tool_errors: None,
        tools: Vec::new(),
        mcp: None,
        model: None,
        tokens: Tokens::default(),
        inventory: Vec::new(),
    };
    let body = otlp::encode(&ctx, &[inv]).to_string();
    match deps.transport.post(url, &cfg.headers, body.as_bytes()) {
        Ok(()) => {
            let _ = writeln!(out, "test event: sent");
            0
        }
        Err(e) => {
            let reason = match e {
                SendError::Network => "network".to_string(),
                SendError::Status(c) => format!("http {c}"),
            };
            let _ = writeln!(out, "test event: failed ({reason})");
            1
        }
    }
}
