//! Spec §5.5: turn a slice of transcript entries into closed invocations.
//!
//! The harness's `attributionSkill` decides which skill an entry belongs to. This module only
//! decides where an invocation starts and ends, carrying open ones across turns in
//! [`SessionState`].

use crate::event::{Invocation, Invoker, Kind, McpAgg, Outcome, Tokens};
use crate::harness::claude_code::transcript::{AgentResult, Block, Entry, Role, TaskNotification};
use crate::harness::{PrimitiveInfo, PrimitiveLookup};
use crate::ids::span_id;
use crate::state::{OpenInvocation, PendingAgent, SessionState, RECENT_REQUESTS_CAP};
use std::collections::HashMap;
use std::path::{Path, PathBuf};

/// An open invocation with no attributed entry for this long is closed.
pub const IDLE_CLOSE_NS: u64 = 30 * 60 * 1_000_000_000;

const SKILL: &str = "skill";

/// A subagent whose own transcript should be derived next.
#[derive(Debug, Clone, PartialEq)]
pub struct SubagentRef {
    pub agent_id: String,
    /// The subagent invocation's span key (`agent:<agent_id>`).
    pub span_key: String,
    /// The subagent invocation's parent (the skill that launched it), if any.
    pub parent_key: Option<String>,
}

/// `<dir of transcript>/<session_id>/subagents/agent-<agent_id>.jsonl`
pub fn subagent_transcript(transcript_path: &Path, session_id: &str, agent_id: &str) -> PathBuf {
    transcript_path
        .parent()
        .unwrap_or_else(|| Path::new(""))
        .join(session_id)
        .join("subagents")
        .join(format!("agent-{agent_id}.jsonl"))
}

/// An `Agent` tool_use seen in this slice, waiting for its result.
struct AgentUse {
    ts_ns: u64,
    subagent_type: Option<String>,
    parent_key: Option<String>,
}

/// One MCP aggregate: (server, tool, enclosing skill) within one `derive` call.
struct McpAccum {
    server: String,
    tool: String,
    parent_key: Option<String>,
    first_id: String,
    start_ns: u64,
    end_ns: u64,
    agg: McpAgg,
}

/// What lives only for one `derive` call.
#[derive(Default)]
struct SliceScratch {
    agent_uses: HashMap<String, AgentUse>,
    mcp: Vec<McpAccum>,
    /// MCP tool_use id -> (index into `mcp`, tool_use ts).
    mcp_uses: HashMap<String, (usize, u64)>,
}

#[derive(Debug, Clone, PartialEq, Default)]
pub struct Derived {
    /// Invocations closed during this call, in close order.
    pub closed: Vec<Invocation>,
    pub subagents: Vec<SubagentRef>,
}

/// Apply the §5.5 rules to `entries` in order, updating `state`.
///
/// With `end_of_session`, everything still open is closed at the end; otherwise the idle rule
/// is applied against `now_ns`.
pub fn derive(
    session_id: &str,
    entries: &[Entry],
    state: &mut SessionState,
    lookup: &dyn PrimitiveLookup,
    now_ns: u64,
    end_of_session: bool,
) -> Derived {
    let mut out = Derived::default();
    // Start of the current turn: the last plain user prompt, else the start of the slice.
    let mut turn_start = entries.first().map(|e| e.ts_ns).unwrap_or(0);
    let mut scratch = SliceScratch::default();

    for e in entries {
        close_idle(state, e.ts_ns, lookup, &mut out);
        state.last_ns = state.last_ns.max(e.ts_ns);
        match e.role {
            Role::User => {
                if let Some(cmd) = &e.command_name {
                    if lookup.skill(cmd).is_some() {
                        start_from_command(session_id, cmd, e.ts_ns, state, lookup, &mut out);
                    } else {
                        let parent = state.root_parent_key.clone();
                        out.closed.push(command(session_id, cmd, e.ts_ns, parent));
                    }
                }
                apply_results(e, state);
                apply_mcp_results(e, &mut scratch);
                if let Some(a) = &e.agent_result {
                    apply_agent_result(e, a, &mut scratch, state, lookup, &mut out);
                }
                if let Some(n) = &e.task_notification {
                    close_notified(n, e.ts_ns, state, lookup, &mut out);
                }
                if e.interrupted {
                    mark_interrupted(state, turn_start);
                } else if let Some(cmd) = &e.command_name {
                    // R15: a command is a new prompt, except for the skill it (re)starts.
                    turn_start = e.ts_ns;
                    for o in &mut state.open {
                        if o.name != *cmd {
                            o.prompt_since_start = true;
                        }
                    }
                } else if is_plain_prompt(e) {
                    turn_start = e.ts_ns;
                    for o in &mut state.open {
                        o.prompt_since_start = true;
                    }
                }
            }
            Role::Assistant => {
                start_from_tool_uses(e, state, lookup, &mut out);
                record_tool_uses(e, state, &mut scratch);
                apply_belonging(e, state);
            }
            Role::Other => {}
        }
    }

    if end_of_session {
        while !state.open.is_empty() {
            let o = state.open.remove(0);
            out.closed.push(close(o, lookup));
        }
        let end_ns = state.last_ns;
        for p in std::mem::take(&mut state.pending_agents) {
            close_pending(p, end_ns, Outcome::Interrupted, lookup, &mut out);
        }
    } else {
        close_idle(state, now_ns, lookup, &mut out);
    }
    out.closed
        .extend(scratch.mcp.into_iter().map(mcp_invocation));
    out
}

fn is_plain_prompt(e: &Entry) -> bool {
    !e.is_meta
        && e.command_name.is_none()
        && e.task_notification.is_none()
        && !e
            .blocks
            .iter()
            .any(|b| matches!(b, Block::ToolResult { .. }))
}

fn close_idle(
    state: &mut SessionState,
    ts_ns: u64,
    lookup: &dyn PrimitiveLookup,
    out: &mut Derived,
) {
    let mut i = 0;
    while i < state.open.len() {
        if ts_ns > state.open[i].last_ns.saturating_add(IDLE_CLOSE_NS) {
            let o = state.open.remove(i);
            out.closed.push(close(o, lookup));
        } else {
            i += 1;
        }
    }
}

/// Restart rule: close any open skill invocation with this name.
fn close_same_name(
    state: &mut SessionState,
    name: &str,
    lookup: &dyn PrimitiveLookup,
    out: &mut Derived,
) {
    let mut i = 0;
    while i < state.open.len() {
        if state.open[i].kind == SKILL && state.open[i].name == name {
            let o = state.open.remove(i);
            out.closed.push(close(o, lookup));
        } else {
            i += 1;
        }
    }
}

/// Index of the most recently started open skill with this name.
fn find_open_skill(state: &SessionState, name: &str) -> Option<usize> {
    state
        .open
        .iter()
        .rposition(|o| o.kind == SKILL && o.name == name)
}

fn new_skill(span_key: String, name: &str, invoker: Invoker, ts_ns: u64) -> OpenInvocation {
    OpenInvocation {
        span_key,
        kind: SKILL.to_string(),
        name: name.to_string(),
        invoker: invoker.as_str().to_string(),
        start_ns: ts_ns,
        last_ns: ts_ns,
        ..OpenInvocation::default()
    }
}

fn start_from_command(
    session_id: &str,
    name: &str,
    ts_ns: u64,
    state: &mut SessionState,
    lookup: &dyn PrimitiveLookup,
    out: &mut Derived,
) {
    close_same_name(state, name, lookup, out);
    let mut o = new_skill(
        format!("cmd:{session_id}:{ts_ns}"),
        name,
        Invoker::User,
        ts_ns,
    );
    o.from_command = true;
    o.parent_key = state.root_parent_key.clone();
    state.open.push(o);
}

fn start_from_tool_uses(
    e: &Entry,
    state: &mut SessionState,
    lookup: &dyn PrimitiveLookup,
    out: &mut Derived,
) {
    for b in &e.blocks {
        let Block::ToolUse {
            id,
            name,
            skill: Some(skill),
            ..
        } = b
        else {
            continue;
        };
        if name != "Skill" {
            continue;
        }
        // Merge (R12): a slash command opened this skill in the same turn and no Skill tool_use
        // has claimed it yet; attributed entries before or after the tool_use both merge.
        if let Some(i) = find_open_skill(state, skill) {
            let o = &mut state.open[i];
            if o.from_command && o.start_tool_use_id.is_none() && !o.prompt_since_start {
                o.start_tool_use_id = Some(id.clone());
                continue;
            }
        }
        // Nesting: the start entry is attributed to a different open skill.
        let parent_key = e
            .attribution_skill
            .as_deref()
            .filter(|outer| *outer != skill.as_str())
            .and_then(|outer| find_open_skill(state, outer))
            .map(|i| state.open[i].span_key.clone());
        close_same_name(state, skill, lookup, out);
        let invoker = if parent_key.is_some() {
            Invoker::Nested
        } else {
            Invoker::Model
        };
        let mut o = new_skill(id.clone(), skill, invoker, e.ts_ns);
        o.parent_key = parent_key.or_else(|| state.root_parent_key.clone());
        o.start_tool_use_id = Some(id.clone());
        state.open.push(o);
    }
}

/// Runs after the entry's starts (R13): if this entry restarts its own attributed skill, its usage
/// goes to the new invocation.
fn apply_belonging(e: &Entry, state: &mut SessionState) {
    let Some(i) = e
        .attribution_skill
        .as_deref()
        .and_then(|name| find_open_skill(state, name))
    else {
        return;
    };
    let fresh = match &e.request_id {
        Some(r) if state.recent_requests.contains(r) => false,
        Some(r) => {
            state.recent_requests.push(r.clone());
            if state.recent_requests.len() > RECENT_REQUESTS_CAP {
                let excess = state.recent_requests.len() - RECENT_REQUESTS_CAP;
                state.recent_requests.drain(..excess);
            }
            true
        }
        None => true,
    };
    let o = &mut state.open[i];
    if fresh {
        if let Some(u) = &e.usage {
            o.tokens.add(u);
        }
    }
    if e.model.is_some() {
        o.model = e.model.clone();
    }
    o.last_ns = o.last_ns.max(e.ts_ns);
    o.attributed = true;
    for b in &e.blocks {
        if let Block::ToolUse { id, name, .. } = b {
            // R15: the invocation's own start tool_use is not one of its tool calls.
            if o.start_tool_use_id.as_deref() == Some(id.as_str()) {
                continue;
            }
            o.tool_calls += 1;
            *o.tools.entry(name.clone()).or_insert(0) += 1;
            o.pending.push(id.clone());
        }
    }
}

fn apply_results(e: &Entry, state: &mut SessionState) {
    for b in &e.blocks {
        let Block::ToolResult {
            tool_use_id,
            is_error,
        } = b
        else {
            continue;
        };
        for o in &mut state.open {
            if let Some(p) = o.pending.iter().position(|id| id == tool_use_id) {
                o.pending.remove(p);
                if *is_error {
                    o.tool_errors += 1;
                }
            }
            if *is_error && o.start_tool_use_id.as_deref() == Some(tool_use_id.as_str()) {
                o.outcome = Some(Outcome::Error.as_str().to_string());
            }
        }
    }
}

/// R14: `last_ns` starts at `start_ns`, so a skill started but not yet attributed in this turn is
/// marked interrupted too.
fn mark_interrupted(state: &mut SessionState, turn_start: u64) {
    for o in &mut state.open {
        if o.kind == SKILL
            && o.last_ns >= turn_start
            && o.outcome.as_deref() != Some(Outcome::Error.as_str())
        {
            o.outcome = Some(Outcome::Interrupted.as_str().to_string());
        }
    }
}

fn parse_invoker(s: &str) -> Option<Invoker> {
    match s {
        "user" => Some(Invoker::User),
        "model" => Some(Invoker::Model),
        "nested" => Some(Invoker::Nested),
        _ => None,
    }
}

fn parse_outcome(s: Option<&str>) -> Outcome {
    match s {
        Some("error") => Outcome::Error,
        Some("interrupted") => Outcome::Interrupted,
        _ => Outcome::Ok,
    }
}

fn parse_kind(s: &str) -> Kind {
    match s {
        "subagent" => Kind::Subagent,
        "mcp_tool" => Kind::McpTool,
        "command" => Kind::Command,
        "inventory" => Kind::Inventory,
        _ => Kind::Skill,
    }
}

/// Installed-primitive metadata for an invocation of this kind and name.
fn metadata(kind: Kind, name: &str, lookup: &dyn PrimitiveLookup) -> Option<PrimitiveInfo> {
    match kind {
        Kind::Skill => lookup.skill(name),
        Kind::Subagent => lookup.agent(name),
        _ => None,
    }
}

/// An invocation with no metadata, counts or tokens; callers fill in the rest.
fn bare(span_key: &str, parent_key: Option<&str>, kind: Kind, name: String) -> Invocation {
    Invocation {
        span_id: span_id(span_key),
        parent_span_id: parent_key.map(span_id),
        start_ns: 0,
        end_ns: 0,
        kind,
        name,
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
    }
}

fn with_metadata(mut i: Invocation, lookup: &dyn PrimitiveLookup) -> Invocation {
    if let Some(info) = metadata(i.kind, &i.name, lookup) {
        i.version = info.version;
        i.plugin = info.plugin;
        i.marketplace = info.marketplace;
        i.context_tokens = info.context_tokens;
    }
    i
}

/// A slash command that is not a skill: one closed, instantaneous event.
fn command(session_id: &str, name: &str, ts_ns: u64, parent_key: Option<String>) -> Invocation {
    let mut i = bare(
        &format!("cmd:{session_id}:{ts_ns}"),
        parent_key.as_deref(),
        Kind::Command,
        name.to_string(),
    );
    i.start_ns = ts_ns;
    i.end_ns = ts_ns;
    i.invoker = Some(Invoker::User);
    i
}

/// `mcp__<server>__<tool>`, split at the first `__` after the prefix.
fn parse_mcp(name: &str) -> Option<(&str, &str)> {
    let (server, tool) = name.strip_prefix("mcp__")?.split_once("__")?;
    (!server.is_empty() && !tool.is_empty()).then_some((server, tool))
}

/// Span key of the open skill this entry is attributed to, else the root parent (R17).
fn attributed_parent(e: &Entry, state: &SessionState) -> Option<String> {
    e.attribution_skill
        .as_deref()
        .and_then(|name| find_open_skill(state, name))
        .map(|i| state.open[i].span_key.clone())
        .or_else(|| state.root_parent_key.clone())
}

/// Note `Agent` and MCP tool_uses so their results can be matched by tool_use id.
fn record_tool_uses(e: &Entry, state: &SessionState, scratch: &mut SliceScratch) {
    for b in &e.blocks {
        let Block::ToolUse {
            id,
            name,
            subagent_type,
            ..
        } = b
        else {
            continue;
        };
        if name == "Agent" || subagent_type.is_some() {
            scratch.agent_uses.insert(
                id.clone(),
                AgentUse {
                    ts_ns: e.ts_ns,
                    subagent_type: subagent_type.clone(),
                    parent_key: attributed_parent(e, state),
                },
            );
        } else if let Some((server, tool)) = parse_mcp(name) {
            let parent_key = attributed_parent(e, state);
            let idx =
                match scratch.mcp.iter().position(|m| {
                    m.server == server && m.tool == tool && m.parent_key == parent_key
                }) {
                    Some(i) => i,
                    None => {
                        scratch.mcp.push(McpAccum {
                            server: server.to_string(),
                            tool: tool.to_string(),
                            parent_key,
                            first_id: id.clone(),
                            start_ns: e.ts_ns,
                            end_ns: e.ts_ns,
                            agg: McpAgg {
                                server: server.to_string(),
                                calls: 0,
                                errors: 0,
                                total_duration_ms: 0,
                                max_duration_ms: 0,
                            },
                        });
                        scratch.mcp.len() - 1
                    }
                };
            scratch.mcp[idx].agg.calls += 1;
            scratch.mcp_uses.insert(id.clone(), (idx, e.ts_ns));
        }
    }
}

fn apply_mcp_results(e: &Entry, scratch: &mut SliceScratch) {
    for b in &e.blocks {
        let Block::ToolResult {
            tool_use_id,
            is_error,
        } = b
        else {
            continue;
        };
        let Some((idx, use_ns)) = scratch.mcp_uses.remove(tool_use_id) else {
            continue;
        };
        let m = &mut scratch.mcp[idx];
        let ms = e.ts_ns.saturating_sub(use_ns) / 1_000_000;
        m.agg.total_duration_ms = m.agg.total_duration_ms.saturating_add(ms);
        m.agg.max_duration_ms = m.agg.max_duration_ms.max(ms);
        if *is_error {
            m.agg.errors += 1;
        }
        m.end_ns = m.end_ns.max(e.ts_ns);
    }
}

fn mcp_invocation(m: McpAccum) -> Invocation {
    let mut i = bare(
        &format!("mcp:{}", m.first_id),
        m.parent_key.as_deref(),
        Kind::McpTool,
        format!("{}__{}", m.server, m.tool),
    );
    i.start_ns = m.start_ns;
    i.end_ns = m.end_ns;
    i.mcp = Some(m.agg);
    i
}

fn subagent_invocation(
    span_key: &str,
    parent_key: Option<&str>,
    name: String,
    start_ns: u64,
    end_ns: u64,
    outcome: Outcome,
    lookup: &dyn PrimitiveLookup,
) -> Invocation {
    let mut i = with_metadata(bare(span_key, parent_key, Kind::Subagent, name), lookup);
    i.start_ns = start_ns;
    i.end_ns = end_ns;
    i.outcome = Some(outcome);
    i
}

/// `agent:<agent_id>:<tool_use_id>`: a continued subagent keeps its agent id, so the launching
/// tool_use id keeps each run's span distinct. Falls back to `agent:<agent_id>`.
fn agent_span_key(agent_id: &str, tool_use_id: Option<&str>) -> String {
    match tool_use_id {
        Some(id) => format!("agent:{agent_id}:{id}"),
        None => format!("agent:{agent_id}"),
    }
}

/// An `Agent` tool_result: a foreground agent closes now, a background one starts waiting.
fn apply_agent_result(
    e: &Entry,
    a: &AgentResult,
    scratch: &mut SliceScratch,
    state: &mut SessionState,
    lookup: &dyn PrimitiveLookup,
    out: &mut Derived,
) {
    let tool_use_id = e.blocks.iter().find_map(|b| match b {
        Block::ToolResult { tool_use_id, .. } => Some(tool_use_id.clone()),
        _ => None,
    });
    let used = tool_use_id
        .as_deref()
        .and_then(|id| scratch.agent_uses.remove(id));
    let (start_ns, subagent_type, parent_key) = match used {
        Some(u) => (u.ts_ns, u.subagent_type, u.parent_key),
        None => (e.ts_ns, None, state.root_parent_key.clone()),
    };
    let name = subagent_type
        .or_else(|| a.agent_type.clone())
        .unwrap_or_else(|| "unknown".to_string());
    let tool_use_id = tool_use_id.filter(|id| !id.is_empty());
    let span_key = agent_span_key(&a.agent_id, tool_use_id.as_deref());
    if a.is_async {
        // Without a tool_use id no notification could ever match it.
        let Some(tool_use_id) = tool_use_id else {
            return;
        };
        state.pending_agents.push(PendingAgent {
            agent_id: a.agent_id.clone(),
            tool_use_id,
            span_key,
            parent_key,
            name,
            start_ns,
        });
        return;
    }
    let outcome = if a.status.as_deref() == Some("completed") {
        Outcome::Ok
    } else {
        Outcome::Error
    };
    let mut i = subagent_invocation(
        &span_key,
        parent_key.as_deref(),
        name,
        start_ns,
        e.ts_ns,
        outcome,
        lookup,
    );
    i.tokens = a.usage;
    i.tool_calls = a.total_tool_use_count;
    out.closed.push(i);
    out.subagents.push(SubagentRef {
        agent_id: a.agent_id.clone(),
        span_key,
        parent_key,
    });
}

/// A `<task-notification>` for a pending background agent closes it. Others (background Bash) are
/// ignored.
fn close_notified(
    n: &TaskNotification,
    ts_ns: u64,
    state: &mut SessionState,
    lookup: &dyn PrimitiveLookup,
    out: &mut Derived,
) {
    let Some(i) = state
        .pending_agents
        .iter()
        .position(|p| !n.tool_use_id.is_empty() && p.tool_use_id == n.tool_use_id)
    else {
        return;
    };
    let outcome = match n.status.as_deref() {
        Some("completed") => Outcome::Ok,
        Some("killed") => Outcome::Interrupted,
        _ => Outcome::Error,
    };
    let p = state.pending_agents.remove(i);
    close_pending(p, ts_ns, outcome, lookup, out);
}

/// Tokens and tool counts stay unset: Task 12 fills them from the subagent's transcript.
fn close_pending(
    p: PendingAgent,
    end_ns: u64,
    outcome: Outcome,
    lookup: &dyn PrimitiveLookup,
    out: &mut Derived,
) {
    out.closed.push(subagent_invocation(
        &p.span_key,
        p.parent_key.as_deref(),
        p.name,
        p.start_ns,
        end_ns.max(p.start_ns),
        outcome,
        lookup,
    ));
    out.subagents.push(SubagentRef {
        agent_id: p.agent_id,
        span_key: p.span_key,
        parent_key: p.parent_key,
    });
}

fn close(o: OpenInvocation, lookup: &dyn PrimitiveLookup) -> Invocation {
    let kind = parse_kind(&o.kind);
    let info = metadata(kind, &o.name, lookup);
    Invocation {
        span_id: span_id(&o.span_key),
        parent_span_id: o.parent_key.as_deref().map(span_id),
        start_ns: o.start_ns,
        end_ns: if o.attributed { o.last_ns } else { o.start_ns },
        kind,
        version: info.as_ref().and_then(|i| i.version.clone()),
        plugin: info.as_ref().and_then(|i| i.plugin.clone()),
        marketplace: info.as_ref().and_then(|i| i.marketplace.clone()),
        context_tokens: info.as_ref().and_then(|i| i.context_tokens),
        invoker: parse_invoker(&o.invoker),
        outcome: Some(parse_outcome(o.outcome.as_deref())),
        tool_calls: Some(o.tool_calls),
        tool_errors: Some(o.tool_errors),
        tools: o.tools.into_iter().collect(),
        mcp: None,
        model: o.model,
        tokens: o.tokens,
        inventory: Vec::new(),
        name: o.name,
    }
}
