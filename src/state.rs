//! Per-session state carried between hook runs: types, persistence, a non-blocking lock and GC.

use crate::config::Env;
use crate::event::{is_token, Tokens};
use serde::{Deserialize, Serialize};
use std::collections::BTreeMap;
use std::fs::{self, File};
use std::io::Write;
use std::path::{Path, PathBuf};
use std::time::{Duration, SystemTime};

/// Most request ids kept for usage deduplication.
pub const RECENT_REQUESTS_CAP: usize = 256;

/// A skill (or, later, other) invocation that has started but not yet closed.
#[derive(Serialize, Deserialize, Default, Clone, Debug)]
pub struct OpenInvocation {
    pub span_key: String,
    pub parent_key: Option<String>,
    pub kind: String,
    pub name: String,
    pub invoker: String,
    pub start_ns: u64,
    pub last_ns: u64,
    pub tokens: Tokens,
    pub model: Option<String>,
    pub tool_calls: u64,
    pub tool_errors: u64,
    pub tools: BTreeMap<String, u64>,
    pub pending: Vec<String>,
    pub start_tool_use_id: Option<String>,
    pub outcome: Option<String>,
    pub from_command: bool,
    pub attributed: bool,
    /// A plain user prompt arrived while this invocation was open (ends the slash-command merge window).
    #[serde(default)]
    pub prompt_since_start: bool,
}

/// A background subagent waiting for its `<task-notification>`. Used from Task 10.
#[derive(Serialize, Deserialize, Default, Clone, Debug)]
pub struct PendingAgent {
    pub agent_id: String,
    pub tool_use_id: String,
    pub span_key: String,
    pub parent_key: Option<String>,
    pub name: String,
    pub start_ns: u64,
}

#[derive(Serialize, Deserialize, Default, Clone, Debug)]
pub struct SessionState {
    pub offset: u64,
    pub open: Vec<OpenInvocation>,
    /// Request ids whose usage was already counted, oldest first, capped at [`RECENT_REQUESTS_CAP`].
    pub recent_requests: Vec<String>,
    pub pending_agents: Vec<PendingAgent>,
    pub cwd: Option<String>,
    pub user: Option<String>,
    pub repo: Option<String>,
    /// Newest entry timestamp seen.
    pub last_ns: u64,
    /// Parent for invocations with no enclosing skill (R17): a subagent transcript's own subagent span key.
    #[serde(default)]
    pub root_parent_key: Option<String>,
    /// The session's main transcript, saved on every hook run (R27): staleness also looks at its
    /// mtime, and the stale flush follows subagent transcripts next to it.
    #[serde(default)]
    pub transcript_path: Option<String>,
}

const STALE_AFTER: Duration = Duration::from_secs(30 * 60);
const GC_AFTER: Duration = Duration::from_secs(14 * 24 * 60 * 60);

/// `$XDG_STATE_HOME/wake` when set and non-empty, else `home/.local/state/wake`.
pub fn state_dir(env: &dyn Env, home: &Path) -> PathBuf {
    match env.var("XDG_STATE_HOME") {
        Some(x) if !x.is_empty() => PathBuf::from(x).join("wake"),
        _ => home.join(".local/state/wake"),
    }
}

/// Holds the session lock; dropping it releases the lock.
#[derive(Debug)]
pub struct SessionGuard {
    _file: File,
}

fn sessions_dir(dir: &Path) -> PathBuf {
    dir.join("sessions")
}

/// `create_dir_all` with mode 0700 for every directory it creates.
pub fn create_private_dir(dir: &Path) -> std::io::Result<()> {
    use std::os::unix::fs::DirBuilderExt;
    fs::DirBuilder::new()
        .recursive(true)
        .mode(0o700)
        .create(dir)
}

fn ensure_sessions_dir(dir: &Path) -> std::io::Result<PathBuf> {
    let s = sessions_dir(dir);
    create_private_dir(&s)?;
    Ok(s)
}

fn open_private(path: &Path, truncate: bool) -> std::io::Result<File> {
    use std::os::unix::fs::OpenOptionsExt;
    File::options()
        .write(true)
        .create(true)
        .truncate(truncate)
        .mode(0o600)
        .open(path)
}

/// Non-blocking exclusive lock for a session. `None` if held, on any error, or for a bad id.
pub fn lock_session(dir: &Path, session_id: &str) -> Option<SessionGuard> {
    if !is_token(session_id) {
        return None;
    }
    let s = ensure_sessions_dir(dir).ok()?;
    lock_path(&s.join(format!("{session_id}.lock")), &|| {})
}

/// Locks `path`, then checks the path still names the locked file (GC may have unlinked it).
/// `between` runs after the open and before the lock (a test seam).
fn lock_path(path: &Path, between: &dyn Fn()) -> Option<SessionGuard> {
    use std::os::unix::fs::MetadataExt;
    let file = open_private(path, false).ok()?;
    between();
    file.try_lock().ok()?;
    let (on_disk, held) = (fs::metadata(path).ok()?, file.metadata().ok()?);
    if on_disk.ino() != held.ino() || on_disk.dev() != held.dev() {
        return None;
    }
    Some(SessionGuard { _file: file })
}

/// The saved state, or `None` when missing, unparsable or the id is bad.
pub fn load(dir: &Path, session_id: &str) -> Option<SessionState> {
    if !is_token(session_id) {
        return None;
    }
    let bytes = fs::read(sessions_dir(dir).join(format!("{session_id}.json"))).ok()?;
    serde_json::from_slice(&bytes).ok()
}

/// Atomic save: tmp file in the same directory, then rename.
pub fn save(dir: &Path, session_id: &str, s: &SessionState) -> std::io::Result<()> {
    if !is_token(session_id) {
        return Err(std::io::Error::new(
            std::io::ErrorKind::InvalidInput,
            "bad session id",
        ));
    }
    let sd = ensure_sessions_dir(dir)?;
    let tmp = sd.join(format!("{session_id}.json.{}.tmp", std::process::id()));
    let result = (|| {
        let mut f = open_private(&tmp, true)?;
        f.write_all(&serde_json::to_vec(s)?)?;
        fs::rename(&tmp, sd.join(format!("{session_id}.json")))
    })();
    if result.is_err() {
        let _ = fs::remove_file(&tmp);
    }
    result
}

/// `startup` replays the transcript from the start; every other source (or none) starts at its end.
pub fn init_offset(source: Option<&str>, transcript_len: u64) -> u64 {
    if source == Some("startup") {
        0
    } else {
        transcript_len
    }
}

fn age(path: &Path, now: SystemTime) -> Option<Duration> {
    let m = fs::metadata(path).ok()?.modified().ok()?;
    Some(now.duration_since(m).unwrap_or(Duration::ZERO))
}

fn has_open(s: &SessionState) -> bool {
    !s.open.is_empty() || !s.pending_agents.is_empty()
}

/// Untouched for over 30 minutes, or missing. A transcript path that does not exist says nothing.
fn idle_transcript(s: &SessionState, now: SystemTime) -> bool {
    s.transcript_path
        .as_deref()
        .and_then(|p| age(Path::new(p), now))
        .is_none_or(|a| a > STALE_AFTER)
}

/// Idle sessions (R27): open invocations or pending agents, with the state file and the
/// transcript (when its path is known and it exists) both untouched for over 30 minutes.
/// Each comes with its lock held; sessions whose lock is held elsewhere are skipped.
pub fn stale_open(dir: &Path, now: SystemTime) -> Vec<(String, SessionState, SessionGuard)> {
    let mut out = Vec::new();
    let Ok(rd) = fs::read_dir(sessions_dir(dir)) else {
        return out;
    };
    for entry in rd.flatten() {
        let path = entry.path();
        let Some(id) = path
            .file_name()
            .and_then(|n| n.to_str())
            .and_then(|n| n.strip_suffix(".json"))
        else {
            continue;
        };
        if !is_token(id) || age(&path, now).is_none_or(|a| a <= STALE_AFTER) {
            continue;
        }
        let Some(guard) = lock_session(dir, id) else {
            continue;
        };
        // re-read under the lock: the state may have changed since the listing
        let Some(state) = load(dir, id) else { continue };
        if has_open(&state)
            && age(&path, now).is_some_and(|a| a > STALE_AFTER)
            && idle_transcript(&state, now)
        {
            out.push((id.to_string(), state, guard));
        }
    }
    out
}

/// Delete session files untouched for over 14 days. Keeps states with open work and held locks.
pub fn gc(dir: &Path, now: SystemTime) {
    let Ok(rd) = fs::read_dir(sessions_dir(dir)) else {
        return;
    };
    for entry in rd.flatten() {
        let path = entry.path();
        if age(&path, now).is_none_or(|a| a <= GC_AFTER) {
            continue;
        }
        let name = entry.file_name();
        let Some(name) = name.to_str() else { continue };
        if let Some(id) = name.strip_suffix(".json") {
            if load(dir, id).is_some_and(|s| has_open(&s)) {
                continue;
            }
            let _ = fs::remove_file(&path);
        } else if let Some(id) = name.strip_suffix(".lock") {
            // a live session's lock never refreshes its mtime: keep it while the state is recent
            if age(&path.with_file_name(format!("{id}.json")), now).is_some_and(|a| a <= GC_AFTER) {
                continue;
            }
            if let Ok(f) = File::open(&path) {
                if f.try_lock().is_ok() {
                    let _ = fs::remove_file(&path);
                }
            }
        } else if name.ends_with(".tmp") {
            let _ = fs::remove_file(&path);
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn lock_refused_when_path_unlinked_before_lock() {
        let d = tempfile::tempdir().unwrap();
        let p = d.path().join("x.lock");
        assert!(lock_path(&p, &|| fs::remove_file(&p).unwrap()).is_none());
        assert!(lock_path(&p, &|| {}).is_some());
    }
}
