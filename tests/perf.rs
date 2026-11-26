//! `wake emit stop` budget on a 20 MB synthetic transcript.
//!
//! Run with `cargo test --release --test perf -- --ignored`.

use std::fs;
use std::io::Write;
use std::path::Path;
use std::process::{Command, Stdio};
use wake::state::{self, SessionState};

const SESSION: &str = "perf-sess";
const TURNS: usize = 200;
/// Tool call pairs per ordinary turn (~1 KB each) and in the last turn.
const PAIRS: usize = 95;
const LAST_PAIRS: usize = 1000;

fn ts(secs: u64) -> String {
    format!(
        "2026-11-26T{:02}:{:02}:{:02}.000Z",
        secs / 3600,
        (secs / 60) % 60,
        secs % 60
    )
}

/// Writes the transcript and returns (file length, offset of the last turn, entry count).
fn generate(path: &Path) -> (u64, u64, usize) {
    let pad = "x".repeat(330);
    let mut out = Vec::with_capacity(21 << 20);
    let mut secs = 0u64;
    let mut entries = 0usize;
    let mut last_turn_offset = 0u64;
    let mut line = |out: &mut Vec<u8>, kind: &str, extra: String, secs: &mut u64| {
        *secs += 1;
        entries += 1;
        writeln!(
            out,
            r#"{{"parentUuid":null,"isSidechain":false,"userType":"external","cwd":"/work/demo","sessionId":"{SESSION}","version":"2.1.0","type":"{kind}","timestamp":"{}",{extra}}}"#,
            ts(*secs)
        )
        .unwrap();
    };
    for turn in 0..TURNS {
        if turn == TURNS - 1 {
            last_turn_offset = out.len() as u64;
        }
        let skill = format!("skill-{}", turn % 10);
        line(
            &mut out,
            "user",
            r#""message":{"role":"user","content":"next step please"}"#.to_string(),
            &mut secs,
        );
        line(
            &mut out,
            "assistant",
            format!(
                r#""requestId":"req_{turn}_s","message":{{"role":"assistant","model":"claude-opus-5","content":[{{"type":"tool_use","id":"toolu_{turn}_s","name":"Skill","input":{{"skill":"{skill}"}}}}],"usage":{{"input_tokens":10,"output_tokens":5,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}}}"#
            ),
            &mut secs,
        );
        line(
            &mut out,
            "user",
            format!(
                r#""message":{{"role":"user","content":[{{"type":"tool_result","tool_use_id":"toolu_{turn}_s","content":"{pad}"}}]}}"#
            ),
            &mut secs,
        );
        let pairs = if turn == TURNS - 1 { LAST_PAIRS } else { PAIRS };
        for i in 0..pairs {
            line(
                &mut out,
                "assistant",
                format!(
                    r#""requestId":"req_{turn}_{i}","attributionSkill":"{skill}","message":{{"role":"assistant","model":"claude-opus-5","content":[{{"type":"tool_use","id":"toolu_{turn}_{i}","name":"Bash","input":{{"command":"ls"}}}}],"usage":{{"input_tokens":100,"output_tokens":10,"cache_read_input_tokens":50,"cache_creation_input_tokens":0}}}}"#
                ),
                &mut secs,
            );
            line(
                &mut out,
                "user",
                format!(
                    r#""message":{{"role":"user","content":[{{"type":"tool_result","tool_use_id":"toolu_{turn}_{i}","content":"{pad}"}}]}}"#
                ),
                &mut secs,
            );
        }
    }
    fs::write(path, &out).unwrap();
    (out.len() as u64, last_turn_offset, entries)
}

struct Usage {
    cpu_ms: f64,
    max_rss_bytes: u64,
}

fn children_usage() -> Usage {
    let mut ru: libc::rusage = unsafe { std::mem::zeroed() };
    unsafe { libc::getrusage(libc::RUSAGE_CHILDREN, &mut ru) };
    let tv = |t: libc::timeval| t.tv_sec as f64 * 1000.0 + t.tv_usec as f64 / 1000.0;
    // ru_maxrss is bytes on macOS and kilobytes on Linux.
    let rss = if cfg!(target_os = "macos") {
        ru.ru_maxrss as u64
    } else {
        ru.ru_maxrss as u64 * 1024
    };
    Usage {
        cpu_ms: tv(ru.ru_utime) + tv(ru.ru_stime),
        max_rss_bytes: rss,
    }
}

fn stop(bin: &Path, home: &Path, payload: &[u8]) {
    let mut child = Command::new(bin)
        .args(["emit", "stop"])
        .env_clear()
        .env("HOME", home)
        .stdin(Stdio::piped())
        .stdout(Stdio::null())
        .stderr(Stdio::null())
        .spawn()
        .unwrap();
    child.stdin.take().unwrap().write_all(payload).unwrap();
    assert!(child.wait().unwrap().success());
}

fn state_at(offset: u64, cwd: &Path) -> SessionState {
    SessionState {
        offset,
        cwd: Some(cwd.to_string_lossy().into_owned()),
        user: Some("dev@example.com".to_string()),
        repo: Some("github.com/acme/demo".to_string()),
        ..SessionState::default()
    }
}

#[test]
#[ignore]
fn stop_budget_on_20mb_transcript() {
    let d = tempfile::tempdir().unwrap();
    let (home, bin_dir, cwd) = (
        d.path().join("home"),
        d.path().join("bin"),
        d.path().join("work"),
    );
    for p in [&home, &bin_dir, &cwd] {
        fs::create_dir_all(p).unwrap();
    }
    let bin = bin_dir.join("wake");
    fs::copy(env!("CARGO_BIN_EXE_wake"), &bin).unwrap();
    fs::write(
        bin_dir.join("telemetry.json"),
        r#"{"endpoint":"http://127.0.0.1:9/"}"#,
    )
    .unwrap();

    let transcript = d.path().join(format!("{SESSION}.jsonl"));
    let (len, last_turn, entries) = generate(&transcript);
    eprintln!(
        "transcript: {:.1} MB, {entries} entries, last turn {:.2} MB",
        len as f64 / 1e6,
        (len - last_turn) as f64 / 1e6
    );
    assert!(len > 19_000_000, "transcript is {len} bytes");

    let payload = serde_json::to_vec(&serde_json::json!({
        "session_id": SESSION,
        "transcript_path": transcript,
        "cwd": cwd,
    }))
    .unwrap();
    let dir = home.join(".local/state/wake");

    // One-turn slice.
    state::save(&dir, SESSION, &state_at(last_turn, &cwd)).unwrap();
    let before = children_usage();
    stop(&bin, &home, &payload);
    let after = children_usage();
    let one_turn_cpu = after.cpu_ms - before.cpu_ms;
    let one_turn_rss = after.max_rss_bytes;
    assert_eq!(state::load(&dir, SESSION).unwrap().offset, len);

    // Worst case: the whole transcript.
    state::save(&dir, SESSION, &state_at(0, &cwd)).unwrap();
    let before = children_usage();
    stop(&bin, &home, &payload);
    let after = children_usage();
    let full_cpu = after.cpu_ms - before.cpu_ms;
    let full_rss = after.max_rss_bytes;
    assert_eq!(state::load(&dir, SESSION).unwrap().offset, len);

    eprintln!(
        "one turn: {one_turn_cpu:.1} ms CPU, {:.2} MB max RSS",
        one_turn_rss as f64 / 1e6
    );
    eprintln!(
        "offset 0: {full_cpu:.1} ms CPU, {:.2} MB max RSS (max over both runs)",
        full_rss as f64 / 1e6
    );
    assert!(one_turn_cpu < 50.0, "one-turn CPU {one_turn_cpu:.1} ms");
    assert!(
        one_turn_rss < 10 * 1024 * 1024,
        "one-turn max RSS {one_turn_rss} bytes"
    );
    assert!(full_cpu < 500.0, "offset-0 CPU {full_cpu:.1} ms");
}
