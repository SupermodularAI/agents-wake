//! Incremental reader for Claude Code JSONL transcripts.
//!
//! Only allowlisted fields are deserialised. Message text, prompts included,
//! is read transiently while its line is parsed (to recognise slash commands,
//! task notifications and interruptions); it is never retained in an `Entry`
//! nor sent. Tool inputs and outputs are never retained either.

use crate::event::Tokens;
use serde::Deserialize;
use std::fs::File;
use std::io::{BufRead, BufReader, Seek, SeekFrom};
use std::path::Path;

#[derive(Debug, Clone, PartialEq)]
pub enum Block {
    ToolUse {
        id: String,
        name: String,
        skill: Option<String>,
        subagent_type: Option<String>,
    },
    ToolResult {
        tool_use_id: String,
        is_error: bool,
    },
}

#[derive(Debug, Clone, PartialEq)]
pub struct AgentResult {
    pub agent_id: String,
    pub agent_type: Option<String>,
    pub status: Option<String>,
    pub is_async: bool,
    pub total_duration_ms: Option<u64>,
    pub total_tool_use_count: Option<u64>,
    pub usage: Tokens,
}

#[derive(Debug, Clone, PartialEq)]
pub struct TaskNotification {
    pub tool_use_id: String,
    pub status: Option<String>,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Role {
    User,
    Assistant,
    Other,
}

#[derive(Debug, Clone, PartialEq)]
pub struct Entry {
    pub role: Role,
    pub ts_ns: u64,
    pub request_id: Option<String>,
    pub model: Option<String>,
    pub usage: Option<Tokens>,
    pub version: Option<String>,
    pub is_sidechain: bool,
    pub is_meta: bool,
    pub attribution_skill: Option<String>,
    pub attribution_agent: Option<String>,
    pub attribution_mcp_server: Option<String>,
    pub attribution_plugin: Option<String>,
    pub blocks: Vec<Block>,
    pub command_name: Option<String>,
    pub interrupted: bool,
    pub agent_result: Option<AgentResult>,
    pub task_notification: Option<TaskNotification>,
}

// ---- raw (allowlisted) serde shapes ----

#[derive(Deserialize, Default)]
#[serde(default)]
struct RawUsage {
    input_tokens: Option<u64>,
    output_tokens: Option<u64>,
    cache_read_input_tokens: Option<u64>,
    cache_creation_input_tokens: Option<u64>,
}

impl From<RawUsage> for Tokens {
    fn from(u: RawUsage) -> Tokens {
        Tokens {
            input: u.input_tokens,
            output: u.output_tokens,
            cache_read: u.cache_read_input_tokens,
            cache_creation: u.cache_creation_input_tokens,
        }
    }
}

#[derive(Deserialize, Default)]
#[serde(default)]
struct RawInput {
    skill: Option<String>,
    subagent_type: Option<String>,
}

#[derive(Deserialize, Default)]
#[serde(default)]
struct RawBlock {
    #[serde(rename = "type")]
    kind: Option<String>,
    id: Option<String>,
    name: Option<String>,
    input: Option<RawInput>,
    tool_use_id: Option<String>,
    is_error: Option<bool>,
    text: Option<String>,
}

#[derive(Deserialize)]
#[serde(untagged)]
enum RawContent {
    Text(String),
    Blocks(Vec<RawBlock>),
}

#[derive(Deserialize, Default)]
#[serde(default)]
struct RawMessage {
    model: Option<String>,
    usage: Option<RawUsage>,
    content: Option<RawContent>,
}

#[derive(Deserialize)]
#[serde(rename_all = "camelCase")]
struct RawAgent {
    agent_id: String,
    #[serde(default)]
    agent_type: Option<String>,
    #[serde(default)]
    status: Option<String>,
    #[serde(default)]
    is_async: bool,
    #[serde(default)]
    total_duration_ms: Option<u64>,
    #[serde(default)]
    total_tool_use_count: Option<u64>,
    #[serde(default)]
    usage: Option<RawUsage>,
}

#[derive(Deserialize)]
#[serde(untagged)]
enum RawToolUseResult {
    AgentObj(RawAgent),
    #[allow(dead_code)]
    Other(serde::de::IgnoredAny),
}

#[derive(Deserialize, Default)]
#[serde(default, rename_all = "camelCase")]
struct RawEntry {
    #[serde(rename = "type")]
    kind: Option<String>,
    timestamp: Option<String>,
    request_id: Option<String>,
    version: Option<String>,
    is_sidechain: bool,
    is_meta: bool,
    attribution_skill: Option<String>,
    attribution_agent: Option<String>,
    attribution_mcp_server: Option<String>,
    attribution_plugin: Option<String>,
    message: Option<RawMessage>,
    tool_use_result: Option<RawToolUseResult>,
}

// ---- conversion ----

fn first_text(content: &Option<RawContent>) -> Option<&str> {
    match content {
        Some(RawContent::Text(s)) => Some(s),
        Some(RawContent::Blocks(bs)) => bs
            .iter()
            .find(|b| b.kind.as_deref() == Some("text"))
            .and_then(|b| b.text.as_deref()),
        None => None,
    }
}

fn all_text_contains(content: &Option<RawContent>, needle: &str) -> bool {
    match content {
        Some(RawContent::Text(s)) => s.contains(needle),
        Some(RawContent::Blocks(bs)) => bs
            .iter()
            .any(|b| b.text.as_deref().is_some_and(|t| t.contains(needle))),
        None => false,
    }
}

fn tag<'a>(s: &'a str, name: &str) -> Option<&'a str> {
    let open = format!("<{name}>");
    let close = format!("</{name}>");
    let start = s.find(&open)? + open.len();
    let end = start + s[start..].find(&close)?;
    Some(s[start..end].trim())
}

fn parse_command_name(text: &str) -> Option<String> {
    let t = text.trim_start();
    if !(t.starts_with("<command-name>") || t.starts_with("<command-message>")) {
        return None;
    }
    let name = tag(t, "command-name")?;
    let name = name.strip_prefix('/').unwrap_or(name);
    Some(name.to_string())
}

fn parse_task_notification(text: &str) -> Option<TaskNotification> {
    let t = text.trim_start();
    if !t.starts_with("<task-notification>") {
        return None;
    }
    Some(TaskNotification {
        tool_use_id: tag(t, "tool-use-id")?.to_string(),
        status: tag(t, "status").map(str::to_string),
    })
}

fn convert(raw: RawEntry) -> Entry {
    let role = match raw.kind.as_deref() {
        Some("user") => Role::User,
        Some("assistant") => Role::Assistant,
        _ => Role::Other,
    };
    let msg = raw.message.unwrap_or_default();
    let mut blocks = Vec::new();
    if let Some(RawContent::Blocks(bs)) = &msg.content {
        for b in bs {
            match b.kind.as_deref() {
                Some("tool_use") => {
                    let inp = b.input.as_ref();
                    blocks.push(Block::ToolUse {
                        id: b.id.clone().unwrap_or_default(),
                        name: b.name.clone().unwrap_or_default(),
                        skill: inp.and_then(|i| i.skill.clone()),
                        subagent_type: inp.and_then(|i| i.subagent_type.clone()),
                    });
                }
                Some("tool_result") => blocks.push(Block::ToolResult {
                    tool_use_id: b.tool_use_id.clone().unwrap_or_default(),
                    is_error: b.is_error.unwrap_or(false),
                }),
                _ => {}
            }
        }
    }
    let is_user = role == Role::User;
    let text = if is_user {
        first_text(&msg.content)
    } else {
        None
    };
    let command_name = match text {
        Some(t) if !raw.is_meta => parse_command_name(t),
        _ => None,
    };
    let task_notification = text.and_then(parse_task_notification);
    let interrupted = is_user && all_text_contains(&msg.content, "[Request interrupted by user");
    let agent_result = match raw.tool_use_result {
        Some(RawToolUseResult::AgentObj(a)) => {
            let is_async = a.is_async || a.status.as_deref() == Some("async_launched");
            Some(AgentResult {
                agent_id: a.agent_id,
                agent_type: a.agent_type,
                status: a.status,
                is_async,
                total_duration_ms: a.total_duration_ms,
                total_tool_use_count: a.total_tool_use_count,
                usage: a.usage.map(Tokens::from).unwrap_or_default(),
            })
        }
        _ => None,
    };
    Entry {
        role,
        ts_ns: raw.timestamp.as_deref().map(parse_rfc3339_ns).unwrap_or(0),
        request_id: raw.request_id,
        model: msg.model,
        usage: msg.usage.map(Tokens::from),
        version: raw.version,
        is_sidechain: raw.is_sidechain,
        is_meta: raw.is_meta,
        attribution_skill: raw.attribution_skill,
        attribution_agent: raw.attribution_agent,
        attribution_mcp_server: raw.attribution_mcp_server,
        attribution_plugin: raw.attribution_plugin,
        blocks,
        command_name,
        interrupted,
        agent_result,
        task_notification,
    }
}

/// RFC 3339 (`YYYY-MM-DDTHH:MM:SS[.fraction](Z|±HH:MM)`) to ns since the epoch; 0 if unparsable.
fn parse_rfc3339_ns(s: &str) -> u64 {
    parse_rfc3339(s).unwrap_or(0)
}

fn parse_rfc3339(s: &str) -> Option<u64> {
    let b = s.as_bytes();
    if b.len() < 20 || !s.is_ascii() {
        return None;
    }
    let num = |r: std::ops::Range<usize>| -> Option<i64> { s.get(r)?.parse().ok() };
    if b[4] != b'-'
        || b[7] != b'-'
        || !(b[10] == b'T' || b[10] == b't')
        || b[13] != b':'
        || b[16] != b':'
    {
        return None;
    }
    let (y, mo, d) = (num(0..4)?, num(5..7)?, num(8..10)?);
    let (h, mi, sec) = (num(11..13)?, num(14..16)?, num(17..19)?);
    if !(1..=12).contains(&mo) || !(1..=31).contains(&d) || h > 23 || mi > 59 || sec > 60 {
        return None;
    }
    let mut i = 19;
    let mut nanos: i64 = 0;
    if b[i] == b'.' {
        i += 1;
        let start = i;
        let mut scale = 100_000_000;
        while i < b.len() && b[i].is_ascii_digit() {
            nanos += (b[i] - b'0') as i64 * scale;
            scale /= 10;
            i += 1;
        }
        if i == start {
            return None;
        }
    }
    let offset_s: i64 = match b.get(i)? {
        b'Z' | b'z' if i + 1 == b.len() => 0,
        sign @ (b'+' | b'-') if i + 6 == b.len() && b[i + 3] == b':' => {
            let secs = num(i + 1..i + 3)? * 3600 + num(i + 4..i + 6)? * 60;
            if *sign == b'+' {
                secs
            } else {
                -secs
            }
        }
        _ => return None,
    };
    // days from civil (Howard Hinnant)
    let y = if mo <= 2 { y - 1 } else { y };
    let era = y.div_euclid(400);
    let yoe = y - era * 400;
    let mp = (mo + 9) % 12;
    let doy = (153 * mp + 2) / 5 + d - 1;
    let doe = yoe * 365 + yoe / 4 - yoe / 100 + doy;
    let days = era * 146_097 + doe - 719_468;
    let secs = days * 86_400 + h * 3600 + mi * 60 + sec - offset_s;
    u64::try_from(secs)
        .ok()?
        .checked_mul(1_000_000_000)?
        .checked_add(nanos as u64)
}

/// Read complete lines from `offset`. Returns the entries and the offset just
/// after the last complete `\n`. A final chunk without `\n` is not consumed.
/// Lines that fail to parse are skipped (their bytes still count).
pub fn read_from(path: &Path, offset: u64) -> std::io::Result<(Vec<Entry>, u64)> {
    let mut file = File::open(path)?;
    let len = file.metadata()?.len();
    if offset > len {
        return Ok((Vec::new(), len));
    }
    file.seek(SeekFrom::Start(offset))?;
    let mut reader = BufReader::new(file);
    let mut entries = Vec::new();
    let mut pos = offset;
    let mut buf = Vec::new();
    loop {
        buf.clear();
        let n = reader.read_until(b'\n', &mut buf)?;
        if n == 0 || buf.last() != Some(&b'\n') {
            break;
        }
        pos += n as u64;
        if let Ok(raw) = serde_json::from_slice::<RawEntry>(&buf) {
            entries.push(convert(raw));
        }
    }
    Ok((entries, pos))
}
