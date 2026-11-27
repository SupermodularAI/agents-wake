use std::path::{Path, PathBuf};
use wake::event::{Invoker, Kind, Outcome};
use wake::harness::claude_code::derive::{derive, subagent_transcript, Derived};
use wake::harness::claude_code::transcript::{read_from, Entry};
use wake::harness::{PrimitiveInfo, PrimitiveLookup};
use wake::ids::span_id;
use wake::state::SessionState;

/// Knows one skill and one agent, the agent with metadata.
struct Lookup;

impl PrimitiveLookup for Lookup {
    fn skill(&self, name: &str) -> Option<PrimitiveInfo> {
        (name == "writing-plans").then(|| PrimitiveInfo {
            kind: Kind::Skill,
            name: name.to_string(),
            version: None,
            plugin: None,
            marketplace: None,
            context_tokens: None,
        })
    }
    fn agent(&self, name: &str) -> Option<PrimitiveInfo> {
        (name == "Explore").then(|| PrimitiveInfo {
            kind: Kind::Subagent,
            name: name.to_string(),
            version: Some("0.3.0".to_string()),
            plugin: Some("builders".to_string()),
            marketplace: None,
            context_tokens: Some(42),
        })
    }
}

fn entries(name: &str) -> Vec<Entry> {
    let p = PathBuf::from(env!("CARGO_MANIFEST_DIR"))
        .join("tests/fixtures/claude_code")
        .join(format!("{name}.jsonl"));
    read_from(&p, 0).unwrap().0
}

fn run(session: &str, es: &[Entry], st: &mut SessionState, end: bool) -> Derived {
    let now = es.last().map(|e| e.ts_ns).unwrap_or(st.last_ns);
    derive(session, es, st, &Lookup, now, end)
}

fn of_kind(d: &Derived, kind: Kind) -> Vec<&wake::event::Invocation> {
    d.closed.iter().filter(|i| i.kind == kind).collect()
}

#[test]
fn subagent_event_and_parent() {
    let es = entries("others_subagent");
    let mut st = SessionState::default();
    let d = run("sess-1", &es, &mut st, false);
    let subs = of_kind(&d, Kind::Subagent);
    assert_eq!(subs.len(), 1);
    let s = subs[0];
    assert_eq!(s.name, "Explore");
    assert_eq!(s.span_id, span_id("agent:abc:toolu_sa_agent"));
    assert_eq!(s.parent_span_id, Some(span_id("toolu_sa_skill")));
    assert_eq!(s.tool_calls, Some(7));
    assert_eq!(s.outcome, Some(Outcome::Ok));
    assert_eq!(s.invoker, None);
    assert_eq!(s.tokens.input, Some(300));
    assert_eq!(s.tokens.output, Some(40));
    assert_eq!(s.start_ns, es[3].ts_ns);
    assert_eq!(s.end_ns, es[4].ts_ns);
    assert_eq!(s.version.as_deref(), Some("0.3.0"));
    assert_eq!(s.plugin.as_deref(), Some("builders"));
    assert_eq!(s.context_tokens, Some(42));
    assert_eq!(d.subagents.len(), 1);
    assert_eq!(d.subagents[0].agent_id, "abc");
    assert_eq!(d.subagents[0].span_key, "agent:abc:toolu_sa_agent");
    assert_eq!(d.subagents[0].parent_key.as_deref(), Some("toolu_sa_skill"));
    assert!(st.pending_agents.is_empty());
    // The skill is still open and not closed by the subagent.
    assert_eq!(st.open.len(), 1);
}

#[test]
fn subagent_failed_status_is_error() {
    let mut es = entries("others_subagent");
    es[4].agent_result.as_mut().unwrap().status = Some("failed".to_string());
    let mut st = SessionState::default();
    let d = run("sess-1", &es, &mut st, false);
    let subs = of_kind(&d, Kind::Subagent);
    assert_eq!(subs.len(), 1);
    assert_eq!(subs[0].outcome, Some(Outcome::Error));
}

#[test]
fn subagent_without_tool_use_in_slice_falls_back() {
    let es = entries("others_subagent");
    let mut st = SessionState::default();
    let d = run("sess-1", &es[4..5], &mut st, false);
    let subs = of_kind(&d, Kind::Subagent);
    assert_eq!(subs.len(), 1);
    assert_eq!(subs[0].name, "Explore");
    assert_eq!(subs[0].start_ns, es[4].ts_ns);
    assert_eq!(subs[0].parent_span_id, None);
}

#[test]
fn async_subagent_waits_for_notification() {
    let t1 = entries("others_async_t1");
    let t2 = entries("others_async_t2");
    let mut st = SessionState::default();
    let d1 = run("sess-async", &t1, &mut st, false);
    assert!(of_kind(&d1, Kind::Subagent).is_empty());
    assert!(d1.subagents.is_empty());
    assert_eq!(st.pending_agents.len(), 1);
    assert_eq!(st.pending_agents[0].agent_id, "def");

    // Pending agents survive the state file.
    let json = serde_json::to_string(&st).unwrap();
    let mut st: SessionState = serde_json::from_str(&json).unwrap();

    let d2 = run("sess-async", &t2, &mut st, false);
    let subs = of_kind(&d2, Kind::Subagent);
    assert_eq!(subs.len(), 1, "the Bash notification produces nothing");
    let s = subs[0];
    assert_eq!(s.name, "Explore");
    assert_eq!(s.outcome, Some(Outcome::Ok));
    assert_eq!(s.parent_span_id, Some(span_id("toolu_as_skill")));
    assert_eq!(s.span_id, span_id("agent:def:toolu_as_agent"));
    assert_eq!(s.start_ns, t1[3].ts_ns);
    assert_eq!(s.end_ns, t2[1].ts_ns);
    assert_eq!(s.tool_calls, None);
    assert_eq!(s.tokens, Default::default());
    assert_eq!(d2.subagents.len(), 1);
    assert_eq!(d2.subagents[0].agent_id, "def");
    assert!(st.pending_agents.is_empty());
    assert_eq!(d2.closed.len(), 1);
}

#[test]
fn async_subagent_killed_and_session_end() {
    let t1 = entries("others_async_t1");
    let mut t2 = entries("others_async_t2");
    t2[1].task_notification.as_mut().unwrap().status = Some("killed".to_string());
    let mut st = SessionState::default();
    run("sess-async", &t1, &mut st, false);
    let d = run("sess-async", &t2, &mut st, false);
    let subs = of_kind(&d, Kind::Subagent);
    assert_eq!(subs.len(), 1);
    assert_eq!(subs[0].outcome, Some(Outcome::Interrupted));

    let mut t2 = entries("others_async_t2");
    t2[1].task_notification.as_mut().unwrap().status = Some("failed".to_string());
    let mut st = SessionState::default();
    run("sess-async", &t1, &mut st, false);
    let d = run("sess-async", &t2, &mut st, false);
    assert_eq!(of_kind(&d, Kind::Subagent)[0].outcome, Some(Outcome::Error));

    let mut st = SessionState::default();
    run("sess-async", &t1, &mut st, false);
    assert_eq!(st.pending_agents.len(), 1);
    let last = st.last_ns;
    let d = run("sess-async", &[], &mut st, true);
    let subs = of_kind(&d, Kind::Subagent);
    assert_eq!(subs.len(), 1);
    assert_eq!(subs[0].outcome, Some(Outcome::Interrupted));
    assert_eq!(subs[0].end_ns, last);
    assert_eq!(subs[0].start_ns, t1[3].ts_ns);
    assert_eq!(d.subagents.len(), 1);
    assert_eq!(d.subagents[0].agent_id, "def");
    assert!(st.pending_agents.is_empty());
}

#[test]
fn mcp_aggregated_one_event() {
    let es = entries("others_mcp");
    let mut st = SessionState::default();
    let d = run("s-mcp", &es, &mut st, false);
    assert_eq!(d.closed.len(), 1);
    let i = &d.closed[0];
    assert_eq!(i.kind, Kind::McpTool);
    assert_eq!(i.name, "playwright__click");
    assert_eq!(i.span_id, span_id("mcp:toolu_mc1"));
    assert_eq!(i.parent_span_id, None);
    assert_eq!(i.invoker, None);
    assert_eq!(i.outcome, None);
    assert_eq!(i.tool_calls, None);
    assert_eq!(i.start_ns, es[1].ts_ns);
    assert_eq!(i.end_ns, es[6].ts_ns);
    let m = i.mcp.as_ref().unwrap();
    assert_eq!(m.server, "playwright");
    assert_eq!(m.calls, 3);
    assert_eq!(m.errors, 1);
    assert_eq!(m.max_duration_ms, 250);
    assert_eq!(m.total_duration_ms, 400);
}

#[test]
fn mcp_without_result_counts_with_zero_duration() {
    let es = entries("others_mcp");
    let mut st = SessionState::default();
    let d = run("s-mcp", &es[..6], &mut st, false);
    let m = d.closed[0].mcp.as_ref().unwrap();
    assert_eq!(m.calls, 3);
    assert_eq!(m.total_duration_ms, 350);
}

#[test]
fn mcp_inside_skill_is_parented_per_derive_call() {
    // The subagent transcript has an MCP call inside an open skill.
    let p = PathBuf::from(env!("CARGO_MANIFEST_DIR"))
        .join("tests/fixtures/claude_code/sess-1/subagents/agent-abc.jsonl");
    let es = read_from(&p, 0).unwrap().0;
    let mut st = SessionState::default();
    let d = run("sess-1", &es, &mut st, false);
    // brainstorming is unknown to this lookup but still opened by the Skill tool_use.
    let mcp = of_kind(&d, Kind::McpTool);
    assert_eq!(mcp.len(), 1);
    assert_eq!(mcp[0].name, "notion__search");
    assert_eq!(mcp[0].parent_span_id, Some(span_id("toolu_sub_skill")));
}

#[test]
fn command_event() {
    let es = entries("others_command");
    let mut st = SessionState::default();
    let d = run("s-cmd", &es, &mut st, false);
    assert_eq!(d.closed.len(), 1);
    let i = &d.closed[0];
    assert_eq!(i.kind, Kind::Command);
    assert_eq!(i.name, "compact");
    assert_eq!(i.invoker, Some(Invoker::User));
    assert_eq!(i.outcome, None);
    assert_eq!(i.start_ns, es[0].ts_ns);
    assert_eq!(i.end_ns, es[0].ts_ns);
    assert_eq!(i.span_id, span_id(&format!("cmd:s-cmd:{}", es[0].ts_ns)));
    assert!(st.open.is_empty());
}

#[test]
fn builtin_tools_no_events() {
    let es = entries("others_builtin");
    let mut st = SessionState::default();
    let d = run("s-builtin", &es, &mut st, true);
    assert!(d.closed.is_empty());
    assert!(d.subagents.is_empty());
}

#[test]
fn subagent_transcript_path() {
    assert_eq!(
        subagent_transcript(Path::new("/p/sess-1.jsonl"), "sess-1", "abc"),
        PathBuf::from("/p/sess-1/subagents/agent-abc.jsonl")
    );
}

#[test]
fn async_result_without_tool_use_id_is_not_kept() {
    let mut t1 = entries("others_async_t1");
    t1[4].blocks.clear();
    let mut st = SessionState::default();
    run("sess-async", &t1, &mut st, false);
    assert!(st.pending_agents.is_empty());
}

#[test]
fn notification_with_empty_tool_use_id_matches_nothing() {
    let t1 = entries("others_async_t1");
    let mut t2 = entries("others_async_t2");
    let mut st = SessionState::default();
    run("sess-async", &t1, &mut st, false);
    st.pending_agents[0].tool_use_id = String::new();
    t2[1].task_notification.as_mut().unwrap().tool_use_id = String::new();
    let d = run("sess-async", &t2, &mut st, false);
    assert!(of_kind(&d, Kind::Subagent).is_empty());
    assert_eq!(st.pending_agents.len(), 1);
}

#[test]
fn root_parent_key_parents_top_level_invocations() {
    let p = PathBuf::from(env!("CARGO_MANIFEST_DIR"))
        .join("tests/fixtures/claude_code/sess-1/subagents/agent-abc.jsonl");
    let es = read_from(&p, 0).unwrap().0;
    let root = "agent:abc:toolu_sa_agent";
    let mut st = SessionState {
        root_parent_key: Some(root.to_string()),
        ..SessionState::default()
    };
    let d = run("sess-1", &es, &mut st, true);
    let skill = of_kind(&d, Kind::Skill);
    assert_eq!(skill.len(), 1);
    assert_eq!(skill[0].parent_span_id, Some(span_id(root)));
    assert_eq!(skill[0].invoker, Some(Invoker::Model));
    // The MCP call is inside the skill, so the skill stays its parent.
    let mcp = of_kind(&d, Kind::McpTool);
    assert_eq!(mcp[0].parent_span_id, Some(span_id("toolu_sub_skill")));

    // With no enclosing skill, MCP aggregates and commands fall back to the root.
    let mut es2 = entries("others_mcp");
    es2.extend(entries("others_command"));
    let mut st = SessionState {
        root_parent_key: Some(root.to_string()),
        ..SessionState::default()
    };
    let d = run("sess-1", &es2, &mut st, true);
    assert_eq!(
        of_kind(&d, Kind::McpTool)[0].parent_span_id,
        Some(span_id(root))
    );
    assert_eq!(
        of_kind(&d, Kind::Command)[0].parent_span_id,
        Some(span_id(root))
    );
}

#[test]
fn many_entities_in_one_slice_have_distinct_span_ids() {
    let es = entries("others_multi");
    let mut st = SessionState::default();
    let d = run("s-multi-ent", &es, &mut st, true);
    assert_eq!(of_kind(&d, Kind::Subagent).len(), 2);
    assert_eq!(of_kind(&d, Kind::Command).len(), 2);
    let mcp = of_kind(&d, Kind::McpTool);
    assert_eq!(mcp.len(), 2);
    assert_ne!(mcp[0].parent_span_id, mcp[1].parent_span_id);
    assert_eq!(of_kind(&d, Kind::Skill).len(), 1);
    assert_eq!(d.closed.len(), 7);
    let mut ids: Vec<_> = d.closed.iter().map(|i| i.span_id).collect();
    ids.sort();
    ids.dedup();
    assert_eq!(ids.len(), d.closed.len());
    // Same agentId twice (a continued subagent) still gives two subagent refs with distinct keys.
    assert_eq!(d.subagents.len(), 2);
    assert_ne!(d.subagents[0].span_key, d.subagents[1].span_key);
    assert_eq!(d.subagents[0].agent_id, d.subagents[1].agent_id);
}
