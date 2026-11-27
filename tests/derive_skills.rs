use std::path::PathBuf;
use wake::event::{Invocation, Invoker, Kind, Outcome};
use wake::harness::claude_code::derive::{derive, Derived};
use wake::harness::claude_code::transcript::{read_from, Entry};
use wake::harness::{PrimitiveInfo, PrimitiveLookup};
use wake::ids::span_id;
use wake::state::SessionState;

/// Knows a fixed set of skills; `run-sdlc` carries metadata.
struct Lookup;

const SKILLS: &[&str] = &[
    "run-sdlc",
    "writing-plans",
    "test-driven-development",
    "brainstorming",
    "systematic-debugging",
    "verify-changes",
    "pr-review",
];

impl PrimitiveLookup for Lookup {
    fn skill(&self, name: &str) -> Option<PrimitiveInfo> {
        SKILLS.contains(&name).then(|| PrimitiveInfo {
            kind: Kind::Skill,
            name: name.to_string(),
            version: (name == "run-sdlc").then(|| "1.2.0".to_string()),
            plugin: (name == "run-sdlc").then(|| "builders".to_string()),
            marketplace: (name == "run-sdlc").then(|| "supermodular".to_string()),
            context_tokens: (name == "run-sdlc").then_some(1234),
        })
    }
    fn agent(&self, _name: &str) -> Option<PrimitiveInfo> {
        None
    }
}

fn entries(name: &str) -> Vec<Entry> {
    let p = PathBuf::from(env!("CARGO_MANIFEST_DIR"))
        .join("tests/fixtures/claude_code")
        .join(format!("{name}.jsonl"));
    read_from(&p, 0).unwrap().0
}

fn run(session: &str, es: &[Entry], st: &mut SessionState, end: bool) -> Derived {
    let now = es.last().map(|e| e.ts_ns).unwrap_or(0);
    derive(session, es, st, &Lookup, now, end)
}

fn by_name<'a>(d: &'a Derived, name: &str) -> &'a Invocation {
    d.closed.iter().find(|i| i.name == name).unwrap()
}

#[test]
fn multiturn_is_one_invocation() {
    let es = entries("skills_multiturn");
    let mut st = SessionState::default();
    let d = run("s-multi", &es, &mut st, true);
    assert_eq!(d.closed.len(), 1);
    assert!(d.subagents.is_empty());
    let i = &d.closed[0];
    assert_eq!(i.kind, Kind::Skill);
    assert_eq!(i.name, "run-sdlc");
    assert_eq!(i.span_id, span_id("toolu_m_skill"));
    assert_eq!(i.parent_span_id, None);
    assert_eq!(i.invoker, Some(Invoker::Model));
    assert_eq!(i.outcome, Some(Outcome::Ok));
    assert_eq!(i.tokens.input, Some(750));
    assert_eq!(i.tokens.output, Some(105));
    assert_eq!(i.tool_calls, Some(2));
    assert_eq!(i.tool_errors, Some(0));
    assert_eq!(
        i.tools,
        vec![("Bash".to_string(), 1), ("Read".to_string(), 1)]
    );
    assert_eq!(i.model.as_deref(), Some("claude-opus-5"));
    assert_eq!(i.start_ns, es[1].ts_ns);
    assert_eq!(i.end_ns, es[11].ts_ns);
    assert_eq!(i.version.as_deref(), Some("1.2.0"));
    assert_eq!(i.plugin.as_deref(), Some("builders"));
    assert_eq!(i.marketplace.as_deref(), Some("supermodular"));
    assert_eq!(i.context_tokens, Some(1234));
    assert!(st.open.is_empty());
    assert_eq!(st.last_ns, es[11].ts_ns);
}

#[test]
fn multiturn_open_across_stop() {
    let es = entries("skills_multiturn");
    let mut st = SessionState::default();
    let d1 = run("s-multi", &es[..6], &mut st, false);
    assert_eq!(d1.closed.len(), 0);
    assert_eq!(st.open.len(), 1);
    let d2 = run("s-multi", &es[6..10], &mut st, false);
    assert_eq!(d2.closed.len(), 0);
    assert_eq!(st.open.len(), 1);
    let d3 = run("s-multi", &es[10..], &mut st, true);
    assert_eq!(d3.closed.len(), 1);
    assert_eq!(d3.closed[0].tokens.input, Some(750));
    assert!(st.open.is_empty());
}

#[test]
fn nested_parent_and_invoker() {
    let es = entries("skills_nested");
    let mut st = SessionState::default();
    let d = run("s-nested", &es, &mut st, true);
    assert_eq!(d.closed.len(), 2);
    let a = by_name(&d, "writing-plans");
    let b = by_name(&d, "test-driven-development");
    assert_eq!(a.invoker, Some(Invoker::Model));
    assert_eq!(a.parent_span_id, None);
    assert_eq!(b.invoker, Some(Invoker::Nested));
    assert_eq!(b.span_id, span_id("toolu_n_b"));
    assert_eq!(b.parent_span_id, Some(span_id("toolu_n_a")));
    assert_eq!(b.tokens.input, Some(20));
    assert_eq!(b.tools, vec![("Bash".to_string(), 1)]);
    assert_eq!(a.tokens.input, Some(40));
    assert_eq!(a.tools, vec![("Skill".to_string(), 1)]);
}

#[test]
fn slash_then_tool_use_is_one_invocation() {
    let es = entries("skills_slash");
    let mut st = SessionState::default();
    let d = run("s-slash", &es, &mut st, true);
    assert_eq!(d.closed.len(), 1);
    let i = &d.closed[0];
    assert_eq!(i.name, "brainstorming");
    assert_eq!(i.invoker, Some(Invoker::User));
    assert_eq!(i.span_id, span_id(&format!("cmd:s-slash:{}", es[0].ts_ns)));
    assert_eq!(i.start_ns, es[0].ts_ns);
    assert_eq!(i.tokens.input, Some(60));
}

#[test]
fn request_id_dedup() {
    let es = entries("skills_dup_request");
    let mut st = SessionState::default();
    let d = run("s-dup", &es, &mut st, true);
    assert_eq!(d.closed.len(), 1);
    assert_eq!(d.closed[0].tokens.input, Some(100));
    assert_eq!(d.closed[0].tokens.output, Some(7));
    assert_eq!(d.closed[0].tool_calls, Some(1));
}

#[test]
fn skill_load_error_outcome() {
    let es = entries("skills_error_interrupt");
    let mut st = SessionState::default();
    let d = run("s-errint", &es, &mut st, true);
    let bad = by_name(&d, "missing-skill");
    assert_eq!(bad.outcome, Some(Outcome::Error));
    assert_eq!(bad.end_ns, bad.start_ns);
    assert_eq!(bad.version, None);
}

#[test]
fn interrupted_outcome() {
    let es = entries("skills_error_interrupt");
    let mut st = SessionState::default();
    let d = run("s-errint", &es, &mut st, true);
    assert_eq!(d.closed.len(), 2);
    let i = by_name(&d, "verify-changes");
    assert_eq!(i.outcome, Some(Outcome::Interrupted));
    assert_eq!(i.tool_calls, Some(1));
    assert_eq!(i.tool_errors, Some(0));
}

#[test]
fn idle_30min_closes() {
    let es = entries("skills_idle");
    let mut st = SessionState::default();
    let d = run("s-idle", &es, &mut st, false);
    assert_eq!(d.closed.len(), 1);
    assert_eq!(d.closed[0].end_ns, es[3].ts_ns);
    assert!(st.open.is_empty());
}

#[test]
fn idle_rule_applies_against_now() {
    let es = entries("skills_idle");
    let mut st = SessionState::default();
    let d = derive("s-idle", &es[..4], &mut st, &Lookup, es[3].ts_ns, false);
    assert!(d.closed.is_empty());
    let later = es[3].ts_ns + 31 * 60 * 1_000_000_000;
    let d = derive("s-idle", &[], &mut st, &Lookup, later, false);
    assert_eq!(d.closed.len(), 1);
    assert_eq!(d.closed[0].end_ns, es[3].ts_ns);
}

#[test]
fn restart_same_skill_closes_previous() {
    let es = entries("skills_restart");
    let mut st = SessionState::default();
    let d = run("s-restart", &es, &mut st, true);
    assert_eq!(d.closed.len(), 2);
    assert_eq!(d.closed[0].span_id, span_id("toolu_r_1"));
    assert_eq!(d.closed[0].end_ns, es[3].ts_ns);
    assert_eq!(d.closed[0].tokens.input, Some(10));
    assert_eq!(d.closed[1].span_id, span_id("toolu_r_2"));
    assert_eq!(d.closed[1].tokens.input, Some(20));
}

#[test]
fn recent_requests_capped_at_256() {
    let es = entries("skills_multiturn");
    let mut st = SessionState {
        recent_requests: (0..256).map(|i| format!("old_{i}")).collect(),
        ..SessionState::default()
    };
    run("s-multi", &es, &mut st, true);
    assert_eq!(st.recent_requests.len(), 256);
    assert_eq!(
        st.recent_requests.last().map(String::as_str),
        Some("req_m6")
    );
    assert!(!st.recent_requests.iter().any(|r| r == "old_0"));
}

#[test]
fn state_roundtrips_through_json() {
    let es = entries("skills_multiturn");
    let mut st = SessionState::default();
    run("s-multi", &es[..6], &mut st, false);
    let json = serde_json::to_string(&st).unwrap();
    let mut back: SessionState = serde_json::from_str(&json).unwrap();
    let d = run("s-multi", &es[6..], &mut back, true);
    assert_eq!(d.closed[0].tokens.input, Some(750));
}

#[test]
fn slash_attributed_before_tool_use_is_one_invocation() {
    let es = entries("skills_slash_late");
    let mut st = SessionState::default();
    let d = run("s-slash-late", &es, &mut st, true);
    assert_eq!(d.closed.len(), 1);
    let i = &d.closed[0];
    assert_eq!(i.invoker, Some(Invoker::User));
    assert_eq!(
        i.span_id,
        span_id(&format!("cmd:s-slash-late:{}", es[0].ts_ns))
    );
    assert_eq!(i.tokens.input, Some(100));
    assert_eq!(i.end_ns, es[6].ts_ns);
}

#[test]
fn slash_then_tool_use_after_new_prompt_is_two() {
    let es = entries("skills_slash_reprompt");
    let mut st = SessionState::default();
    let d = run("s-slash-reprompt", &es, &mut st, true);
    assert_eq!(d.closed.len(), 2);
    assert_eq!(d.closed[0].invoker, Some(Invoker::User));
    assert_eq!(d.closed[0].tokens.input, Some(10));
    assert_eq!(d.closed[1].invoker, Some(Invoker::Model));
    assert_eq!(d.closed[1].span_id, span_id("toolu_p_skill"));
    assert_eq!(d.closed[1].tokens.input, Some(20));
}

#[test]
fn slash_merge_survives_across_derive_calls() {
    let es = entries("skills_slash_late");
    let mut st = SessionState::default();
    run("s-slash-late", &es[..4], &mut st, false);
    let d = run("s-slash-late", &es[4..], &mut st, true);
    assert_eq!(d.closed.len(), 1);
    assert_eq!(d.closed[0].tokens.input, Some(100));
}

#[test]
fn r15_command_moves_turn_start() {
    let es = entries("skills_cmd_moves_turn");
    let mut st = SessionState::default();
    let d = run("s-r15-cmd", &es, &mut st, true);
    let i = by_name(&d, "brainstorming");
    assert_eq!(i.outcome, Some(Outcome::Ok));
}

#[test]
fn r15_other_command_ends_slash_merge_window() {
    let es = entries("skills_slash_then_cmd");
    let mut st = SessionState::default();
    let d = run("s-r15-merge", &es, &mut st, true);
    let skills: Vec<_> = d.closed.iter().filter(|i| i.kind == Kind::Skill).collect();
    assert_eq!(skills.len(), 2);
    assert_eq!(skills[0].invoker, Some(Invoker::User));
    assert_eq!(skills[1].span_id, span_id("toolu_q_skill"));
}

#[test]
fn r15_own_start_tool_use_not_counted() {
    let es = entries("skills_slash");
    let mut st = SessionState::default();
    let d = run("s-slash", &es, &mut st, true);
    let i = &d.closed[0];
    assert_eq!(i.tool_calls, Some(0));
    assert_eq!(i.tool_errors, Some(0));
    assert!(i.tools.is_empty());
}

#[test]
fn r17_root_parent_for_command_started_skill() {
    let es = entries("skills_slash");
    let mut st = SessionState {
        root_parent_key: Some("agent:x:toolu_y".to_string()),
        ..SessionState::default()
    };
    let d = run("s-slash", &es, &mut st, true);
    assert_eq!(d.closed[0].parent_span_id, Some(span_id("agent:x:toolu_y")));
    assert_eq!(d.closed[0].invoker, Some(Invoker::User));
}

#[test]
fn slash_skill_in_command_message_shape_is_one_user_invocation() {
    let es = entries("skills_slash_command_message");
    let mut st = SessionState::default();
    let d = run("s-slashcm", &es, &mut st, true);
    assert_eq!(d.closed.len(), 1);
    let i = &d.closed[0];
    assert_eq!(i.name, "brainstorming");
    assert_eq!(i.invoker, Some(Invoker::User));
}
