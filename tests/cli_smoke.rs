use std::io::Write;
use std::process::{Command, Stdio};

fn run(args: &[&str], stdin: &str) -> std::process::Output {
    let mut child = Command::new(env!("CARGO_BIN_EXE_wake"))
        .args(args)
        .stdin(Stdio::piped())
        .stdout(Stdio::piped())
        .stderr(Stdio::piped())
        .spawn()
        .expect("spawn wake");
    child
        .stdin
        .take()
        .unwrap()
        .write_all(stdin.as_bytes())
        .unwrap();
    child.wait_with_output().unwrap()
}

#[test]
fn emit_unknown_hook_exits_zero_silently() {
    let out = run(&["emit", "nonsense"], "{}");
    assert_eq!(out.status.code(), Some(0));
    assert!(out.stdout.is_empty());
    assert!(out.stderr.is_empty());
}

#[test]
fn emit_garbage_stdin_exits_zero() {
    let out = run(&["emit", "nonsense"], "not json");
    assert_eq!(out.status.code(), Some(0));
    assert!(out.stdout.is_empty());
    assert!(out.stderr.is_empty());
}

#[test]
fn unknown_subcommand_exits_2() {
    let out = run(&["frobnicate"], "");
    assert_eq!(out.status.code(), Some(2));
}
