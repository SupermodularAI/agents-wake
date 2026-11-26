use std::io::Read;
use std::path::PathBuf;
use std::process::ExitCode;
use std::time::SystemTime;
use wake::config::ProcessEnv;
use wake::harness::claude_code::{run_hook, Deps};
use wake::send::UreqTransport;

const USAGE: &str = "usage: wake <emit <hook> | check | install | uninstall>";

/// `wake emit <hook>`: read the payload, then handle it. Never prints, never fails.
fn emit(hook: &str) {
    let mut stdin = Vec::new();
    let _ = std::io::stdin().read_to_end(&mut stdin);
    let Some(home) = std::env::var_os("HOME").filter(|h| !h.is_empty()) else {
        return;
    };
    let Some(exe_dir) = std::env::current_exe()
        .ok()
        .and_then(|p| p.parent().map(PathBuf::from))
    else {
        return;
    };
    let deps = Deps {
        env: &ProcessEnv,
        home: PathBuf::from(home),
        exe_dir,
        transport: &UreqTransport,
        now: SystemTime::now(),
    };
    run_hook(hook, &stdin, &deps);
}

/// `wake check`: print the resolved config and send a test event; returns the exit code.
fn check() -> u8 {
    let (Some(home), Ok(exe)) = (
        std::env::var_os("HOME").filter(|h| !h.is_empty()),
        std::env::current_exe(),
    ) else {
        eprintln!("wake check: cannot resolve HOME or the executable path");
        return 1;
    };
    let deps = Deps {
        env: &ProcessEnv,
        home: PathBuf::from(home),
        exe_dir: exe.parent().map(PathBuf::from).unwrap_or_default(),
        transport: &UreqTransport,
        now: SystemTime::now(),
    };
    let code = wake::check::run(&deps, &mut std::io::stdout());
    u8::try_from(code).unwrap_or(1)
}

fn main() -> ExitCode {
    let args: Vec<String> = std::env::args().collect();
    match args.get(1).map(String::as_str) {
        Some("emit") => {
            // emit must never fail, print, or panic outward.
            let hook = args.get(2).map(String::as_str).unwrap_or("");
            wake::run_silently(|| emit(hook));
            ExitCode::SUCCESS
        }
        Some("check") => ExitCode::from(check()),
        // Stub until the install task lands.
        Some("install") | Some("uninstall") => ExitCode::SUCCESS,
        _ => {
            eprintln!("{USAGE}");
            ExitCode::from(2)
        }
    }
}
