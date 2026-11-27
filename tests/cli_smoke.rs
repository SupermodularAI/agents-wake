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

/// Runs wake with HOME pointed at a temp dir and no XDG overrides, so nothing real is touched.
fn run_in_home(args: &[&str], home: &std::path::Path) -> std::process::Output {
    Command::new(env!("CARGO_BIN_EXE_wake"))
        .args(args)
        .env("HOME", home)
        .env_remove("XDG_CONFIG_HOME")
        .env_remove("XDG_STATE_HOME")
        .stdin(Stdio::null())
        .output()
        .unwrap()
}

#[test]
fn install_bad_args_exit_2_and_touch_nothing() {
    let dir = tempfile::tempdir().unwrap();
    for args in [
        &["install", "--harness", "codex"][..],
        &["install", "--bogus"],
        &["install", "--endpoint"],
        &["install", "--header", "novalue"],
        &["install", "--identity", "someone"],
        &["uninstall", "--harness", "codex"],
        &["uninstall", "--nope"],
    ] {
        let out = run_in_home(args, dir.path());
        assert_eq!(out.status.code(), Some(2), "{args:?}");
        assert!(!out.stderr.is_empty(), "{args:?}");
    }
    assert_eq!(std::fs::read_dir(dir.path()).unwrap().count(), 0);
}

#[test]
fn install_then_uninstall_via_cli() {
    let dir = tempfile::tempdir().unwrap();
    let out = run_in_home(
        &[
            "install",
            "--endpoint",
            "https://lf.example.com/x",
            "--header",
            "Authorization=Basic c2VjcmV0",
            "--identity",
            "anonymous",
        ],
        dir.path(),
    );
    assert_eq!(out.status.code(), Some(0), "{out:?}");
    let stdout = String::from_utf8(out.stdout).unwrap();
    assert!(!stdout.contains("c2VjcmV0"), "{stdout}");
    let settings = std::fs::read_to_string(dir.path().join(".claude/settings.json")).unwrap();
    assert!(settings.contains("emit session-start"), "{settings}");

    let out = run_in_home(&["uninstall", "--purge"], dir.path());
    assert_eq!(out.status.code(), Some(0), "{out:?}");
    assert_eq!(
        std::fs::read_to_string(dir.path().join(".claude/settings.json")).unwrap(),
        "{}\n"
    );
    assert!(!dir.path().join(".config/wake").exists());
}

#[test]
fn version_prints_crate_version() {
    for flag in ["--version", "-V"] {
        let out = run(&[flag], "");
        assert_eq!(out.status.code(), Some(0), "{flag}");
        assert_eq!(
            String::from_utf8(out.stdout).unwrap(),
            format!("wake {}\n", env!("CARGO_PKG_VERSION")),
            "{flag}"
        );
        assert!(out.stderr.is_empty(), "{flag}");
    }
}
