//! Spec §5.5: turn a slice of transcript entries into closed invocations.
//!
//! The harness's `attributionSkill` decides which skill an entry belongs to. This module only
//! decides where an invocation starts and ends, carrying open ones across turns in
//! [`SessionState`].

use crate::event::{Invocation, Invoker, Kind, Outcome};
use crate::harness::claude_code::transcript::{Block, Entry, Role};
use crate::harness::PrimitiveLookup;
use crate::ids::span_id;
use crate::state::{OpenInvocation, SessionState, RECENT_REQUESTS_CAP};

/// An open invocation with no attributed entry for this long is closed.
pub const IDLE_CLOSE_NS: u64 = 30 * 60 * 1_000_000_000;

const SKILL: &str = "skill";

/// A subagent whose own transcript should be derived next. Filled in by Task 10.
#[derive(Debug, Clone, PartialEq)]
pub struct SubagentRef {
    pub agent_id: String,
    pub span_key: String,
    pub parent_key: Option<String>,
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

    for e in entries {
        close_idle(state, e.ts_ns, lookup, &mut out);
        state.last_ns = state.last_ns.max(e.ts_ns);
        match e.role {
            Role::User => {
                if let Some(cmd) = &e.command_name {
                    if lookup.skill(cmd).is_some() {
                        start_from_command(session_id, cmd, e.ts_ns, state, lookup, &mut out);
                    }
                }
                apply_results(e, state);
                if e.interrupted {
                    mark_interrupted(state, turn_start);
                } else if is_plain_prompt(e) {
                    turn_start = e.ts_ns;
                    for o in &mut state.open {
                        o.prompt_since_start = true;
                    }
                }
            }
            Role::Assistant => {
                start_from_tool_uses(e, state, lookup, &mut out);
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
    } else {
        close_idle(state, now_ns, lookup, &mut out);
    }
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
        o.parent_key = parent_key;
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

fn close(o: OpenInvocation, lookup: &dyn PrimitiveLookup) -> Invocation {
    let info = lookup.skill(&o.name);
    Invocation {
        span_id: span_id(&o.span_key),
        parent_span_id: o.parent_key.as_deref().map(span_id),
        start_ns: o.start_ns,
        end_ns: if o.attributed { o.last_ns } else { o.start_ns },
        kind: Kind::Skill,
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
