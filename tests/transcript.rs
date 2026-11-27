use std::io::Write;
use std::path::PathBuf;
use wake::harness::claude_code::transcript::{read_from, Block, Role};

fn fixture() -> PathBuf {
    PathBuf::from(env!("CARGO_MANIFEST_DIR")).join("tests/fixtures/claude_code/basic.jsonl")
}

fn copy_fixture() -> (tempfile::TempDir, PathBuf) {
    let dir = tempfile::tempdir().unwrap();
    let p = dir.path().join("t.jsonl");
    std::fs::copy(fixture(), &p).unwrap();
    (dir, p)
}

fn append(p: &PathBuf, s: &str) {
    let mut f = std::fs::OpenOptions::new().append(true).open(p).unwrap();
    f.write_all(s.as_bytes()).unwrap();
}

#[test]
fn parses_fields() {
    let (entries, off) = read_from(&fixture(), 0).unwrap();
    assert_eq!(entries.len(), 11);
    assert_eq!(off, std::fs::metadata(fixture()).unwrap().len());
    assert_eq!(entries[0].role, Role::User);
    assert_eq!(entries[0].ts_ns, 1_795_688_130_123_000_000);
    match &entries[1].blocks[0] {
        Block::ToolUse {
            id,
            name,
            skill,
            subagent_type,
        } => {
            assert_eq!(id, "toolu_skill");
            assert_eq!(name, "Skill");
            assert_eq!(skill.as_deref(), Some("brainstorming"));
            assert!(subagent_type.is_none());
        }
        b => panic!("unexpected {b:?}"),
    }
    assert_eq!(entries[1].usage.as_ref().unwrap().cache_read, Some(100));
    assert_eq!(entries[1].model.as_deref(), Some("claude-opus-5"));
    assert_eq!(entries[1].version.as_deref(), Some("2.1.0"));
    assert_eq!(
        entries
            .iter()
            .filter(|e| e.request_id.as_deref() == Some("req_2"))
            .count(),
        2
    );
    assert_eq!(
        entries[3].attribution_skill.as_deref(),
        Some("brainstorming")
    );
    match &entries[5].blocks[0] {
        Block::ToolResult {
            tool_use_id,
            is_error,
        } => {
            assert_eq!(tool_use_id, "toolu_bash");
            assert!(*is_error);
        }
        b => panic!("unexpected {b:?}"),
    }
    assert_eq!(entries[6].command_name.as_deref(), Some("review"));
    assert!(entries[7].is_meta);
    assert!(entries[10].interrupted);
}

#[test]
fn command_name_only_at_start() {
    let (entries, _) = read_from(&fixture(), 0).unwrap();
    assert_eq!(entries[7].command_name, None);
    assert_eq!(entries[8].command_name, None);
}

#[test]
fn task_notification_parsed() {
    let (entries, _) = read_from(&fixture(), 0).unwrap();
    let tn = entries[9].task_notification.as_ref().unwrap();
    assert_eq!(tn.tool_use_id, "toolu_x");
    assert_eq!(tn.status.as_deref(), Some("completed"));
    assert!(!format!("{:?}", entries).contains("SECRET-SUMMARY"));
}

#[test]
fn partial_last_line_not_consumed() {
    let (_d, p) = copy_fixture();
    let len = std::fs::metadata(&p).unwrap().len();
    append(&p, "{\"type\":\"assistant\"");
    let (entries, off) = read_from(&p, 0).unwrap();
    assert_eq!(off, len);
    assert_eq!(entries.len(), 11);
}

#[test]
fn resume_from_offset_reads_only_new() {
    let (_d, p) = copy_fixture();
    let (_, x) = read_from(&p, 0).unwrap();
    append(
        &p,
        "{\"type\":\"user\",\"message\":{\"content\":\"a\"}}\nnot json\n{\"type\":\"assistant\"}\n",
    );
    let (entries, off) = read_from(&p, x).unwrap();
    assert_eq!(entries.len(), 2);
    assert_eq!(off, std::fs::metadata(&p).unwrap().len());
}

#[test]
fn offset_beyond_length_returns_empty() {
    let (_d, p) = copy_fixture();
    let len = std::fs::metadata(&p).unwrap().len();
    let (entries, off) = read_from(&p, len + 100).unwrap();
    assert!(entries.is_empty());
    assert_eq!(off, len);
}

#[test]
fn missing_file_is_err() {
    assert!(read_from(&PathBuf::from("/nonexistent/x.jsonl"), 0).is_err());
}

#[test]
fn unknown_content_not_retained() {
    let (_d, p) = copy_fixture();
    let (_, x) = read_from(&p, 0).unwrap();
    append(&p, "{\"type\":\"user\",\"message\":{\"content\":\"SECRET-PROMPT\"},\"extra\":\"SECRET-PROMPT\"}\n{\"type\":\"assistant\",\"message\":{\"content\":[{\"type\":\"text\",\"text\":\"SECRET-PROMPT\"},{\"type\":\"tool_use\",\"id\":\"t\",\"name\":\"Bash\",\"input\":{\"command\":\"SECRET-PROMPT\"}}]},\"toolUseResult\":\"SECRET-PROMPT\"}\n");
    let (entries, _) = read_from(&p, x).unwrap();
    assert_eq!(entries.len(), 2);
    assert!(!format!("{:?}", entries).contains("SECRET-PROMPT"));
}

#[test]
fn agent_results_parsed() {
    let (_d, p) = copy_fixture();
    let (_, x) = read_from(&p, 0).unwrap();
    append(&p, concat!(
        "{\"type\":\"user\",\"message\":{\"content\":[]},\"toolUseResult\":{\"agentId\":\"a1\",\"agentType\":\"Explore\",\"status\":\"completed\",\"totalDurationMs\":5000,\"totalToolUseCount\":7,\"usage\":{\"input_tokens\":1,\"output_tokens\":2},\"content\":[{\"type\":\"text\",\"text\":\"SECRET-PROMPT\"}]}}\n",
        "{\"type\":\"user\",\"message\":{\"content\":[]},\"toolUseResult\":{\"agentId\":\"a2\",\"isAsync\":true,\"status\":\"async_launched\",\"prompt\":\"SECRET-PROMPT\"}}\n",
        "{\"type\":\"user\",\"message\":{\"content\":[]},\"toolUseResult\":{\"stdout\":\"x\"}}\n"));
    let (e, _) = read_from(&p, x).unwrap();
    let a = e[0].agent_result.as_ref().unwrap();
    assert_eq!(a.agent_id, "a1");
    assert_eq!(a.agent_type.as_deref(), Some("Explore"));
    assert_eq!(a.total_duration_ms, Some(5000));
    assert_eq!(a.total_tool_use_count, Some(7));
    assert_eq!(a.usage.input, Some(1));
    assert!(!a.is_async);
    let b = e[1].agent_result.as_ref().unwrap();
    assert!(b.is_async);
    assert_eq!(b.usage.input, None);
    assert!(e[2].agent_result.is_none());
    assert!(!format!("{:?}", e).contains("SECRET-PROMPT"));
}

#[test]
fn command_name_after_command_message() {
    let p = PathBuf::from(env!("CARGO_MANIFEST_DIR"))
        .join("tests/fixtures/claude_code/skills_slash_command_message.jsonl");
    let (entries, _) = read_from(&p, 0).unwrap();
    assert_eq!(entries[0].command_name.as_deref(), Some("brainstorming"));
    assert_eq!(entries[1].command_name, None);
}
