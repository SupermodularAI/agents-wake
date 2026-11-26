use serde_json::Value;
use wake::event::*;
use wake::ids::{span_id, trace_id};
use wake::otlp::encode;

fn ctx(user: UserId) -> Context {
    Context {
        harness: "claude-code",
        harness_version: Some("2.1.0".into()),
        session_id: "sess-1".into(),
        user,
        repo: Some("github.com/acme/app".into()),
    }
}

fn inv(kind: Kind, name: &str) -> Invocation {
    Invocation {
        span_id: span_id(name),
        parent_span_id: None,
        start_ns: 1_000_000_000,
        end_ns: 3_500_000_000,
        kind,
        name: name.into(),
        version: None,
        plugin: None,
        marketplace: None,
        context_tokens: None,
        invoker: None,
        outcome: None,
        tool_calls: None,
        tool_errors: None,
        tools: vec![],
        mcp: None,
        model: None,
        tokens: Tokens::default(),
        inventory: vec![],
    }
}

fn spans(v: &Value) -> &Vec<Value> {
    assert_eq!(v["resourceSpans"].as_array().unwrap().len(), 1);
    assert_eq!(
        v["resourceSpans"][0]["scopeSpans"]
            .as_array()
            .unwrap()
            .len(),
        1
    );
    v["resourceSpans"][0]["scopeSpans"][0]["spans"]
        .as_array()
        .unwrap()
}

fn attr<'a>(span: &'a Value, key: &str) -> Option<&'a Value> {
    span["attributes"]
        .as_array()
        .unwrap()
        .iter()
        .find(|a| a["key"] == key)
        .map(|a| &a["value"])
}

#[test]
fn ids_deterministic() {
    assert_eq!(span_id("a"), span_id("a"));
    assert_ne!(span_id("a"), span_id("b"));
    assert_eq!(trace_id("s"), trace_id("s"));
    assert_ne!(trace_id("s"), trace_id("t"));
    assert_eq!(trace_id("s").len(), 16);
}

/// OTel GenAI semconv (and Langfuse, which subtracts the cache fields from it):
/// `gen_ai.usage.input_tokens` is the TOTAL input, cached tokens included. Claude Code's
/// transcript `input_tokens` excludes cache, so wake adds the cache fields back in.
#[test]
fn input_tokens_include_cache_per_otel_semconv() {
    let mut i = inv(Kind::Skill, "brainstorming");
    i.tokens.input = Some(10);
    i.tokens.output = Some(100);
    i.tokens.cache_read = Some(50_000);
    i.tokens.cache_creation = Some(2_000);
    let v = encode(&ctx(UserId::Unknown), &[i]);
    let s = &spans(&v)[0];
    assert_eq!(
        attr(s, "gen_ai.usage.input_tokens").unwrap()["intValue"],
        "52010"
    );
    assert_eq!(
        attr(s, "gen_ai.usage.cache_read_input_tokens").unwrap()["intValue"],
        "50000"
    );
    assert_eq!(
        attr(s, "gen_ai.usage.cache_creation_input_tokens").unwrap()["intValue"],
        "2000"
    );

    // Cache only, no uncached input recorded: the total is still the cache sum.
    let mut j = inv(Kind::Skill, "brainstorming");
    j.tokens.cache_read = Some(7);
    let v = encode(&ctx(UserId::Unknown), &[j]);
    assert_eq!(
        attr(&spans(&v)[0], "gen_ai.usage.input_tokens").unwrap()["intValue"],
        "7"
    );

    // No input-side counts at all: the attribute stays absent, never 0.
    let mut k = inv(Kind::Skill, "brainstorming");
    k.tokens.output = Some(5);
    let v = encode(&ctx(UserId::Unknown), &[k]);
    assert!(attr(&spans(&v)[0], "gen_ai.usage.input_tokens").is_none());
}

#[test]
fn encode_skill_span_shape() {
    let mut i = inv(Kind::Skill, "brainstorming");
    i.tokens.input = Some(48211);
    i.tokens.output = Some(6930);
    i.tools = vec![("Read".into(), 5), ("Bash".into(), 6)];
    i.model = Some("claude-opus-5-5".into());
    let v = encode(&ctx(UserId::Unknown), &[i]);
    let s = &spans(&v)[0];
    assert_eq!(s["name"], "wake.skill brainstorming");
    assert_eq!(s["kind"], 1);
    assert_eq!(
        attr(s, "wake.window.tools").unwrap()["stringValue"],
        "Bash:6,Read:5"
    );
    assert_eq!(
        attr(s, "gen_ai.usage.input_tokens").unwrap()["intValue"],
        "48211"
    );
    assert!(attr(s, "gen_ai.usage.cache_read_input_tokens").is_none());
    assert_eq!(
        attr(s, "gen_ai.request.model").unwrap()["stringValue"],
        "claude-opus-5-5"
    );
    assert_eq!(
        attr(s, "langfuse.observation.type").unwrap()["stringValue"],
        "generation"
    );
    assert_eq!(
        attr(s, "wake.window.duration_ms").unwrap()["intValue"],
        "2500"
    );
    assert_eq!(s["traceId"].as_str().unwrap().len(), 32);
    assert_eq!(s["spanId"].as_str().unwrap().len(), 16);
    assert_eq!(s["startTimeUnixNano"], "1000000000");
    assert!(s.get("parentSpanId").is_none());
}

#[test]
fn encode_no_generation_marker_without_tokens_or_model() {
    let v = encode(&ctx(UserId::Unknown), &[inv(Kind::Command, "init")]);
    assert!(attr(&spans(&v)[0], "langfuse.observation.type").is_none());
}

#[test]
fn encode_parent_and_resource() {
    let mut i = inv(Kind::McpTool, "mcp__playwright__click");
    i.parent_span_id = Some(span_id("p"));
    i.mcp = Some(McpAgg {
        server: "playwright".into(),
        calls: 2,
        errors: 1,
        total_duration_ms: 30,
        max_duration_ms: 20,
    });
    let v = encode(&ctx(UserId::Unknown), &[i]);
    let s = &spans(&v)[0];
    assert_eq!(s["parentSpanId"].as_str().unwrap().len(), 16);
    assert_eq!(attr(s, "wake.mcp.calls").unwrap()["intValue"], "2");
    let res = v["resourceSpans"][0]["resource"]["attributes"]
        .as_array()
        .unwrap();
    assert!(res
        .iter()
        .any(|a| a["key"] == "service.name" && a["value"]["stringValue"] == "wake"));
    assert_eq!(
        v["resourceSpans"][0]["scopeSpans"][0]["scope"]["name"],
        "wake"
    );
}

#[test]
fn encode_email_vs_hash() {
    let v = encode(
        &ctx(UserId::Hash("abc123".into())),
        &[inv(Kind::Skill, "x")],
    );
    let s = &spans(&v)[0];
    assert_eq!(attr(s, "wake.user.hash").unwrap()["stringValue"], "abc123");
    assert_eq!(attr(s, "user.id").unwrap()["stringValue"], "abc123");
    assert!(attr(s, "wake.user.email").is_none());
    let v = encode(
        &ctx(UserId::Email("a@b.co".into())),
        &[inv(Kind::Skill, "x")],
    );
    let s = &spans(&v)[0];
    assert_eq!(attr(s, "wake.user.email").unwrap()["stringValue"], "a@b.co");
    assert_eq!(attr(s, "user.id").unwrap()["stringValue"], "a@b.co");
    assert!(attr(s, "wake.user.hash").is_none());
}

#[test]
fn encode_inventory_array() {
    let mut i = inv(Kind::Inventory, "inventory");
    i.inventory = vec!["skill:a@1".into(), "skill:b@-".into()];
    let v = encode(&ctx(UserId::Unknown), &[i]);
    let s = &spans(&v)[0];
    assert_eq!(s["name"], "wake.inventory");
    let vals = attr(s, "wake.inventory").unwrap()["arrayValue"]["values"]
        .as_array()
        .unwrap();
    assert_eq!(vals.len(), 2);
    assert_eq!(vals[0]["stringValue"], "skill:a@1");
}

#[test]
fn encode_never_contains_forbidden_keys() {
    let mut i = inv(Kind::Skill, "brainstorming");
    i.version = Some("1.0".into());
    i.plugin = Some("superpowers".into());
    i.marketplace = Some("official".into());
    i.context_tokens = Some(10);
    i.invoker = Some(Invoker::Model);
    i.outcome = Some(Outcome::Ok);
    i.tool_calls = Some(3);
    i.tool_errors = Some(1);
    i.tools = vec![("Bash".into(), 2)];
    i.mcp = Some(McpAgg {
        server: "s".into(),
        calls: 1,
        errors: 0,
        total_duration_ms: 1,
        max_duration_ms: 1,
    });
    i.model = Some("m".into());
    i.tokens = Tokens {
        input: Some(1),
        output: Some(2),
        cache_read: Some(3),
        cache_creation: Some(4),
    };
    i.parent_span_id = Some(span_id("p"));
    let v = encode(&ctx(UserId::Email("a@b.co".into())), &[i]);
    let text = serde_json::to_string(&v).unwrap();
    for bad in [
        "prompt",
        "tool_input",
        "command",
        "/Users/",
        "cwd",
        "transcript",
    ] {
        assert!(!text.contains(bad), "contains {bad}");
    }
}

#[test]
fn id_known_answers() {
    // Pinned from `printf 'wake-trace\0s1' | shasum -a 256` (first 16 bytes) and
    // `printf 'wake-span\0k1' | shasum -a 256` (first 8 bytes): changing the derivation would
    // re-key every span already in a backend.
    let hex = |b: &[u8]| b.iter().map(|x| format!("{x:02x}")).collect::<String>();
    assert_eq!(hex(&trace_id("s1")), "dd76f1fcdb8bdd8cc57fccbc4a718289");
    assert_eq!(hex(&span_id("k1")), "ddd5aa5ec2ea342d");
}
