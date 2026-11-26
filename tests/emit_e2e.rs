use serde_json::Value;
use std::cell::RefCell;
use std::collections::HashMap;
use std::fs;
use std::io::Write;
use std::path::{Path, PathBuf};
use std::process::{Command, Stdio};
use std::time::{Duration, SystemTime};
use wake::config::Env;
use wake::harness::claude_code::{run_hook, Deps};
use wake::ids::span_id;
use wake::send::{SendError, Transport};
use wake::state::{self, OpenInvocation, PendingAgent, SessionState};

const ENDPOINT: &str = "http://127.0.0.1:1/";

struct MapEnv(HashMap<String, String>);

impl Env for MapEnv {
    fn var(&self, key: &str) -> Option<String> {
        self.0.get(key).cloned()
    }
}

#[derive(Default)]
struct FakeTransport {
    calls: RefCell<Vec<Value>>,
    /// Every post fails with a network error (still recorded).
    fail: std::cell::Cell<bool>,
}

impl FakeTransport {
    fn count(&self) -> usize {
        self.calls.borrow().len()
    }
    fn spans_since(&self, from: usize) -> Vec<Value> {
        self.calls.borrow()[from..]
            .iter()
            // A flush may merge several payloads into one POST (R28).
            .flat_map(|b| b["resourceSpans"].as_array().cloned().unwrap_or_default())
            .flat_map(|rs| {
                rs["scopeSpans"][0]["spans"]
                    .as_array()
                    .cloned()
                    .unwrap_or_default()
            })
            .collect()
    }
    fn spans(&self) -> Vec<Value> {
        self.spans_since(0)
    }
}

impl Transport for FakeTransport {
    fn post(&self, _url: &str, _h: &[(String, String)], body: &[u8]) -> Result<(), SendError> {
        self.calls
            .borrow_mut()
            .push(serde_json::from_slice(body).unwrap());
        if self.fail.get() {
            Err(SendError::Network)
        } else {
            Ok(())
        }
    }
}

fn attr(span: &Value, key: &str) -> Option<String> {
    span["attributes"]
        .as_array()?
        .iter()
        .find(|a| a["key"] == key)
        .and_then(|a| {
            let v = &a["value"];
            v["stringValue"]
                .as_str()
                .or_else(|| v["intValue"].as_str())
                .map(str::to_string)
        })
}

fn of_kind(spans: &[Value], kind: &str) -> Vec<Value> {
    spans
        .iter()
        .filter(|s| attr(s, "wake.primitive.kind").as_deref() == Some(kind))
        .cloned()
        .collect()
}

fn hex(b: &[u8]) -> String {
    b.iter().map(|x| format!("{x:02x}")).collect()
}

fn fixture(rel: &str) -> PathBuf {
    PathBuf::from(env!("CARGO_MANIFEST_DIR"))
        .join("tests/fixtures/claude_code")
        .join(rel)
}

struct H {
    _d: tempfile::TempDir,
    home: PathBuf,
    exe_dir: PathBuf,
    cwd: PathBuf,
    projects: PathBuf,
    env: MapEnv,
}

impl H {
    fn new() -> H {
        Self::with_env(HashMap::new())
    }

    fn with_env(env: HashMap<String, String>) -> H {
        let d = tempfile::tempdir().unwrap();
        let home = d.path().join("home");
        let exe_dir = d.path().join("exe");
        let cwd = d.path().join("work");
        let projects = d.path().join("projects");
        for p in [&home, &exe_dir, &cwd, &projects] {
            fs::create_dir_all(p).unwrap();
        }
        fs::write(
            exe_dir.join("telemetry.json"),
            format!(r#"{{"endpoint":"{ENDPOINT}"}}"#),
        )
        .unwrap();
        fs::write(
            home.join(".claude.json"),
            r#"{"oauthAccount":{"emailAddress":"dev@example.com"}}"#,
        )
        .unwrap();
        H {
            _d: d,
            home,
            exe_dir,
            cwd,
            projects,
            env: MapEnv(env),
        }
    }

    fn state_dir(&self) -> PathBuf {
        self.home.join(".local/state/wake")
    }

    /// Copy a fixture as `<projects>/<session>.jsonl` and return its path.
    fn transcript(&self, fixture_name: &str, session: &str) -> PathBuf {
        let p = self.projects.join(format!("{session}.jsonl"));
        fs::copy(fixture(&format!("{fixture_name}.jsonl")), &p).unwrap();
        p
    }

    fn subagent(&self, session: &str, agent: &str) {
        let dir = self.projects.join(session).join("subagents");
        fs::create_dir_all(&dir).unwrap();
        fs::copy(
            fixture(&format!("{session}/subagents/agent-{agent}.jsonl")),
            dir.join(format!("agent-{agent}.jsonl")),
        )
        .unwrap();
    }

    fn payload(&self, session: &str, transcript: &Path, source: Option<&str>) -> Vec<u8> {
        let mut v = serde_json::json!({
            "session_id": session,
            "transcript_path": transcript,
            "cwd": self.cwd,
            "hook_event_name": "x",
        });
        if let Some(s) = source {
            v["source"] = Value::from(s);
        }
        serde_json::to_vec(&v).unwrap()
    }

    fn run(
        &self,
        t: &FakeTransport,
        hook: &str,
        session: &str,
        transcript: &Path,
        source: Option<&str>,
    ) {
        let deps = Deps {
            env: &self.env,
            home: self.home.clone(),
            exe_dir: self.exe_dir.clone(),
            transport: t,
            now: SystemTime::now(),
        };
        run_hook(hook, &self.payload(session, transcript, source), &deps);
    }
}

fn append(dst: &Path, fixture_name: &str) {
    let bytes = fs::read(fixture(&format!("{fixture_name}.jsonl"))).unwrap();
    fs::OpenOptions::new()
        .append(true)
        .open(dst)
        .unwrap()
        .write_all(&bytes)
        .unwrap();
}

#[test]
fn stop_then_end_sends_expected_spans() {
    let h = H::new();
    let t = FakeTransport::default();
    let tr = h.transcript("skills_multiturn", "s-multi");
    h.run(&t, "session-start", "s-multi", &tr, Some("startup"));
    h.run(&t, "stop", "s-multi", &tr, None);
    h.run(&t, "session-end", "s-multi", &tr, None);
    let spans = t.spans();
    let skills = of_kind(&spans, "skill");
    assert_eq!(skills.len(), 1, "{spans:#?}");
    assert_eq!(of_kind(&spans, "inventory").len(), 1);
    assert_eq!(
        spans
            .iter()
            .filter(|s| s["name"] == "wake.inventory")
            .count(),
        1
    );
    let s = &skills[0];
    assert_eq!(s["name"], "wake.skill run-sdlc");
    assert_eq!(attr(s, "wake.harness").as_deref(), Some("claude-code"));
    assert_eq!(attr(s, "wake.harness.version").as_deref(), Some("2.1.0"));
    assert_eq!(attr(s, "session.id").as_deref(), Some("s-multi"));
    assert_eq!(
        attr(s, "wake.user.email").as_deref(),
        Some("dev@example.com")
    );
    // The state file is kept after session-end, with nothing left open.
    let st = state::load(&h.state_dir(), "s-multi").unwrap();
    assert!(st.open.is_empty());
    assert_eq!(st.offset, fs::metadata(&tr).unwrap().len());
}

#[test]
fn second_stop_sends_nothing_new() {
    let h = H::new();
    let t = FakeTransport::default();
    let tr = h.transcript("others_command", "s-cmd");
    h.run(&t, "session-start", "s-cmd", &tr, Some("startup"));
    h.run(&t, "stop", "s-cmd", &tr, None);
    assert_eq!(of_kind(&t.spans(), "command").len(), 1);
    let before = t.count();
    h.run(&t, "stop", "s-cmd", &tr, None);
    assert_eq!(t.count(), before);
}

#[test]
fn missing_state_skips_history() {
    let h = H::new();
    let t = FakeTransport::default();
    let tr = h.transcript("skills_idle", "s-idle");
    h.run(&t, "stop", "s-idle", &tr, None);
    assert_eq!(of_kind(&t.spans(), "skill").len(), 0);
    let st = state::load(&h.state_dir(), "s-idle").expect("state saved");
    assert_eq!(st.offset, fs::metadata(&tr).unwrap().len());
}

#[test]
fn resume_does_not_resend() {
    let h = H::new();
    let t = FakeTransport::default();
    let tr = h.transcript("others_command", "s-cmd");
    h.run(&t, "session-start", "s-cmd", &tr, Some("startup"));
    h.run(&t, "stop", "s-cmd", &tr, None);
    h.run(&t, "session-end", "s-cmd", &tr, None);
    assert_eq!(of_kind(&t.spans(), "command").len(), 1);
    let before = t.count();
    h.run(&t, "session-start", "s-cmd", &tr, Some("resume"));
    let after_start = t.count();
    // session-start sends only the inventory.
    let start_spans = t.spans_since(before);
    assert!(start_spans.iter().all(|s| s["name"] == "wake.inventory"));
    h.run(&t, "stop", "s-cmd", &tr, None);
    assert_eq!(t.count(), after_start);
}

#[test]
fn fork_starts_at_end_of_transcript() {
    let h = H::new();
    let t = FakeTransport::default();
    let tr = h.transcript("others_command", "s-fork");
    h.run(&t, "session-start", "s-fork", &tr, Some("fork"));
    let before = t.count();
    h.run(&t, "stop", "s-fork", &tr, None);
    assert_eq!(t.count(), before);
}

#[test]
fn wake_disabled_posts_nothing() {
    let h = H::with_env(HashMap::from([(
        "WAKE_DISABLED".to_string(),
        "1".to_string(),
    )]));
    let t = FakeTransport::default();
    let tr = h.transcript("others_command", "s-cmd");
    h.run(&t, "session-start", "s-cmd", &tr, Some("startup"));
    h.run(&t, "stop", "s-cmd", &tr, None);
    assert_eq!(t.count(), 0);
    assert!(!h.state_dir().exists());
}

#[test]
fn bad_session_id_does_nothing() {
    let h = H::new();
    let t = FakeTransport::default();
    let tr = h.transcript("others_command", "s-cmd");
    h.run(&t, "session-start", "../evil", &tr, Some("startup"));
    h.run(&t, "stop", "../evil", &tr, None);
    assert_eq!(t.count(), 0);
    assert!(!h.state_dir().exists());
}

#[test]
fn unknown_hook_does_nothing() {
    let h = H::new();
    let t = FakeTransport::default();
    let tr = h.transcript("others_command", "s-cmd");
    h.run(&t, "SessionStart", "s-cmd", &tr, Some("startup"));
    assert_eq!(t.count(), 0);
    assert!(!h.state_dir().exists());
}

#[test]
fn concurrent_stop_one_wins() {
    let h = H::new();
    let t = FakeTransport::default();
    let tr = h.transcript("others_command", "s-cmd");
    let _guard = state::lock_session(&h.state_dir(), "s-cmd").unwrap();
    h.run(&t, "stop", "s-cmd", &tr, None);
    h.run(&t, "session-start", "s-cmd", &tr, Some("startup"));
    assert_eq!(t.count(), 0);
    assert!(state::load(&h.state_dir(), "s-cmd").is_none());
}

#[test]
fn subagent_recursion() {
    let h = H::new();
    let t = FakeTransport::default();
    let tr = h.transcript("others_subagent", "sess-1");
    h.subagent("sess-1", "abc");
    h.run(&t, "session-start", "sess-1", &tr, Some("startup"));
    h.run(&t, "stop", "sess-1", &tr, None);
    let spans = t.spans();
    let sub = of_kind(&spans, "subagent");
    assert_eq!(sub.len(), 1, "{spans:#?}");
    let sub = &sub[0];
    let sub_id = hex(&span_id("agent:abc:toolu_sa_agent"));
    assert_eq!(sub["spanId"], sub_id);
    // R16: tool calls come from the subagent's own transcript.
    assert_eq!(attr(sub, "wake.window.tool_calls").as_deref(), Some("2"));
    // R29: the model of the transcript's last assistant entry with usage, so it can be priced.
    assert_eq!(
        attr(sub, "gen_ai.request.model").as_deref(),
        Some("claude-haiku-5-5")
    );

    let skill = spans
        .iter()
        .find(|s| s["name"] == "wake.skill brainstorming")
        .expect("subagent's skill");
    assert_eq!(skill["parentSpanId"], sub_id);
    // R29: the transcript holds 20 input / 2 output tokens (two requests). The nested skill owns
    // req_sub2; the subagent span keeps only the rest, so the two are disjoint and sum to the total.
    let tok = |s: &Value, k: &str| -> u64 { attr(s, k).map_or(0, |v| v.parse().unwrap()) };
    for (key, total) in [
        ("gen_ai.usage.input_tokens", 20),
        ("gen_ai.usage.output_tokens", 2),
    ] {
        assert!(tok(skill, key) > 0, "{key}: skill has tokens");
        assert!(tok(sub, key) > 0, "{key}: subagent keeps its own");
        assert_eq!(tok(sub, key) + tok(skill, key), total, "{key}");
    }
    assert_eq!(
        attr(sub, "gen_ai.usage.input_tokens").as_deref(),
        Some("10")
    );
    let mcp = of_kind(&spans, "mcp_tool");
    assert_eq!(mcp.len(), 1);
    assert_eq!(mcp[0]["parentSpanId"], skill["spanId"]);
}

#[test]
fn async_subagent_across_stops() {
    let h = H::new();
    let t = FakeTransport::default();
    let tr = h.transcript("others_async_t1", "sess-async");
    h.subagent("sess-async", "def");
    h.run(&t, "session-start", "sess-async", &tr, Some("startup"));
    h.run(&t, "stop", "sess-async", &tr, None);
    assert_eq!(of_kind(&t.spans(), "subagent").len(), 0);
    let st = state::load(&h.state_dir(), "sess-async").unwrap();
    assert_eq!(st.pending_agents.len(), 1);

    append(&tr, "others_async_t2");
    h.run(&t, "stop", "sess-async", &tr, None);
    let sub = of_kind(&t.spans(), "subagent");
    assert_eq!(sub.len(), 1);
    assert_eq!(
        attr(&sub[0], "gen_ai.usage.input_tokens").as_deref(),
        Some("120")
    );
    assert_eq!(
        attr(&sub[0], "wake.window.tool_calls").as_deref(),
        Some("1")
    );
    assert_eq!(attr(&sub[0], "wake.outcome").as_deref(), Some("ok"));
    assert_eq!(
        attr(&sub[0], "gen_ai.request.model").as_deref(),
        Some("claude-haiku-5-5")
    );
}

#[test]
fn stale_session_flushed() {
    let h = H::new();
    let t = FakeTransport::default();
    let dir = h.state_dir();
    let other = SessionState {
        offset: 1234,
        open: vec![OpenInvocation {
            span_key: "toolu_stale".to_string(),
            kind: "skill".to_string(),
            name: "stale-skill".to_string(),
            invoker: "model".to_string(),
            start_ns: 1_000_000_000,
            last_ns: 2_000_000_000,
            attributed: true,
            ..OpenInvocation::default()
        }],
        user: Some("cached@example.com".to_string()),
        repo: Some("github.com/acme/demo".to_string()),
        ..SessionState::default()
    };
    state::save(&dir, "other-sess", &other).unwrap();
    let old = SystemTime::now() - Duration::from_secs(31 * 60);
    fs::File::options()
        .write(true)
        .open(dir.join("sessions/other-sess.json"))
        .unwrap()
        .set_modified(old)
        .unwrap();

    let tr = h.transcript("others_command", "s-cmd");
    h.run(&t, "stop", "s-cmd", &tr, None);
    let spans = t.spans();
    let stale: Vec<_> = spans
        .iter()
        .filter(|s| s["name"] == "wake.skill stale-skill")
        .collect();
    assert_eq!(stale.len(), 1, "{spans:#?}");
    assert_eq!(
        attr(stale[0], "wake.user.email").as_deref(),
        Some("cached@example.com")
    );
    assert_eq!(
        attr(stale[0], "wake.repo").as_deref(),
        Some("github.com/acme/demo")
    );
    assert_eq!(attr(stale[0], "session.id").as_deref(), Some("other-sess"));
    let after = state::load(&dir, "other-sess").unwrap();
    assert!(after.open.is_empty());
    assert!(after.pending_agents.is_empty());
    assert_eq!(after.offset, 1234);
}

fn save_stale(dir: &Path, id: &str, skill: &str) {
    let st = SessionState {
        open: vec![OpenInvocation {
            span_key: format!("toolu_{id}"),
            kind: "skill".to_string(),
            name: skill.to_string(),
            invoker: "model".to_string(),
            start_ns: 1_000_000_000,
            last_ns: 2_000_000_000,
            ..OpenInvocation::default()
        }],
        ..SessionState::default()
    };
    state::save(dir, id, &st).unwrap();
    fs::File::options()
        .write(true)
        .open(dir.join(format!("sessions/{id}.json")))
        .unwrap()
        .set_modified(SystemTime::now() - Duration::from_secs(31 * 60))
        .unwrap();
}

fn set_mtime(path: &Path, ago: Duration) {
    fs::File::options()
        .write(true)
        .open(path)
        .unwrap()
        .set_modified(SystemTime::now() - ago)
        .unwrap();
}

/// A session with an open skill whose state is 31 minutes old and whose transcript was last
/// written `transcript_age` ago.
fn save_with_transcript(h: &H, id: &str, transcript_age: Duration) {
    let tr = h.transcript("others_command", id);
    let st = SessionState {
        offset: fs::metadata(&tr).unwrap().len(),
        open: vec![OpenInvocation {
            span_key: format!("toolu_{id}"),
            kind: "skill".to_string(),
            name: format!("long-{id}"),
            invoker: "model".to_string(),
            start_ns: 1_000_000_000,
            last_ns: 2_000_000_000,
            attributed: true,
            ..OpenInvocation::default()
        }],
        transcript_path: Some(tr.to_str().unwrap().to_string()),
        ..SessionState::default()
    };
    state::save(&h.state_dir(), id, &st).unwrap();
    set_mtime(
        &h.state_dir().join(format!("sessions/{id}.json")),
        Duration::from_secs(31 * 60),
    );
    set_mtime(&tr, transcript_age);
}

#[test]
fn stale_needs_an_idle_transcript_too() {
    // R27: a long single turn keeps writing its transcript while its state file ages.
    let h = H::new();
    let t = FakeTransport::default();
    save_with_transcript(&h, "live-turn", Duration::from_secs(60));
    save_with_transcript(&h, "idle-sess", Duration::from_secs(31 * 60));
    let tr = h.transcript("others_command", "s-cmd");
    h.run(&t, "stop", "s-cmd", &tr, None);
    let spans = t.spans();
    assert!(
        !spans
            .iter()
            .any(|s| s["name"] == "wake.skill long-live-turn"),
        "{spans:#?}"
    );
    assert_eq!(
        state::load(&h.state_dir(), "live-turn").unwrap().open.len(),
        1
    );
    assert!(spans
        .iter()
        .any(|s| s["name"] == "wake.skill long-idle-sess"));
    assert!(state::load(&h.state_dir(), "idle-sess")
        .unwrap()
        .open
        .is_empty());
}

#[test]
fn run_hook_records_transcript_path() {
    let h = H::new();
    let t = FakeTransport::default();
    let tr = h.transcript("others_command", "s-cmd");
    h.run(&t, "session-start", "s-cmd", &tr, Some("startup"));
    let st = state::load(&h.state_dir(), "s-cmd").unwrap();
    assert_eq!(st.transcript_path.as_deref(), tr.to_str());
    h.run(&t, "stop", "s-cmd", &tr, None);
    let st = state::load(&h.state_dir(), "s-cmd").unwrap();
    assert_eq!(st.transcript_path.as_deref(), tr.to_str());
}

#[test]
fn stale_pending_agent_follows_its_transcript() {
    let h = H::new();
    let t = FakeTransport::default();
    // The main transcript of the stale session is gone; its subagent transcript is still there.
    let main = h.projects.join("sess-async.jsonl");
    h.subagent("sess-async", "def");
    let st = SessionState {
        pending_agents: vec![PendingAgent {
            agent_id: "def".to_string(),
            tool_use_id: "toolu_async_agent".to_string(),
            span_key: "agent:def:toolu_async_agent".to_string(),
            parent_key: None,
            name: "Explore".to_string(),
            start_ns: 1_000_000_000,
        }],
        transcript_path: Some(main.to_str().unwrap().to_string()),
        ..SessionState::default()
    };
    state::save(&h.state_dir(), "sess-async", &st).unwrap();
    set_mtime(
        &h.state_dir().join("sessions/sess-async.json"),
        Duration::from_secs(31 * 60),
    );
    let tr = h.transcript("others_command", "s-cmd");
    h.run(&t, "stop", "s-cmd", &tr, None);
    let sub = of_kind(&t.spans(), "subagent");
    assert_eq!(sub.len(), 1, "{:#?}", t.spans());
    assert_eq!(
        attr(&sub[0], "gen_ai.usage.input_tokens").as_deref(),
        Some("120")
    );
    assert_eq!(
        attr(&sub[0], "wake.window.tool_calls").as_deref(),
        Some("1")
    );
    assert_eq!(
        attr(&sub[0], "gen_ai.request.model").as_deref(),
        Some("claude-haiku-5-5")
    );
    assert_eq!(attr(&sub[0], "session.id").as_deref(), Some("sess-async"));
}

#[test]
fn failed_send_keeps_state_advanced_and_queues_payload() {
    let h = H::new();
    let t = FakeTransport::default();
    let tr = h.transcript("others_command", "s-cmd");
    h.run(&t, "session-start", "s-cmd", &tr, Some("fork"));
    let before = t.count();
    // Start at 0 so the stop has a command to send.
    let mut st = state::load(&h.state_dir(), "s-cmd").unwrap();
    st.offset = 0;
    state::save(&h.state_dir(), "s-cmd", &st).unwrap();

    t.fail.set(true);
    h.run(&t, "stop", "s-cmd", &tr, None);
    assert_eq!(t.count(), before + 1, "one attempt");
    let st = state::load(&h.state_dir(), "s-cmd").unwrap();
    assert_eq!(st.offset, fs::metadata(&tr).unwrap().len());
    let outbox = fs::read_to_string(h.state_dir().join("outbox.ndjson")).unwrap();
    assert_eq!(outbox.lines().count(), 1);
    assert!(outbox.contains("wake.command compact"));

    t.fail.set(false);
    let before = t.count();
    h.run(&t, "stop", "s-cmd", &tr, None);
    let resent = t.spans_since(before);
    assert_eq!(t.count(), before + 1);
    assert_eq!(resent.len(), 1);
    assert_eq!(resent[0]["name"], "wake.command compact");
    assert!(!h.state_dir().join("outbox.ndjson").exists());
}

#[test]
fn stale_sessions_queue_then_one_post_attempt() {
    let h = H::new();
    let t = FakeTransport::default();
    t.fail.set(true);
    let dir = h.state_dir();
    for i in 0..3 {
        save_stale(&dir, &format!("stale-{i}"), &format!("skill-{i}"));
    }
    let tr = h.transcript("others_command", "s-cmd");
    h.run(&t, "stop", "s-cmd", &tr, None);
    assert_eq!(t.count(), 1);
    let outbox = fs::read_to_string(dir.join("outbox.ndjson")).unwrap();
    assert_eq!(outbox.lines().count(), 3);
    for i in 0..3 {
        assert!(state::load(&dir, &format!("stale-{i}"))
            .unwrap()
            .open
            .is_empty());
    }
}

#[test]
fn empty_subagent_transcript_keeps_derived_usage() {
    let h = H::new();
    let t = FakeTransport::default();
    let tr = h.transcript("others_subagent", "sess-1");
    let sub_dir = h.projects.join("sess-1/subagents");
    fs::create_dir_all(&sub_dir).unwrap();
    fs::write(sub_dir.join("agent-abc.jsonl"), "").unwrap();
    h.run(&t, "session-start", "sess-1", &tr, Some("startup"));
    h.run(&t, "stop", "sess-1", &tr, None);
    let sub = of_kind(&t.spans(), "subagent");
    assert_eq!(sub.len(), 1);
    // From toolUseResult, not overwritten by the empty transcript.
    assert_eq!(
        attr(&sub[0], "gen_ai.usage.input_tokens").as_deref(),
        Some("300")
    );
    assert_eq!(
        attr(&sub[0], "wake.window.tool_calls").as_deref(),
        Some("7")
    );
}

// ---- tests that spawn the real binary ----

fn spawn_emit(home: &Path, path_env: &str, hook: &str, payload: &[u8]) -> std::process::Output {
    let mut child = Command::new(env!("CARGO_BIN_EXE_wake"))
        .args(["emit", hook])
        .env_clear()
        .env("HOME", home)
        .env("PATH", path_env)
        .env("WAKE_OTLP_ENDPOINT", ENDPOINT)
        .stdin(Stdio::piped())
        .stdout(Stdio::piped())
        .stderr(Stdio::piped())
        .spawn()
        .unwrap();
    child.stdin.take().unwrap().write_all(payload).unwrap();
    child.wait_with_output().unwrap()
}

#[test]
fn identity_cached() {
    use std::os::unix::fs::PermissionsExt;
    let h = H::new();
    let bin = h.home.join("fakebin");
    fs::create_dir_all(&bin).unwrap();
    let log = h.home.join("git.log");
    let git = bin.join("git");
    fs::write(
        &git,
        format!(
            "#!/bin/sh\necho \"$@\" >> '{}'\necho git@github.com:acme/demo.git\n",
            log.display()
        ),
    )
    .unwrap();
    fs::set_permissions(&git, fs::Permissions::from_mode(0o755)).unwrap();
    let path_env = bin.to_str().unwrap();

    let tr = h.transcript("skills_multiturn", "s-git");
    let start = h.payload("s-git", &tr, Some("startup"));
    let stop = h.payload("s-git", &tr, None);
    spawn_emit(&h.home, path_env, "session-start", &start);
    spawn_emit(&h.home, path_env, "stop", &stop);
    append(&tr, "others_command");
    spawn_emit(&h.home, path_env, "stop", &stop);

    let calls = fs::read_to_string(&log).unwrap_or_default();
    assert_eq!(calls.lines().count(), 1, "git calls: {calls}");
    let st = state::load(&h.state_dir(), "s-git").unwrap();
    assert_eq!(st.repo.as_deref(), Some("github.com/acme/demo"));
    assert_eq!(st.user.as_deref(), Some("dev@example.com"));
}

#[test]
fn cli_emit_silent_exit_zero() {
    let h = H::new();
    let tr = h.transcript("skills_multiturn", "s-cli");
    for (hook, source) in [
        ("session-start", Some("startup")),
        ("stop", None),
        ("session-end", None),
    ] {
        let out = spawn_emit(
            &h.home,
            "/usr/bin:/bin",
            hook,
            &h.payload("s-cli", &tr, source),
        );
        assert_eq!(out.status.code(), Some(0));
        assert!(out.stdout.is_empty());
        assert!(out.stderr.is_empty());
    }
    // It really ran: state was written and the failed sends were queued.
    assert!(state::load(&h.state_dir(), "s-cli").is_some());
    assert!(h.state_dir().join("outbox.ndjson").exists());
}

#[test]
fn state_dir_and_outbox_are_private() {
    use std::os::unix::fs::PermissionsExt;
    let h = H::new();
    let t = FakeTransport::default();
    t.fail.set(true);
    let tr = h.transcript("others_command", "s-cmd");
    h.run(&t, "session-start", "s-cmd", &tr, Some("startup"));
    let mode = |p: &Path| fs::metadata(p).unwrap().permissions().mode() & 0o777;
    assert_eq!(mode(&h.state_dir()), 0o700);
    assert_eq!(mode(&h.state_dir().join("sessions")), 0o700);
    assert_eq!(mode(&h.state_dir().join("outbox.ndjson")), 0o600);
}
