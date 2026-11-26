use wake::event::*;

fn inv(name: &str) -> Invocation {
    Invocation {
        span_id: [1; 8],
        parent_span_id: None,
        start_ns: 1,
        end_ns: 2,
        kind: Kind::Skill,
        name: name.to_string(),
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

#[test]
fn token_accepts_plugin_namespace() {
    assert!(is_token("superpowers:brainstorming"));
}

#[test]
fn token_accepts_underscores() {
    assert!(is_token("mcp__playwright__click"));
    assert!(is_token("ai103_labs"));
}

#[test]
fn token_rejects_paths_spaces_newlines() {
    for s in ["/Users/x/skill", "a b", "a\nb", "", "_a"] {
        assert!(!is_token(s), "{s:?}");
    }
    assert!(!is_token(&"a".repeat(129)));
    assert!(is_token(&"a".repeat(128)));
}

#[test]
fn repo_accepts_host_group_name() {
    assert!(is_repo("gitlab.com/supermodularai/core/agents-wake"));
}

#[test]
fn repo_rejects() {
    for s in ["agents-wake", "/Users/x/repo", "gitlab.com/../x", "a//b"] {
        assert!(!is_repo(s), "{s:?}");
    }
    assert!(!is_repo("a/b/c/d/e/f/g/h/i"));
}

#[test]
fn email_rules() {
    assert!(is_email("jose.morais@supermodular.ai"));
    assert!(!is_email("no-at"));
    assert!(!is_email("a@b@c"));
    assert!(!is_email("@b.c"));
    assert!(!is_email("a@"));
    assert!(!is_email("a b@c.d"));
}

#[test]
fn sanitized_drops_invalid_name() {
    assert!(inv("rm -rf /").sanitized().is_none());
    assert!(inv("ok").sanitized().is_some());
}

#[test]
fn sanitized_drops_bad_optionals() {
    let mut i = inv("ok");
    i.version = Some("1.0 beta".into());
    i.plugin = Some("good".into());
    i.tools = vec![("Bash".into(), 3), ("../x".into(), 1)];
    let s = i.sanitized().unwrap();
    assert_eq!(s.version, None);
    assert_eq!(s.plugin.as_deref(), Some("good"));
    assert_eq!(s.tools, vec![("Bash".to_string(), 3)]);
}

#[test]
fn sanitized_filters_inventory_entries() {
    let mut i = inv("inventory");
    i.kind = Kind::Inventory;
    i.inventory = vec!["a:b".into(), "bad name".into(), "c".into()];
    let s = i.sanitized().unwrap();
    assert_eq!(s.inventory, vec!["a:b".to_string(), "c".to_string()]);
}

#[test]
fn sanitized_drops_bad_mcp_server() {
    let mut i = inv("mcp__a__b");
    i.mcp = Some(McpAgg {
        server: "a b".into(),
        calls: 1,
        errors: 0,
        total_duration_ms: 1,
        max_duration_ms: 1,
    });
    assert!(i.sanitized().unwrap().mcp.is_none());
}

#[test]
fn context_sanitized() {
    let c = Context {
        harness: "claude-code",
        harness_version: Some("1 2".into()),
        session_id: "s".into(),
        user: UserId::Email("bad email".into()),
        repo: Some("/local/path".into()),
    };
    let c = c.sanitized();
    assert_eq!(c.harness_version, None);
    assert_eq!(c.repo, None);
    assert_eq!(c.user, UserId::Unknown);
    let hex = "a".repeat(64);
    let ok = Context {
        harness: "claude-code",
        harness_version: Some("2.1.0".into()),
        session_id: "s".into(),
        user: UserId::Hash(hex.clone()),
        repo: Some("gitlab.com/a/b".into()),
    }
    .sanitized();
    assert_eq!(ok.user, UserId::Hash(hex));
    assert!(ok.repo.is_some() && ok.harness_version.is_some());
    let bad = Context {
        harness: "claude-code",
        harness_version: None,
        session_id: "s".into(),
        user: UserId::Hash("ABC".into()),
        repo: None,
    }
    .sanitized();
    assert_eq!(bad.user, UserId::Unknown);
}

#[test]
fn tokens_add() {
    let mut a = Tokens::default();
    a.add(&Tokens {
        input: Some(5),
        ..Default::default()
    });
    assert_eq!(a.input, Some(5));
    a.add(&Tokens {
        input: Some(3),
        ..Default::default()
    });
    assert_eq!(a.input, Some(8));
    assert_eq!(a.output, None);
}

#[test]
fn as_str_values() {
    assert_eq!(Kind::McpTool.as_str(), "mcp_tool");
    assert_eq!(Kind::Inventory.as_str(), "inventory");
    assert_eq!(Invoker::Nested.as_str(), "nested");
    assert_eq!(Outcome::Interrupted.as_str(), "interrupted");
}
