//! Per-session state carried between hook runs. Persistence (lock, GC, paths) comes in Task 11.

use crate::event::Tokens;
use serde::{Deserialize, Serialize};
use std::collections::BTreeMap;

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
}
