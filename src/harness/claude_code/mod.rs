//! `wake emit` for Claude Code: read the new transcript slice, derive, encode and deliver.
//!
//! Nothing here prints or panics on runtime data; every failure ends the run quietly.

pub mod derive;
pub mod inventory;
pub mod transcript;

use crate::config::{self, Config, Env, IdentityMode};
use crate::event::{is_email, is_token, Context, Invocation, Kind, Tokens, UserId};
use crate::harness::PrimitiveLookup;
use crate::ids::span_id;
use crate::send::{self, Outbox, Transport};
use crate::state::{self, SessionState};
use crate::{identity, otlp, repo};
use derive::{derive, subagent_transcript, SubagentRef};
use inventory::Installed;
use serde::Deserialize;
use std::collections::HashSet;
use std::fs::{self, File};
use std::path::{Path, PathBuf};
use std::time::{SystemTime, UNIX_EPOCH};
use transcript::{read_from, Block, Entry, Role};

const HARNESS: &str = "claude-code";
/// Subagent transcripts are followed this many levels below the main transcript.
const MAX_SUBAGENT_DEPTH: u32 = 3;

pub struct Deps<'a> {
    pub env: &'a dyn Env,
    pub home: PathBuf,
    pub exe_dir: PathBuf,
    pub transport: &'a dyn Transport,
    pub now: SystemTime,
}

#[derive(Clone, Copy, PartialEq, Eq)]
enum Hook {
    SessionStart,
    Stop,
    SessionEnd,
}

#[derive(Deserialize)]
struct Payload {
    session_id: Option<String>,
    transcript_path: Option<String>,
    cwd: Option<String>,
    source: Option<String>,
}

/// What one run works with once the payload is accepted.
struct Run<'a> {
    deps: &'a Deps<'a>,
    cfg: Config,
    dir: PathBuf,
    session_id: String,
    transcript: PathBuf,
    cwd: String,
    source: Option<String>,
    installed: Installed,
    now_ns: u64,
    now_secs: u64,
}

/// Handle one Claude Code hook (`session-start`, `stop` or `session-end`). Never panics on
/// runtime data; anything unexpected ends the run without output.
pub fn run_hook(hook: &str, stdin: &[u8], deps: &Deps) {
    let hook = match hook {
        "session-start" => Hook::SessionStart,
        "stop" => Hook::Stop,
        "session-end" => Hook::SessionEnd,
        _ => return,
    };
    let cfg = config::resolve(deps.env, &deps.exe_dir, &deps.home);
    if cfg.disabled || cfg.endpoint.is_none() {
        return;
    }
    let Ok(p) = serde_json::from_slice::<Payload>(stdin) else {
        return;
    };
    let (Some(session_id), Some(transcript), Some(cwd)) = (p.session_id, p.transcript_path, p.cwd)
    else {
        return;
    };
    if !is_token(&session_id) {
        return;
    }
    let since_epoch = deps.now.duration_since(UNIX_EPOCH).unwrap_or_default();
    let run = Run {
        deps,
        cfg,
        dir: state::state_dir(deps.env, &deps.home),
        installed: Installed::scan(&deps.home, Path::new(&cwd)),
        session_id,
        transcript: PathBuf::from(transcript),
        cwd,
        source: p.source,
        now_ns: u64::try_from(since_epoch.as_nanos()).unwrap_or(u64::MAX),
        now_secs: since_epoch.as_secs(),
    };

    // Every payload is queued and its state saved before anything goes over the network (R20),
    // so a kill during the POST neither loses nor re-derives events.
    run.close_stale();
    state::gc(&run.dir, deps.now);
    if let Some(_guard) = state::lock_session(&run.dir, &run.session_id) {
        match hook {
            Hook::SessionStart => run.session_start(),
            Hook::Stop => run.read_slice(false),
            Hook::SessionEnd => run.read_slice(true),
        }
    }
    run.flush_outbox();
}

impl Run<'_> {
    fn transcript_len(&self) -> u64 {
        fs::metadata(&self.transcript).map(|m| m.len()).unwrap_or(0)
    }

    /// Close and queue the open work of other sessions idle for over 30 minutes (R18, R27),
    /// keeping their offsets. Subagents they close are followed through the saved transcript path.
    fn close_stale(&self) {
        for (id, mut st, _guard) in state::stale_open(&self.dir, self.deps.now) {
            if id == self.session_id {
                continue;
            }
            let d = derive(&id, &[], &mut st, &self.installed, self.now_ns, true);
            let mut closed = d.closed;
            if let Some(tp) = st.transcript_path.clone() {
                follow_subagents(
                    Path::new(&tp),
                    &id,
                    d.subagents,
                    1,
                    &self.installed,
                    self.now_ns,
                    &mut closed,
                );
            }
            let ctx = context(&id, None, &st);
            self.queue(ctx, closed);
            st.open.clear();
            st.pending_agents.clear();
            let _ = state::save(&self.dir, &id, &st);
        }
    }

    /// Saves the state with the transcript path of this run (R27).
    fn save(&self, st: &mut SessionState) {
        st.transcript_path = self.transcript.to_str().map(str::to_string);
        let _ = state::save(&self.dir, &self.session_id, st);
    }

    fn session_start(&self) {
        let fork = self.source.as_deref() == Some("fork");
        let mut st = match state::load(&self.dir, &self.session_id) {
            Some(s) if !fork => s,
            _ => SessionState {
                offset: state::init_offset(self.source.as_deref(), self.transcript_len()),
                ..SessionState::default()
            },
        };
        let inv = inventory_invocation(
            &format!("inv:{}:{}", self.session_id, self.now_secs),
            self.installed.inventory_strings(),
            self.now_ns,
        );
        self.resolve_identity(&mut st);
        self.queue(context(&self.session_id, None, &st), vec![inv]);
        self.save(&mut st);
    }

    /// `stop` and `session-end`: derive the new slice and the subagents it finished.
    fn read_slice(&self, end_of_session: bool) {
        // A missing state never replays history: start at the current end of the transcript.
        let mut st = state::load(&self.dir, &self.session_id).unwrap_or_else(|| SessionState {
            offset: state::init_offset(None, self.transcript_len()),
            ..SessionState::default()
        });
        // A missing or unreadable transcript is an empty slice; the offset stays put.
        let (entries, offset) =
            read_from(&self.transcript, st.offset).unwrap_or((Vec::new(), st.offset));
        st.offset = offset;
        let version = entries.iter().rev().find_map(|e| e.version.clone());
        let d = derive(
            &self.session_id,
            &entries,
            &mut st,
            &self.installed,
            self.now_ns,
            end_of_session,
        );
        drop(entries);
        let mut closed = d.closed;
        follow_subagents(
            &self.transcript,
            &self.session_id,
            d.subagents,
            1,
            &self.installed,
            self.now_ns,
            &mut closed,
        );
        if !closed.is_empty() {
            self.resolve_identity(&mut st);
            self.queue(context(&self.session_id, version, &st), closed);
        }
        self.save(&mut st);
    }

    /// Fill the state's user and repo cache, running `git` only when it is stale.
    fn resolve_identity(&self, st: &mut SessionState) {
        if !identity_stale(st, &self.cwd, self.cfg.identity) {
            return;
        }
        let cwd = Path::new(&self.cwd);
        st.user = match identity::user_id(&self.cfg, identity::email(&self.deps.home, cwd)) {
            UserId::Email(s) | UserId::Hash(s) => Some(s),
            UserId::Unknown => None,
        };
        st.repo = repo::repo(cwd);
        st.cwd = Some(self.cwd.clone());
    }

    fn outbox(&self) -> Outbox {
        Outbox {
            path: self.dir.join("outbox.ndjson"),
        }
    }

    /// Sanitise, encode and append to the shared outbox, under the outbox lock (R11).
    fn queue(&self, ctx: Context, invs: Vec<Invocation>) {
        let invs: Vec<Invocation> = invs.into_iter().filter_map(Invocation::sanitized).collect();
        if invs.is_empty() {
            return;
        }
        let payload = otlp::encode(&ctx.sanitized(), &invs);
        let _lock = lock_outbox(&self.dir);
        send::enqueue(&self.outbox(), &payload, self.now_secs);
    }

    /// One flush per run, under the outbox lock. An empty outbox makes no network call.
    fn flush_outbox(&self) {
        if !self.outbox().path.exists() {
            return;
        }
        let _lock = lock_outbox(&self.dir);
        send::flush(
            self.deps.transport,
            &self.cfg,
            &self.outbox(),
            self.now_secs,
        );
    }
}

/// Exclusive blocking lock on `<state dir>/outbox.lock`; released when the file drops.
/// `None` on any error, in which case the outbox is used unlocked.
fn lock_outbox(dir: &Path) -> Option<File> {
    use std::os::unix::fs::OpenOptionsExt;
    fs::create_dir_all(dir).ok()?;
    let f = File::options()
        .write(true)
        .create(true)
        .truncate(false)
        .mode(0o600)
        .open(dir.join("outbox.lock"))
        .ok()?;
    f.lock().ok()?;
    Some(f)
}

/// The cache is stale when it was filled for another `cwd` (or never), or for another identity mode.
fn identity_stale(st: &SessionState, cwd: &str, mode: IdentityMode) -> bool {
    if st.cwd.as_deref() != Some(cwd) {
        return true;
    }
    match (&st.user, mode) {
        (Some(u), IdentityMode::Anonymous) => is_email(u),
        (Some(u), IdentityMode::Email) => !is_email(u),
        (None, _) => false,
    }
}

/// The cached user: an email, a 64-hex pseudonym, or unknown.
fn cached_user(user: Option<&str>) -> UserId {
    match user {
        Some(u) if is_email(u) => UserId::Email(u.to_string()),
        Some(u) if u.len() == 64 && u.bytes().all(|c| c.is_ascii_hexdigit()) => {
            UserId::Hash(u.to_string())
        }
        _ => UserId::Unknown,
    }
}

fn context(session_id: &str, harness_version: Option<String>, st: &SessionState) -> Context {
    Context {
        harness: HARNESS,
        harness_version,
        session_id: session_id.to_string(),
        user: cached_user(st.user.as_deref()),
        repo: st.repo.clone(),
    }
}

fn inventory_invocation(span_key: &str, inventory: Vec<String>, now_ns: u64) -> Invocation {
    Invocation {
        span_id: span_id(span_key),
        parent_span_id: None,
        start_ns: now_ns,
        end_ns: now_ns,
        kind: Kind::Inventory,
        name: "inventory".to_string(),
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
        inventory,
    }
}

/// What a subagent's own transcript says about the subagent span.
struct TranscriptUsage {
    /// Assistant usage deduplicated by request id.
    tokens: Tokens,
    /// Number of tool_use blocks.
    tool_calls: u64,
    /// Model of the last assistant entry with usage.
    model: Option<String>,
}

fn transcript_usage(entries: &[Entry]) -> TranscriptUsage {
    let mut u = TranscriptUsage {
        tokens: Tokens::default(),
        tool_calls: 0,
        model: None,
    };
    let mut seen = HashSet::new();
    for e in entries.iter().filter(|e| e.role == Role::Assistant) {
        u.tool_calls += e
            .blocks
            .iter()
            .filter(|b| matches!(b, Block::ToolUse { .. }))
            .count() as u64;
        if e.usage.is_some() && e.model.is_some() {
            u.model = e.model.clone();
        }
        let fresh = match &e.request_id {
            Some(r) => seen.insert(r.as_str()),
            None => true,
        };
        if let (true, Some(usage)) = (fresh, &e.usage) {
            u.tokens.add(usage);
        }
    }
    u
}

/// Field-wise `a - b`, saturating at zero; a field unset in `a` stays unset.
fn minus(a: &Tokens, b: &Tokens) -> Tokens {
    let f = |x: Option<u64>, y: Option<u64>| x.map(|x| x.saturating_sub(y.unwrap_or(0)));
    Tokens {
        input: f(a.input, b.input),
        output: f(a.output, b.output),
        cache_read: f(a.cache_read, b.cache_read),
        cache_creation: f(a.cache_creation, b.cache_creation),
    }
}

/// Derive each finished subagent's own transcript (R17) and take its tool calls, model and tokens
/// from it (R16, R29). The subagent span's tokens are the transcript total minus what the skills
/// derived from that transcript already carry (both request-id deduplicated, so disjoint).
/// `depth` is the depth of the transcripts derived here; the main transcript is depth 0.
fn follow_subagents(
    transcript: &Path,
    session_id: &str,
    refs: Vec<SubagentRef>,
    depth: u32,
    lookup: &dyn PrimitiveLookup,
    now_ns: u64,
    out: &mut Vec<Invocation>,
) {
    for r in refs {
        let path = subagent_transcript(transcript, session_id, &r.agent_id);
        let Ok((entries, _)) = read_from(&path, 0) else {
            continue;
        };
        let usage = (!entries.is_empty()).then(|| transcript_usage(&entries));
        let id = span_id(&r.span_key);
        let mut st = SessionState {
            root_parent_key: Some(r.span_key),
            ..SessionState::default()
        };
        let d = derive(session_id, &entries, &mut st, lookup, now_ns, true);
        drop(entries);
        // An empty transcript says nothing: keep what derive produced.
        if let Some(u) = usage {
            let mut in_skills = Tokens::default();
            for i in d.closed.iter().filter(|i| i.kind == Kind::Skill) {
                in_skills.add(&i.tokens);
            }
            if let Some(inv) = out
                .iter_mut()
                .find(|i| i.kind == Kind::Subagent && i.span_id == id)
            {
                inv.tokens = minus(&u.tokens, &in_skills);
                inv.tool_calls = Some(u.tool_calls);
                if u.model.is_some() {
                    inv.model = u.model;
                }
            }
        }
        out.extend(d.closed);
        if depth < MAX_SUBAGENT_DEPTH {
            follow_subagents(
                transcript,
                session_id,
                d.subagents,
                depth + 1,
                lookup,
                now_ns,
                out,
            );
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn st(cwd: Option<&str>, user: Option<&str>) -> SessionState {
        SessionState {
            cwd: cwd.map(String::from),
            user: user.map(String::from),
            ..SessionState::default()
        }
    }

    #[test]
    fn identity_resolved_once_per_cwd() {
        let mode = IdentityMode::Email;
        assert!(identity_stale(&st(None, None), "/w", mode));
        assert!(!identity_stale(&st(Some("/w"), Some("a@b.c")), "/w", mode));
        // An unknown user is cached too, so git is not rerun on every Stop.
        assert!(!identity_stale(&st(Some("/w"), None), "/w", mode));
        assert!(identity_stale(
            &st(Some("/w"), Some("a@b.c")),
            "/other",
            mode
        ));
    }

    #[test]
    fn identity_mode_change_invalidates_the_cache() {
        let hash = "a".repeat(64);
        let email_cached = st(Some("/w"), Some("a@b.c"));
        let hash_cached = st(Some("/w"), Some(&hash));
        assert!(identity_stale(&email_cached, "/w", IdentityMode::Anonymous));
        assert!(identity_stale(&hash_cached, "/w", IdentityMode::Email));
        assert!(!identity_stale(&hash_cached, "/w", IdentityMode::Anonymous));
    }

    #[test]
    fn minus_saturates_and_keeps_unset_fields() {
        let a = Tokens {
            input: Some(10),
            output: Some(1),
            cache_read: None,
            cache_creation: Some(5),
        };
        let b = Tokens {
            input: Some(4),
            output: Some(3),
            cache_read: Some(7),
            cache_creation: None,
        };
        let d = minus(&a, &b);
        assert_eq!(d.input, Some(6));
        assert_eq!(d.output, Some(0));
        assert_eq!(d.cache_read, None);
        assert_eq!(d.cache_creation, Some(5));
    }

    #[test]
    fn cached_user_kinds() {
        assert_eq!(
            cached_user(Some("a@b.c")),
            UserId::Email("a@b.c".to_string())
        );
        let hash = "0f".repeat(32);
        assert_eq!(cached_user(Some(&hash)), UserId::Hash(hash.clone()));
        assert_eq!(cached_user(Some("nope")), UserId::Unknown);
        assert_eq!(cached_user(None), UserId::Unknown);
    }
}
